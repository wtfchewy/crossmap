package usbmux

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// DefaultSocket is the path Apple's usbmuxd listens on.
const DefaultSocket = "/var/run/usbmuxd"

// Client talks to a usbmuxd-compatible socket.
//
// Every operation uses its own connection, mirroring libusbmuxd: usbmuxd turns
// a connection into a raw device stream after a successful Connect, so a
// shared multiplexed connection would be unusable afterwards.
type Client struct {
	socket string
	tag    atomic.Uint32
}

// NewClient returns a Client for the usbmuxd socket at path.
func NewClient(path string) *Client {
	if path == "" {
		path = DefaultSocket
	}
	return &Client{socket: path}
}

// Socket reports the path this client dials.
func (c *Client) Socket() string { return c.socket }

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("usbmux: dial %s: %w", c.socket, err)
	}
	return conn, nil
}

func (c *Client) nextTag() uint32 { return c.tag.Add(1) }

// request sends one message and reads one reply on a fresh connection.
func (c *Client) request(ctx context.Context, req Request, reply any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	if err := c.send(conn, req); err != nil {
		return err
	}
	pkt, err := ReadPacket(conn)
	if err != nil {
		return fmt.Errorf("usbmux: read %s reply: %w", req.MessageType, err)
	}
	if reply == nil {
		return nil
	}
	return Decode(pkt.Payload, reply)
}

func (c *Client) send(conn net.Conn, req Request) error {
	req.ClientVersionString = ClientVersion
	req.ProgName = ProgName
	req.LibUSBMuxVersion = 3
	payload, err := Encode(req)
	if err != nil {
		return fmt.Errorf("usbmux: encode %s: %w", req.MessageType, err)
	}
	if err := WritePacket(conn, &Packet{Tag: c.nextTag(), Payload: payload}); err != nil {
		return fmt.Errorf("usbmux: write %s: %w", req.MessageType, err)
	}
	return nil
}

// ListDevices returns the devices usbmuxd currently knows about.
func (c *Client) ListDevices(ctx context.Context) ([]Event, error) {
	var list DeviceList
	if err := c.request(ctx, Request{MessageType: MsgListDevices}, &list); err != nil {
		return nil, err
	}
	// usbmuxd omits DeviceID from the envelope in some versions; backfill it
	// from Properties so downstream code can rely on one field.
	for i := range list.DeviceList {
		if list.DeviceList[i].DeviceID == 0 {
			list.DeviceList[i].DeviceID = list.DeviceList[i].Properties.DeviceID
		}
	}
	return list.DeviceList, nil
}

// ReadBUID returns this host's system BUID.
func (c *Client) ReadBUID(ctx context.Context) (string, error) {
	var b BUID
	if err := c.request(ctx, Request{MessageType: MsgReadBUID}, &b); err != nil {
		return "", err
	}
	return b.BUID, nil
}

// ReadPairRecord returns the stored pair record for a UDID. This is how a
// remote host's trust relationship with a device travels to Crossmap's host
// side without copying files out of /var/db/lockdown.
func (c *Client) ReadPairRecord(ctx context.Context, udid string) ([]byte, error) {
	var rec PairRecord
	err := c.request(ctx, Request{MessageType: MsgReadPairRecord, PairRecordID: udid}, &rec)
	if err != nil {
		return nil, err
	}
	if len(rec.PairRecordData) == 0 {
		return nil, fmt.Errorf("usbmux: no pair record for %s", udid)
	}
	return rec.PairRecordData, nil
}

// Connect opens a stream to a port on a device. On success the returned
// connection carries raw device traffic with no further framing.
func (c *Client) Connect(ctx context.Context, deviceID, port int) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	req := Request{MessageType: MsgConnect, DeviceID: deviceID, PortNumber: swap16(port)}
	if err := c.send(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	pkt, err := ReadPacket(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("usbmux: read Connect reply: %w", err)
	}
	var res Result
	if err := Decode(pkt.Payload, &res); err != nil {
		conn.Close()
		return nil, err
	}
	if res.Number != ResultOK {
		conn.Close()
		if res.Number == ResultBadDevice {
			return nil, ErrBadDevice
		}
		return nil, fmt.Errorf("usbmux: Connect to device %d port %d refused (result %d)", deviceID, port, res.Number)
	}
	// Clear the handshake deadline; the caller owns stream timing from here.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// Listen streams attach and detach events until ctx is cancelled or the
// connection fails. Events are delivered in order on the returned channel,
// which is closed when the stream ends.
func (c *Client) Listen(ctx context.Context) (<-chan Event, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.send(conn, Request{MessageType: MsgListen}); err != nil {
		conn.Close()
		return nil, err
	}
	pkt, err := ReadPacket(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("usbmux: read Listen reply: %w", err)
	}
	var res Result
	if err := Decode(pkt.Payload, &res); err != nil {
		conn.Close()
		return nil, err
	}
	if res.Number != ResultOK {
		conn.Close()
		return nil, fmt.Errorf("usbmux: Listen refused (result %d)", res.Number)
	}
	_ = conn.SetDeadline(time.Time{})

	events := make(chan Event, 16)
	go func() {
		defer close(events)
		defer conn.Close()
		go func() {
			<-ctx.Done()
			conn.Close()
		}()
		for {
			pkt, err := ReadPacket(conn)
			if err != nil {
				return
			}
			var ev Event
			if err := Decode(pkt.Payload, &ev); err != nil {
				continue
			}
			if ev.MessageType != MsgAttached && ev.MessageType != MsgDetached {
				continue
			}
			if ev.DeviceID == 0 {
				ev.DeviceID = ev.Properties.DeviceID
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, nil
}
