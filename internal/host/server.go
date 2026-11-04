package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wtfchewy/crossmap/internal/devices"
	"github.com/wtfchewy/crossmap/internal/link"
	"github.com/wtfchewy/crossmap/internal/usbmux"
	"github.com/wtfchewy/crossmap/internal/wire"
)

// maxUnixPath is the size of sun_path on Darwin.
const maxUnixPath = 104

// route records where a device in the merged table actually lives.
type route struct {
	source Source
	udid   string
}

// Server presents a usbmuxd-compatible socket backed by several Sources.
//
// Devices from every source share one ID space. Crossmap assigns its own
// DeviceIDs rather than passing through each source's, because two sources can
// legitimately use the same DeviceID for different devices.
type Server struct {
	log     *slog.Logger
	local   *localSource
	mu      sync.RWMutex
	sources []Source
	byID    map[int]route
	byUdid  map[string]int
	nextID  int
	merged  *devices.Set
}

// NewServer returns a Server whose only source is the local usbmuxd at
// localSocket. Pass "" to use Apple's default path.
func NewServer(localSocket string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	local := newLocalSource(localSocket)
	s := &Server{
		log:    log,
		local:  local,
		byID:   make(map[int]route),
		byUdid: make(map[string]int),
		// Start well above the small integers Apple's usbmuxd hands out so
		// a stray ID from either side is obviously ours in logs.
		nextID: 1000,
		merged: devices.New(),
	}
	s.sources = []Source{local}
	return s
}

// AddSource registers an additional device source.
func (s *Server) AddSource(src Source) {
	s.mu.Lock()
	s.sources = append(s.sources, src)
	s.mu.Unlock()
	s.Refresh()
}

// Refresh recomputes the merged device table from every source. It is called
// whenever any source reports a change; recomputing wholesale rather than
// applying deltas keeps the table correct even when events are dropped.
func (s *Server) Refresh() {
	var merged []wire.DeviceInfo
	byID := make(map[int]route)

	s.mu.Lock()
	for _, src := range s.sources {
		for _, d := range src.Devices() {
			id, ok := s.byUdid[d.Udid]
			if !ok {
				id = s.nextID
				s.nextID++
				s.byUdid[d.Udid] = id
			}
			d.DeviceID = id
			if d.Agent == "" {
				d.Agent = src.Name()
			}
			byID[id] = route{source: src, udid: d.Udid}
			merged = append(merged, d)
		}
	}
	s.byID = byID
	s.mu.Unlock()

	s.merged.Replace(merged)
}

// Devices returns the merged device table.
func (s *Server) Devices() []wire.DeviceInfo { return s.merged.List() }

// Sources returns the registered sources.
func (s *Server) Sources() []Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Source(nil), s.sources...)
}

func (s *Server) resolve(deviceID int) (route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[deviceID]
	return r, ok
}

// sourceFor finds which source owns a UDID, for pair-record lookups that
// arrive without a DeviceID.
func (s *Server) sourceFor(udid string) (Source, bool) {
	s.mu.RLock()
	id, ok := s.byUdid[udid]
	r, ok2 := s.byID[id]
	s.mu.RUnlock()
	if !ok || !ok2 {
		return nil, false
	}
	return r.source, true
}

// Run starts the local source watcher and serves the socket at path until ctx
// is cancelled.
func (s *Server) Run(ctx context.Context, path string) error {
	go s.local.run(ctx, s.Refresh)

	ln, err := listenUnix(path)
	if err != nil {
		return err
	}
	s.log.Info("serving usbmuxd socket", "path", path)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer os.Remove(path)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("host: accept: %w", err)
		}
		go s.serveClient(ctx, conn)
	}
}

// listenUnix binds a unix socket, clearing a stale one left by a previous
// crash. The socket is world-accessible because every user-level tool on the
// machine expects to reach usbmuxd, matching Apple's own permissions.
func listenUnix(path string) (net.Listener, error) {
	// macOS caps sun_path at 104 bytes and reports an overrun only as
	// EINVAL from bind, which is impossible to act on.
	if len(path) >= maxUnixPath {
		return nil, fmt.Errorf("host: socket path %s is %d bytes; macOS allows at most %d - choose a shorter path with --socket",
			path, len(path), maxUnixPath-1)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("host: create socket directory: %w", err)
	}
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("host: %s exists and is not a socket; refusing to replace it", path)
		}
		// A live daemon would still be accepting; only clear a dead socket.
		if c, derr := net.DialTimeout("unix", path, 500*time.Millisecond); derr == nil {
			c.Close()
			return nil, fmt.Errorf("host: %s is already served by a live daemon", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("host: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("host: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, fmt.Errorf("host: chmod socket: %w", err)
	}
	return ln, nil
}

// serveClient speaks the usbmuxd protocol to one connected tool.
func (s *Server) serveClient(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex
	send := func(tag uint32, v any) error {
		payload, err := usbmux.Encode(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return usbmux.WritePacket(conn, &usbmux.Packet{Tag: tag, Payload: payload})
	}
	sendResult := func(tag uint32, code int) error {
		return send(tag, usbmux.Result{MessageType: usbmux.MsgResult, Number: code})
	}

	for {
		pkt, err := usbmux.ReadPacket(conn)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				s.log.Debug("client read ended", "err", err)
			}
			return
		}
		msgType, err := usbmux.MessageType(pkt.Payload)
		if err != nil {
			_ = sendResult(pkt.Tag, usbmux.ResultBadCommand)
			continue
		}

		switch msgType {
		case usbmux.MsgListDevices:
			_ = send(pkt.Tag, usbmux.DeviceList{DeviceList: s.attachedEvents()})

		case usbmux.MsgListen:
			// Listen consumes the connection: it becomes an event stream.
			s.streamEvents(ctx, pkt.Tag, send, sendResult)
			return

		case usbmux.MsgConnect:
			var req usbmux.Request
			if err := usbmux.Decode(pkt.Payload, &req); err != nil {
				_ = sendResult(pkt.Tag, usbmux.ResultBadCommand)
				continue
			}
			s.handleConnect(ctx, conn, pkt.Tag, req, sendResult)
			return // the connection is now a raw device stream

		case usbmux.MsgReadBUID:
			buid, err := s.local.mux.ReadBUID(ctx)
			if err != nil {
				_ = sendResult(pkt.Tag, usbmux.ResultBadDevice)
				continue
			}
			_ = send(pkt.Tag, usbmux.BUID{BUID: buid})

		case usbmux.MsgReadPairRecord:
			var req usbmux.Request
			if err := usbmux.Decode(pkt.Payload, &req); err != nil {
				_ = sendResult(pkt.Tag, usbmux.ResultBadCommand)
				continue
			}
			data, err := s.readPairRecord(ctx, req.PairRecordID)
			if err != nil {
				s.log.Debug("pair record unavailable", "udid", req.PairRecordID, "err", err)
				_ = sendResult(pkt.Tag, usbmux.ResultBadDevice)
				continue
			}
			_ = send(pkt.Tag, usbmux.PairRecord{PairRecordData: data})

		case usbmux.MsgSavePairRecord, usbmux.MsgDeletePairRecord:
			// Pair records for remote devices belong to the agent; Crossmap
			// does not rewrite them from here.
			_ = sendResult(pkt.Tag, usbmux.ResultOK)

		default:
			_ = sendResult(pkt.Tag, usbmux.ResultBadCommand)
		}
	}
}

