// Package agent runs on the Mac an iPhone is physically plugged into. It
// exposes that machine's usbmuxd and the device's CoreDevice front door to
// authenticated Crossmap hosts over the tailnet.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/wtfchewy/crossmap/internal/devices"
	"github.com/wtfchewy/crossmap/internal/link"
	"github.com/wtfchewy/crossmap/internal/usbmux"
	"github.com/wtfchewy/crossmap/internal/wire"
)

// Config parameterises an Agent.
type Config struct {
	// Listen is the address to serve Crossmap links on. It should be bound
	// to the tailnet interface rather than 0.0.0.0 so the agent is not
	// exposed on whatever untrusted network the machine is also on.
	Listen string
	// PSK authenticates hosts.
	PSK []byte
	// UsbmuxdSocket overrides the usbmuxd path (defaults to Apple's).
	UsbmuxdSocket string
	// Version is reported to hosts in Describe.
	Version string
	// Discovery supplies CoreDevice RemotePairing services; may be nil to
	// run the usbmuxd plane only.
	Discovery Discovery
	Logger    *slog.Logger
}

// Discovery finds the RemotePairing advertisements of locally attached
// devices. It is an interface so the mDNS implementation can be swapped or
// stubbed in tests.
type Discovery interface {
	Services() []wire.Service
}

// Agent serves Crossmap links.
type Agent struct {
	cfg    Config
	log    *slog.Logger
	mux    *usbmux.Client
	reg    *devices.Set
	info   wire.AgentInfo
	ln     net.Listener
	closed chan struct{}
	once   sync.Once
}

// New builds an Agent from cfg.
func New(cfg Config) (*Agent, error) {
	if len(cfg.PSK) == 0 {
		return nil, errors.New("agent: a pre-shared key is required")
	}
	if cfg.Listen == "" {
		return nil, errors.New("agent: a listen address is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	host, _ := os.Hostname()
	return &Agent{
		cfg: cfg,
		log: cfg.Logger,
		mux: usbmux.NewClient(cfg.UsbmuxdSocket),
		reg: devices.New(),
		info: wire.AgentInfo{
			Hostname: host,
			OS:       runtime.GOOS,
			Version:  cfg.Version,
		},
		closed: make(chan struct{}),
	}, nil
}

// Run serves until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("agent: listen on %s: %w", a.cfg.Listen, err)
	}
	a.ln = ln
	a.log.Info("agent listening", "addr", ln.Addr().String(), "usbmuxd", a.mux.Socket())

	go a.watchDevices(ctx)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("agent: accept: %w", err)
		}
		go a.serve(ctx, conn)
	}
}

// Addr reports the address the agent is actually bound to, which differs from
// Config.Listen when port 0 was requested.
func (a *Agent) Addr() string {
	if a.ln == nil {
		return a.cfg.Listen
	}
	return a.ln.Addr().String()
}

