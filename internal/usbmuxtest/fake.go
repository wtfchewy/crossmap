// Package usbmuxtest provides a fake usbmuxd for exercising Crossmap end to
// end without a physical iOS device.
//
// It implements the subset of the protocol Crossmap depends on: device
// enumeration, the Listen event stream, pair-record and BUID reads, and
// Connect, which hands the caller a live stream to a per-device handler.
package usbmuxtest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/wtfchewy/crossmap/internal/usbmux"
)

// Device is a synthetic device served by the fake.
type Device struct {
	Udid     string
	DeviceID int
	// PairRecord is returned for ReadPairRecord.
	PairRecord []byte
	// Handle serves a Connect to the given port. The fake closes the
	// returned connection when the client goes away.
	Handle func(port int) (net.Conn, error)
}

// Fake is a usbmuxd stand-in listening on a unix socket.
type Fake struct {
	Path string
	BUID string

	ln net.Listener

	mu        sync.RWMutex
	devices   map[string]Device
	listeners map[chan usbmux.Event]struct{}
}

// New starts a fake usbmuxd on a socket inside dir.
func New(dir string) (*Fake, error) {
	path := filepath.Join(dir, "fake-usbmuxd.sock")
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("usbmuxtest: listen: %w", err)
	}
	f := &Fake{
		Path:      path,
		BUID:      "00000000-FAKEBUID-0000-000000000000",
		ln:        ln,
		devices:   make(map[string]Device),
		listeners: make(map[chan usbmux.Event]struct{}),
	}
	go f.serve()
	return f, nil
}

// Close stops the fake.
func (f *Fake) Close() error {
	err := f.ln.Close()
	_ = os.Remove(f.Path)
	return err
}

// Attach adds a device and notifies listeners.
func (f *Fake) Attach(d Device) {
	f.mu.Lock()
	f.devices[d.Udid] = d
	f.mu.Unlock()
	f.broadcast(usbmux.Event{
		MessageType: usbmux.MsgAttached,
		DeviceID:    d.DeviceID,
		Properties:  propsFor(d),
	})
}

// Detach removes a device and notifies listeners.
func (f *Fake) Detach(udid string) {
	f.mu.Lock()
	d, ok := f.devices[udid]
	delete(f.devices, udid)
	f.mu.Unlock()
	if ok {
		f.broadcast(usbmux.Event{MessageType: usbmux.MsgDetached, DeviceID: d.DeviceID})
	}
}

func propsFor(d Device) usbmux.DeviceProperties {
	return usbmux.DeviceProperties{
		ConnectionType: "USB",
		DeviceID:       d.DeviceID,
		SerialNumber:   d.Udid,
		ProductID:      0x12a8,
		LocationID:     0x14100000,
	}
}

func (f *Fake) broadcast(ev usbmux.Event) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for ch := range f.listeners {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *Fake) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *Fake) handle(conn net.Conn) {
	defer conn.Close()
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
	result := func(tag uint32, code int) error {
		return send(tag, usbmux.Result{MessageType: usbmux.MsgResult, Number: code})
	}

	for {
		pkt, err := usbmux.ReadPacket(conn)
		if err != nil {
			return
		}
		var req usbmux.Request
		if err := usbmux.Decode(pkt.Payload, &req); err != nil {
			_ = result(pkt.Tag, usbmux.ResultBadCommand)
			continue
		}

		switch req.MessageType {
		case usbmux.MsgListDevices:
			f.mu.RLock()
			list := usbmux.DeviceList{}
			for _, d := range f.devices {
				list.DeviceList = append(list.DeviceList, usbmux.Event{
					MessageType: usbmux.MsgAttached,
					DeviceID:    d.DeviceID,
					Properties:  propsFor(d),
				})
			}
			f.mu.RUnlock()
			_ = send(pkt.Tag, list)

		case usbmux.MsgReadBUID:
			_ = send(pkt.Tag, usbmux.BUID{BUID: f.BUID})

		case usbmux.MsgReadPairRecord:
			f.mu.RLock()
			d, ok := f.devices[req.PairRecordID]
			f.mu.RUnlock()
			if !ok || len(d.PairRecord) == 0 {
				_ = result(pkt.Tag, usbmux.ResultBadDevice)
				continue
			}
			_ = send(pkt.Tag, usbmux.PairRecord{PairRecordData: d.PairRecord})

		case usbmux.MsgListen:
			f.streamEvents(pkt.Tag, send, result)
			return

		case usbmux.MsgConnect:
			port := ((req.PortNumber << 8) & 0xff00) | ((req.PortNumber >> 8) & 0xff)
			f.mu.RLock()
			var target Device
			found := false
			for _, d := range f.devices {
				if d.DeviceID == req.DeviceID {
					target, found = d, true
					break
				}
			}
			f.mu.RUnlock()
			if !found || target.Handle == nil {
				_ = result(pkt.Tag, usbmux.ResultBadDevice)
				continue
			}
			peer, err := target.Handle(port)
			if err != nil {
				_ = result(pkt.Tag, usbmux.ResultConnRefused)
				continue
			}
			if err := result(pkt.Tag, usbmux.ResultOK); err != nil {
				peer.Close()
				return
			}
			splice(conn, peer)
			return

		default:
			_ = result(pkt.Tag, usbmux.ResultBadCommand)
		}
	}
}

func (f *Fake) streamEvents(tag uint32, send func(uint32, any) error, result func(uint32, int) error) {
	if err := result(tag, usbmux.ResultOK); err != nil {
		return
	}
	ch := make(chan usbmux.Event, 32)
	f.mu.Lock()
	f.listeners[ch] = struct{}{}
	devices := make([]Device, 0, len(f.devices))
	for _, d := range f.devices {
		devices = append(devices, d)
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.listeners, ch)
		f.mu.Unlock()
	}()

	for _, d := range devices {
		if err := send(0, usbmux.Event{
			MessageType: usbmux.MsgAttached,
			DeviceID:    d.DeviceID,
			Properties:  propsFor(d),
		}); err != nil {
			return
		}
	}
	for ev := range ch {
		if err := send(0, ev); err != nil {
			return
		}
	}
}

func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyAll(b, a) }()
	go func() { defer wg.Done(); copyAll(a, b) }()
	wg.Wait()
	a.Close()
	b.Close()
}

func copyAll(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
