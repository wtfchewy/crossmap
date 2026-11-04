// Package usbmux implements enough of the usbmuxd wire protocol for Crossmap to
// act as both a client of a real usbmuxd and a server standing in for one.
//
// The wire format is a 16-byte little-endian header followed by a property
// list. Apple's usbmuxd accepts XML plists, which is what we emit; it may reply
// with either XML or binary, so decoding is format-agnostic.
package usbmux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"howett.net/plist"
)

const (
	// headerLen is the fixed size of the usbmuxd packet header.
	headerLen = 16
	// versionPlist selects the plist-based protocol (as opposed to the
	// long-obsolete binary struct protocol).
	versionPlist = 1
	// messageTypePlist is the only message type used by the plist protocol.
	messageTypePlist = 8

	// maxPayload bounds a single plist message so a malformed or hostile
	// length field cannot make us allocate without limit.
	maxPayload = 8 << 20
)

// ProgName identifies Crossmap to usbmuxd in Listen requests.
const ProgName = "crossmap"

// ClientVersion is the libusbmuxd-compatible version string we advertise.
const ClientVersion = "crossmap-1.0"

// Message types we send or recognise.
const (
	MsgResult           = "Result"
	MsgConnect          = "Connect"
	MsgListen           = "Listen"
	MsgListDevices      = "ListDevices"
	MsgAttached         = "Attached"
	MsgDetached         = "Detached"
	MsgPaired           = "Paired"
	MsgReadBUID         = "ReadBUID"
	MsgReadPairRecord   = "ReadPairRecord"
	MsgSavePairRecord   = "SavePairRecord"
	MsgDeletePairRecord = "DeletePairRecord"
)

// Result codes returned by usbmuxd.
const (
	ResultOK          = 0
	ResultBadCommand  = 1
	ResultBadDevice   = 2
	ResultConnRefused = 3
	ResultBadVersion  = 6
)

// ErrBadDevice reports that usbmuxd does not know the requested device.
var ErrBadDevice = errors.New("usbmux: unknown device")

// Packet is one framed plist message plus the tag used to correlate a reply
// with its request.
type Packet struct {
	Tag     uint32
	Payload []byte
}

// ReadPacket reads a single framed message from r.
func ReadPacket(r io.Reader) (*Packet, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(hdr[0:4])
	version := binary.LittleEndian.Uint32(hdr[4:8])
	msgType := binary.LittleEndian.Uint32(hdr[8:12])
	tag := binary.LittleEndian.Uint32(hdr[12:16])

	if version != versionPlist || msgType != messageTypePlist {
		return nil, fmt.Errorf("usbmux: unsupported frame (version %d, type %d)", version, msgType)
	}
	if length < headerLen || length > headerLen+maxPayload {
		return nil, fmt.Errorf("usbmux: implausible frame length %d", length)
	}

	payload := make([]byte, length-headerLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return &Packet{Tag: tag, Payload: payload}, nil
}

// WritePacket frames and writes a single message to w.
func WritePacket(w io.Writer, p *Packet) error {
	var hdr [headerLen]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(headerLen+len(p.Payload)))
	binary.LittleEndian.PutUint32(hdr[4:8], versionPlist)
	binary.LittleEndian.PutUint32(hdr[8:12], messageTypePlist)
	binary.LittleEndian.PutUint32(hdr[12:16], p.Tag)

	// One write per frame keeps frames atomic on the wire for concurrent
	// writers that hold the connection's write lock.
	buf := make([]byte, 0, headerLen+len(p.Payload))
	buf = append(buf, hdr[:]...)
	buf = append(buf, p.Payload...)
	_, err := w.Write(buf)
	return err
}

// Encode marshals v as an XML plist suitable for a packet payload.
func Encode(v any) ([]byte, error) {
	return plist.Marshal(v, plist.XMLFormat)
}

// Decode unmarshals a packet payload, accepting any plist encoding.
func Decode(payload []byte, v any) error {
	_, err := plist.Unmarshal(payload, v)
	return err
}

// MessageType peeks at the "MessageType" key of a payload without committing to
// a full struct, so a dispatcher can route before decoding.
func MessageType(payload []byte) (string, error) {
	var probe struct {
		MessageType string `plist:"MessageType"`
	}
	if err := Decode(payload, &probe); err != nil {
		return "", err
	}
	return probe.MessageType, nil
}