// watchDevices keeps the registry in step with usbmuxd, reconnecting if the
// daemon restarts or the socket disappears.
func (a *Agent) watchDevices(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := a.followUsbmuxd(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("usbmuxd stream ended", "err", err, "retry_in", backoff)
		}
		// usbmuxd is gone or the stream broke: nothing is attached as far
		// as hosts are concerned until we can see it again.
		a.reg.Replace(nil)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (a *Agent) followUsbmuxd(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Subscribe before listing so a device that arrives during the initial
	// sync is not missed.
	events, err := a.mux.Listen(ctx)
	if err != nil {
		return err
	}
	devs, err := a.mux.ListDevices(ctx)
	if err != nil {
		return err
	}
	initial := make([]wire.DeviceInfo, 0, len(devs))
	for _, d := range devs {
		initial = append(initial, deviceFromEvent(d))
	}
	a.reg.Replace(initial)
	for _, d := range initial {
		a.log.Info("device attached", "udid", d.Udid, "device_id", d.DeviceID, "conn", d.ConnectionType)
	}

	for ev := range events {
		switch ev.MessageType {
		case usbmux.MsgAttached:
			d := deviceFromEvent(ev)
			a.log.Info("device attached", "udid", d.Udid, "device_id", d.DeviceID, "conn", d.ConnectionType)
			a.reg.Attach(d)
		case usbmux.MsgDetached:
			a.log.Info("device detached", "device_id", ev.DeviceID)
			a.reg.DetachByDeviceID(ev.DeviceID)
		}
	}
	return errors.New("usbmuxd event stream closed")
}

func (a *Agent) serve(ctx context.Context, conn net.Conn) {
	peer := conn.RemoteAddr().String()
	sess, err := link.Accept(conn, a.cfg.PSK)
	if err != nil {
		a.log.Warn("link rejected", "peer", peer, "err", err)
		return
	}
	a.log.Info("host connected", "peer", peer)
	defer func() {
		sess.Close()
		a.log.Info("host disconnected", "peer", peer)
	}()

	go func() {
		<-ctx.Done()
		sess.Close()
	}()

	for {
		req, stream, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go a.handleStream(ctx, req, stream)
	}
}

func (a *Agent) handleStream(ctx context.Context, req link.Request, st *link.Stream) {
	switch req.Kind {
	case link.KindControl:
		a.handleControl(ctx, req, st)
	case link.KindEvents:
		a.handleEvents(ctx, st)
	case link.KindDevice:
		a.handleDevice(ctx, req, st)
	case link.KindRelay:
		a.handleRelay(ctx, req, st)
	default:
		st.Fail(fmt.Errorf("agent: unknown stream kind %q", req.Kind))
	}
}

func (a *Agent) handleControl(ctx context.Context, req link.Request, st *link.Stream) {
	defer st.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reply := func(v any) {
		body, err := json.Marshal(v)
		if err != nil {
			st.Fail(err)
			return
		}
		_ = st.Reply(link.Response{Data: body})
	}

	switch req.Op {
	case wire.OpDescribe:
		reply(a.info)

	case wire.OpListDevices:
		reply(wire.ListDevicesResult{Devices: a.reg.List()})

	case wire.OpReadBUID:
		buid, err := a.mux.ReadBUID(ctx)
		if err != nil {
			st.Fail(err)
			return
		}
		reply(wire.ReadBUIDResult{BUID: buid})

	case wire.OpReadPairRecord:
		var args wire.ReadPairRecordArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			st.Fail(err)
			return
		}
		data, err := a.mux.ReadPairRecord(ctx, args.Udid)
		if err != nil {
			st.Fail(err)
			return
		}
		reply(wire.ReadPairRecordResult{Data: data})

	case wire.OpListServices:
		var svcs []wire.Service
		if a.cfg.Discovery != nil {
			svcs = a.cfg.Discovery.Services()
		}
		reply(wire.ListServicesResult{Services: svcs})

	default:
		st.Fail(fmt.Errorf("agent: unknown control op %q", req.Op))
	}
}

func (a *Agent) handleEvents(ctx context.Context, st *link.Stream) {
	defer st.Close()
	events, unsubscribe := a.reg.Subscribe()
	defer unsubscribe()

	if err := st.Reply(link.Response{}); err != nil {
		return
	}
	enc := json.NewEncoder(st)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
		}
	}
}

// handleDevice opens a lockdown or service port on a local device and splices
// it to the stream.
func (a *Agent) handleDevice(ctx context.Context, req link.Request, st *link.Stream) {
	dev, ok := a.reg.Lookup(req.Udid)
	if !ok {
		st.Fail(fmt.Errorf("agent: device %s is not attached", req.Udid))
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, err := a.mux.Connect(dctx, dev.DeviceID, req.Port)
	if err != nil {
		st.Fail(fmt.Errorf("agent: connect %s port %d: %w", req.Udid, req.Port, err))
		return
	}
	if err := st.Reply(link.Response{}); err != nil {
		conn.Close()
		st.Close()
		return
	}
	link.Splice(st, conn)
}
