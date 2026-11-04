package link

import (
	"encoding/binary"
	"io"
	"net"
)

// MaxDatagram bounds a single relayed UDP payload.
const MaxDatagram = 65535

// Datagram relays carry UDP over a yamux stream, which is a byte stream and
// would otherwise destroy message boundaries. Each payload is framed with a
// 2-byte big-endian length. This matters because the CoreDevice trusted
// tunnel runs QUIC, which does not tolerate coalesced or split datagrams.

// WriteDatagram frames and writes one payload.
func WriteDatagram(w io.Writer, payload []byte) error {
	if len(payload) > MaxDatagram {
		return io.ErrShortWrite
	}
	buf := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload)))
	copy(buf[2:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadDatagrams reads framed payloads until the stream ends or handle returns
// false. The slice passed to handle is only valid for the duration of the
// call.
func ReadDatagrams(r io.Reader, handle func([]byte) bool) error {
	var lenbuf [2]byte
	buf := make([]byte, MaxDatagram)
	for {
		if _, err := io.ReadFull(r, lenbuf[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint16(lenbuf[:]))
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return err
		}
		if !handle(buf[:n]) {
			return nil
		}
	}
}

// PumpDatagrams bridges a framed datagram stream and a connected UDP socket in
// both directions until either side fails.
func PumpDatagrams(stream net.Conn, conn net.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ReadDatagrams(stream, func(b []byte) bool {
			_, err := conn.Write(b)
			return err == nil
		})
	}()

	buf := make([]byte, MaxDatagram)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		if err := WriteDatagram(stream, buf[:n]); err != nil {
			break
		}
	}
	stream.Close()
	conn.Close()
	<-done
}