// attachedEvents renders the merged table as usbmuxd Attached events.
func (s *Server) attachedEvents() []usbmux.Event {
	devs := s.merged.List()
	out := make([]usbmux.Event, 0, len(devs))
	for _, d := range devs {
		out = append(out, usbmux.Event{
			MessageType: usbmux.MsgAttached,
			DeviceID:    d.DeviceID,
			Properties: usbmux.DeviceProperties{
				ConnectionType:  connectionType(d),
				ConnectionSpeed: d.ConnectionSpeed,
				DeviceID:        d.DeviceID,
				LocationID:      d.LocationID,
				ProductID:       d.ProductID,
				SerialNumber:    d.Udid,
				UDID:            d.Udid,
			},
		})
	}
	return out
}

// connectionType reports a device as USB-attached regardless of which machine
// it is plugged into. Tools such as ideviceinstaller treat "Network" devices
// as second-class and may skip them, and from the caller's point of view the
// device really is on the far end of a cable.
func connectionType(d wire.DeviceInfo) string {
	if d.ConnectionType == "" {
		return "USB"
	}
	return d.ConnectionType
}

func (s *Server) streamEvents(ctx context.Context, tag uint32, send func(uint32, any) error, sendResult func(uint32, int) error) {
	if err := sendResult(tag, usbmux.ResultOK); err != nil {
		return
	}
	events, unsubscribe := s.merged.Subscribe()
	defer unsubscribe()

	// usbmuxd replays the current device set to a new listener before
	// streaming changes.
	for _, ev := range s.attachedEvents() {
		if err := send(0, ev); err != nil {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			var out usbmux.Event
			if ev.Type == wire.EventAttached {
				out = usbmux.Event{
					MessageType: usbmux.MsgAttached,
					DeviceID:    ev.Device.DeviceID,
					Properties: usbmux.DeviceProperties{
						ConnectionType: connectionType(ev.Device),
						DeviceID:       ev.Device.DeviceID,
						LocationID:     ev.Device.LocationID,
						ProductID:      ev.Device.ProductID,
						SerialNumber:   ev.Device.Udid,
						UDID:           ev.Device.Udid,
					},
				}
			} else {
				out = usbmux.Event{MessageType: usbmux.MsgDetached, DeviceID: ev.Device.DeviceID}
			}
			if err := send(0, out); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleConnect(ctx context.Context, conn net.Conn, tag uint32, req usbmux.Request, sendResult func(uint32, int) error) {
	r, ok := s.resolve(req.DeviceID)
	if !ok {
		_ = sendResult(tag, usbmux.ResultBadDevice)
		return
	}
	// PortNumber arrives byte-swapped, as usbmuxd expects from libusbmuxd.
	port := ((req.PortNumber << 8) & 0xff00) | ((req.PortNumber >> 8) & 0xff)

	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	remote, err := r.source.Connect(dctx, r.udid, port)
	if err != nil {
		s.log.Warn("connect failed", "udid", r.udid, "port", port, "source", r.source.Name(), "err", err)
		_ = sendResult(tag, usbmux.ResultConnRefused)
		return
	}
	if err := sendResult(tag, usbmux.ResultOK); err != nil {
		remote.Close()
		return
	}
	s.log.Debug("device stream open", "udid", r.udid, "port", port, "source", r.source.Name())
	link.Splice(conn, remote)
}

// readPairRecord fetches a device's trust record from whichever source owns
// it, falling back to the local store so devices this Mac has paired with
// directly keep working.
func (s *Server) readPairRecord(ctx context.Context, udid string) ([]byte, error) {
	if udid == "" {
		return nil, errors.New("host: empty pair record id")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if src, ok := s.sourceFor(udid); ok {
		data, err := src.ReadPairRecord(ctx, udid)
		if err == nil && len(data) > 0 {
			return data, nil
		}
	}
	return s.local.ReadPairRecord(ctx, udid)
}
