// Package link carries Crossmap traffic between the agent (the Mac the iPhone
// is plugged into) and the host (the Mac that wants to see the iPhone).
//
// The wire runs over the tailnet, which already provides encryption and
// machine identity, so link adds only a pre-shared-key handshake to make sure
// both ends belong to the same Crossmap deployment, then multiplexes every
// logical channel over one TCP connection with yamux.
package link

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// Protocol constants.
var magic = [8]byte{'C', 'R', 'O', 'S', 'S', 'M', 'A', 'P'}

const (
	protocolVersion = 1
	nonceLen        = 32
	handshakeBudget = 10 * time.Second
)

// ErrAuth reports a failed or mismatched pre-shared-key handshake.
var ErrAuth = errors.New("link: authentication failed")

// StreamKind identifies what a newly opened stream is for. The kind travels in
// the stream's opening request so the far side can dispatch without a separate
// control round trip.
type StreamKind string

const (
	// KindControl carries request/response RPCs (device lists, pair records).
	KindControl StreamKind = "control"
	// KindEvents is a server-push stream of device attach/detach events.
	KindEvents StreamKind = "events"
	// KindDevice becomes a raw lockdown/service stream to a device port.
	KindDevice StreamKind = "device"
	// KindRelay becomes a raw TCP or UDP relay to an address reachable from
	// the agent, used for the CoreDevice RemotePairing plane.
	KindRelay StreamKind = "relay"
)

// Request opens a stream. Only the fields relevant to Kind are populated.
type Request struct {
	Kind StreamKind `json:"kind"`

	// Control
	Op   string          `json:"op,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`

	// Device
	Udid string `json:"udid,omitempty"`
	Port int    `json:"port,omitempty"`

	// Relay
	Network string `json:"network,omitempty"` // "tcp" or "udp"
	Address string `json:"address,omitempty"` // host:port as seen by the agent
}

