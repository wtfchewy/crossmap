// Package host runs on the Mac that wants to use a remote iPhone. It serves a
// usbmuxd-compatible socket that merges locally attached devices with devices
// published by Crossmap agents, and mirrors the CoreDevice RemotePairing
// plane so Xcode and devicectl see the device as locally present.
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wtfchewy/crossmap/internal/devices"
	"github.com/wtfchewy/crossmap/internal/link"
	"github.com/wtfchewy/crossmap/internal/usbmux"
	"github.com/wtfchewy/crossmap/internal/wire"
)

// Source is a place devices come from: this machine's own usbmuxd, or a
// Crossmap agent across the tailnet.
type Source interface {
	// Name identifies the source in logs and in `crossmap status`.
	Name() string
	// Devices returns the source's current devices.
	Devices() []wire.DeviceInfo
	// Connect opens a device service port.
	Connect(ctx context.Context, udid string, port int) (net.Conn, error)
	// ReadPairRecord returns the trust record the source holds for a device.
	ReadPairRecord(ctx context.Context, udid string) ([]byte, error)
	// Subscribe streams device changes until the returned cancel is called.
	Subscribe() (<-chan wire.DeviceEvent, func())
}

// --- local source -------------------------------------------------------

// localSource exposes this machine's real usbmuxd. It exists so a Crossmap
// host that has taken over /var/run/usbmuxd still shows devices plugged into
// the host itself.
type localSource struct {
	mux *usbmux.Client
	reg *devices.Set
}

func newLocalSource(socket string) *localSource {
	return &localSource{mux: usbmux.NewClient(socket), reg: devices.New()}
}

func (l *localSource) Name() string                                 { return "local" }
func (l *localSource) Devices() []wire.DeviceInfo                   { return l.reg.List() }
func (l *localSource) Subscribe() (<-chan wire.DeviceEvent, func()) { return l.reg.Subscribe() }

func (l *localSource) Connect(ctx context.Context, udid string, port int) (net.Conn, error) {
	d, ok := l.reg.Lookup(udid)
	if !ok {
		return nil, usbmux.ErrBadDevice
	}
	return l.mux.Connect(ctx, d.DeviceID, port)
}

func (l *localSource) ReadPairRecord(ctx context.Context, udid string) ([]byte, error) {
	return l.mux.ReadPairRecord(ctx, udid)
}

// run keeps the local device set in step with usbmuxd.
func (l *localSource) run(ctx context.Context, onChange func()) {
	backoff := time.Second
	for ctx.Err() == nil {
		l.follow(ctx, onChange)
		l.reg.Replace(nil)
		onChange()
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

func (l *localSource) follow(ctx context.Context, onChange func()) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events, err := l.mux.Listen(ctx)
	if err != nil {
		return
	}
	devs, err := l.mux.ListDevices(ctx)
	if err != nil {
		return
	}
	initial := make([]wire.DeviceInfo, 0, len(devs))
	for _, d := range devs {
		initial = append(initial, wire.DeviceInfo{
			Udid:           d.Properties.Udid(),
			DeviceID:       d.DeviceID,
			ConnectionType: d.Properties.ConnectionType,
			ProductID:      d.Properties.ProductID,
			LocationID:     d.Properties.LocationID,
			Agent:          "local",
		})
	}
	l.reg.Replace(initial)
	onChange()

	for ev := range events {
		switch ev.MessageType {
		case usbmux.MsgAttached:
			l.reg.Attach(wire.DeviceInfo{
				Udid:           ev.Properties.Udid(),
				DeviceID:       ev.DeviceID,
				ConnectionType: ev.Properties.ConnectionType,
				ProductID:      ev.Properties.ProductID,
				LocationID:     ev.Properties.LocationID,
				Agent:          "local",
			})
		case usbmux.MsgDetached:
			l.reg.DetachByDeviceID(ev.DeviceID)
		}
		onChange()
	}
}

// --- remote source ------------------------------------------------------

// remoteSource is one Crossmap agent. It owns its link and reconnects on its
// own schedule so a sleeping laptop does not take the host down with it.
type remoteSource struct {
	name    string
	addr    string
	psk     []byte
	reg     *devices.Set
	mu      sync.RWMutex
	sess    *link.Session
	info    wire.AgentInfo
	svcs    []wire.Service
	lastErr error
}

func newRemoteSource(name, addr string, psk []byte) *remoteSource {
	return &remoteSource{name: name, addr: addr, psk: psk, reg: devices.New()}
}

func (r *remoteSource) Name() string                                 { return r.name }
func (r *remoteSource) Devices() []wire.DeviceInfo                   { return r.reg.List() }
func (r *remoteSource) Subscribe() (<-chan wire.DeviceEvent, func()) { return r.reg.Subscribe() }

func (r *remoteSource) session() (*link.Session, error) {
	r.mu.RLock()
	s := r.sess
	r.mu.RUnlock()
	if s == nil || s.Closed() {
		return nil, fmt.Errorf("host: agent %s is not connected", r.name)
	}
	return s, nil
}

// Info returns what the agent said about itself, for `crossmap status`.
func (r *remoteSource) Info() (wire.AgentInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.info, r.lastErr
}

// Services returns the CoreDevice advertisements the agent reported.
func (r *remoteSource) Services() []wire.Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]wire.Service(nil), r.svcs...)
}

func (r *remoteSource) Connect(ctx context.Context, udid string, port int) (net.Conn, error) {
	sess, err := r.session()
	if err != nil {
		return nil, err
	}
	conn, _, err := sess.Open(link.Request{Kind: link.KindDevice, Udid: udid, Port: port})
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// RelayTCP opens a raw relay to an address reachable from the agent.
func (r *remoteSource) RelayTCP(address string) (net.Conn, error) {
	sess, err := r.session()
	if err != nil {
		return nil, err
	}
	conn, _, err := sess.Open(link.Request{Kind: link.KindRelay, Network: "tcp", Address: address})
	return conn, err
}

// RelayUDP opens a datagram relay to an address reachable from the agent.
func (r *remoteSource) RelayUDP(address string) (net.Conn, error) {
	sess, err := r.session()
	if err != nil {
		return nil, err
	}
	conn, _, err := sess.Open(link.Request{Kind: link.KindRelay, Network: "udp", Address: address})
	return conn, err
}

func (r *remoteSource) ReadPairRecord(ctx context.Context, udid string) ([]byte, error) {
	var out wire.ReadPairRecordResult
	args, _ := json.Marshal(wire.ReadPairRecordArgs{Udid: udid})
	if err := r.control(link.Request{Kind: link.KindControl, Op: wire.OpReadPairRecord, Args: args}, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// control runs a single request/response control RPC.
func (r *remoteSource) control(req link.Request, out any) error {
	sess, err := r.session()
	if err != nil {
		return err
	}
	conn, resp, err := sess.Open(req)
	if err != nil {
		return err
	}
	defer conn.Close()
	if out == nil {
		return nil
	}
	return json.Unmarshal(resp.Data, out)
}
