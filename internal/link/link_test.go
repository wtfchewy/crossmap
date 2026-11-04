package link

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

// pair spins up a listener and returns a connected client/server Session pair.
func pair(t *testing.T, clientPSK, serverPSK []byte) (*Session, *Session, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type res struct {
		s   *Session
		err error
	}
	srvCh := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srvCh <- res{nil, err}
			return
		}
		s, err := Accept(c, serverPSK)
		srvCh <- res{s, err}
	}()

	cli, cerr := Dial(ln.Addr().String(), clientPSK, 5*time.Second)
	sr := <-srvCh
	if cerr != nil {
		return nil, nil, cerr
	}
	if sr.err != nil {
		return nil, nil, sr.err
	}
	return cli, sr.s, nil
}

func TestHandshakeAndEcho(t *testing.T) {
	psk := []byte("shared-secret")
	cli, srv, err := pair(t, psk, psk)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer cli.Close()
	defer srv.Close()

	// Server side: echo whatever arrives after replying OK.
	go func() {
		req, st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		if req.Kind != KindRelay || req.Address != "1.2.3.4:49152" {
			st.Fail(io.ErrUnexpectedEOF)
			return
		}
		payload, _ := json.Marshal(map[string]string{"bound": "ok"})
		if err := st.Reply(Response{Data: payload}); err != nil {
			return
		}
		io.Copy(st, st)
		st.Close()
	}()

	conn, resp, err := cli.Open(Request{Kind: KindRelay, Network: "tcp", Address: "1.2.3.4:49152"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	var data map[string]string
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("response data: %v", err)
	}
	if data["bound"] != "ok" {
		t.Fatalf("unexpected response data %v", data)
	}

	want := bytes.Repeat([]byte("crossmap"), 4096)
	go func() {
		conn.Write(want)
		CloseWrite(conn)
	}()
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestHandshakeRejectsWrongPSK(t *testing.T) {
	_, _, err := pair(t, []byte("right"), []byte("wrong"))
	if err == nil {
		t.Fatal("expected authentication failure, got success")
	}
	t.Logf("rejected as expected: %v", err)
}

func TestHandshakeRejectsNonCrossmapPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		_, err = Accept(c, []byte("psk"))
		errCh <- err
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	c.Close()
	if err := <-errCh; err == nil {
		t.Fatal("expected rejection of non-Crossmap peer")
	} else {
		t.Logf("rejected as expected: %v", err)
	}
}
