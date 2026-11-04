package link

import (
	"io"
	"net"
	"sync"
)

// halfCloser is implemented by TCP and Unix connections, which can signal EOF
// in one direction while still reading. yamux streams do not implement it:
// their Close sends a FIN and leaves the read side usable, so it serves the
// same purpose.
type halfCloser interface{ CloseWrite() error }

// CloseWrite signals end-of-stream to the peer without tearing down the read
// direction, using whichever mechanism the connection supports.
func CloseWrite(c net.Conn) error {
	if hc, ok := c.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return c.Close()
}

// Splice joins two connections and copies in both directions until each side
// reaches EOF, propagating half-close so protocols that signal completion by
// shutting down one direction (lockdown, debugserver) behave correctly.
//
// It returns the bytes copied from a to b and from b to a.
func Splice(a, b net.Conn) (atob, btoa int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		atob, _ = io.Copy(b, a)
		_ = CloseWrite(b)
	}()
	go func() {
		defer wg.Done()
		btoa, _ = io.Copy(a, b)
		_ = CloseWrite(a)
	}()
	wg.Wait()
	a.Close()
	b.Close()
	return atob, btoa
}