// Response acknowledges a Request. A stream carrying raw bytes switches to
// those bytes immediately after a Response with Err empty.
type Response struct {
	Err  string          `json:"err,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Error returns the response's error as a Go error, or nil.
func (r Response) Error() error {
	if r.Err == "" {
		return nil
	}
	return errors.New(r.Err)
}

// Session is an authenticated, multiplexed connection to the far side.
type Session struct {
	mux  *yamux.Session
	peer string
}

// Peer reports the remote address.
func (s *Session) Peer() string { return s.peer }

// Close tears down the session and every stream on it.
func (s *Session) Close() error { return s.mux.Close() }

// Closed reports whether the session has gone away.
func (s *Session) Closed() bool { return s.mux.IsClosed() }

// CloseChan fires when the session dies, so supervisors can reconnect.
func (s *Session) CloseChan() <-chan struct{} { return s.mux.CloseChan() }

func muxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	// Device streams can idle for a long time (idevicesyslog, a paused
	// debugger) while the link itself must still be proven alive.
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.MaxStreamWindowSize = 1 << 20
	cfg.LogOutput = io.Discard
	return cfg
}

// Dial connects to a Crossmap agent and completes the handshake.
func Dial(addr string, psk []byte, timeout time.Duration) (*Session, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("link: dial %s: %w", addr, err)
	}
	if err := clientHandshake(conn, psk); err != nil {
		conn.Close()
		return nil, err
	}
	mux, err := yamux.Client(conn, muxConfig())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("link: mux: %w", err)
	}
	return &Session{mux: mux, peer: conn.RemoteAddr().String()}, nil
}

// Accept completes the server side of the handshake on an already-accepted
// TCP connection and returns a multiplexed session.
func Accept(conn net.Conn, psk []byte) (*Session, error) {
	if err := serverHandshake(conn, psk); err != nil {
		conn.Close()
		return nil, err
	}
	mux, err := yamux.Server(conn, muxConfig())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("link: mux: %w", err)
	}
	return &Session{mux: mux, peer: conn.RemoteAddr().String()}, nil
}

// Open starts a stream, sends req, and waits for the response. The returned
// conn is positioned at the first raw byte for stream kinds that carry one.
func (s *Session) Open(req Request) (net.Conn, Response, error) {
	stream, err := s.mux.OpenStream()
	if err != nil {
		return nil, Response{}, fmt.Errorf("link: open stream: %w", err)
	}
	if err := writeJSON(stream, req); err != nil {
		stream.Close()
		return nil, Response{}, err
	}
	var resp Response
	if err := readJSON(stream, &resp); err != nil {
		stream.Close()
		return nil, Response{}, err
	}
	if err := resp.Error(); err != nil {
		stream.Close()
		return nil, resp, err
	}
	return stream, resp, nil
}

// Stream is an accepted stream whose opening request has been read but whose
// response has not yet been sent.
type Stream struct {
	net.Conn
	replied bool
}

// Reply sends the response that unblocks the opener. Call it once; afterwards
// the stream carries raw bytes.
func (st *Stream) Reply(resp Response) error {
	if st.replied {
		return errors.New("link: stream already replied")
	}
	st.replied = true
	return writeJSON(st.Conn, resp)
}

// Fail replies with an error and closes the stream.
func (st *Stream) Fail(err error) {
	if !st.replied {
		_ = st.Reply(Response{Err: err.Error()})
	}
	st.Close()
}

// AcceptStream waits for the next inbound stream and reads its request.
func (s *Session) AcceptStream() (Request, *Stream, error) {
	raw, err := s.mux.AcceptStream()
	if err != nil {
		return Request{}, nil, err
	}
	var req Request
	// A peer that opens a stream must state its intent promptly.
	_ = raw.SetReadDeadline(time.Now().Add(handshakeBudget))
	if err := readJSON(raw, &req); err != nil {
		raw.Close()
		return Request{}, nil, err
	}
	_ = raw.SetReadDeadline(time.Time{})
	return req, &Stream{Conn: raw}, nil
}

// writeJSON writes a length-prefixed JSON value.
func writeJSON(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("link: encode: %w", err)
	}
	if len(body) > 1<<20 {
		return errors.New("link: message too large")
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(body)))
	copy(buf[4:], body)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("link: write: %w", err)
	}
	return nil
}

// readJSON reads a length-prefixed JSON value.
func readJSON(r io.Reader, v any) error {
	var lenbuf [4]byte
	if _, err := io.ReadFull(r, lenbuf[:]); err != nil {
		return fmt.Errorf("link: read length: %w", err)
	}
	n := binary.BigEndian.Uint32(lenbuf[:])
	if n > 1<<20 {
		return errors.New("link: message too large")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("link: read body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("link: decode: %w", err)
	}
	return nil
}

// --- handshake ---------------------------------------------------------
//
// Both sides prove knowledge of the PSK over fresh nonces. The tailnet
// already authenticates and encrypts the pipe; this exists so a stray or
// misconfigured peer cannot drive a Crossmap agent, and so the PSK itself is
// never sent.

func proof(psk, clientNonce, serverNonce []byte, label string) []byte {
	mac := hmac.New(sha256.New, psk)
	mac.Write(clientNonce)
	mac.Write(serverNonce)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

func clientHandshake(conn net.Conn, psk []byte) error {
	_ = conn.SetDeadline(time.Now().Add(handshakeBudget))
	defer conn.SetDeadline(time.Time{})

	clientNonce := make([]byte, nonceLen)
	if _, err := rand.Read(clientNonce); err != nil {
		return err
	}
	hello := make([]byte, 0, len(magic)+1+nonceLen)
	hello = append(hello, magic[:]...)
	hello = append(hello, protocolVersion)
	hello = append(hello, clientNonce...)
	if _, err := conn.Write(hello); err != nil {
		return fmt.Errorf("link: write hello: %w", err)
	}

	serverNonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(conn, serverNonce); err != nil {
		return fmt.Errorf("link: read server nonce: %w", err)
	}
	serverProof := make([]byte, sha256.Size)
	if _, err := io.ReadFull(conn, serverProof); err != nil {
		return fmt.Errorf("link: read server proof: %w", err)
	}
	if !hmac.Equal(serverProof, proof(psk, clientNonce, serverNonce, "server")) {
		return ErrAuth
	}
	if _, err := conn.Write(proof(psk, clientNonce, serverNonce, "client")); err != nil {
		return fmt.Errorf("link: write client proof: %w", err)
	}
	return nil
}

func serverHandshake(conn net.Conn, psk []byte) error {
	_ = conn.SetDeadline(time.Now().Add(handshakeBudget))
	defer conn.SetDeadline(time.Time{})

	var head [len(magic) + 1]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("link: read hello: %w", err)
	}
	if string(head[:len(magic)]) != string(magic[:]) {
		return errors.New("link: not a Crossmap peer")
	}
	if head[len(magic)] != protocolVersion {
		return fmt.Errorf("link: protocol version %d unsupported (want %d)", head[len(magic)], protocolVersion)
	}
	clientNonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(conn, clientNonce); err != nil {
		return fmt.Errorf("link: read client nonce: %w", err)
	}
	serverNonce := make([]byte, nonceLen)
	if _, err := rand.Read(serverNonce); err != nil {
		return err
	}
	out := append(serverNonce, proof(psk, clientNonce, serverNonce, "server")...)
	if _, err := conn.Write(out); err != nil {
		return fmt.Errorf("link: write server proof: %w", err)
	}
	clientProof := make([]byte, sha256.Size)
	if _, err := io.ReadFull(conn, clientProof); err != nil {
		return fmt.Errorf("link: read client proof: %w", err)
	}
	if !hmac.Equal(clientProof, proof(psk, clientNonce, serverNonce, "client")) {
		return ErrAuth
	}
	return nil
}
