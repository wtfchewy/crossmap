package agent

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/wtfchewy/crossmap/internal/link"
)

// tunnelPortWindow is how far above a service's advertised port the agent will
// relay. After the RemotePairing handshake the device allocates a fresh
// listener for the trusted tunnel on a nearby port, so the window has to cover
// it without turning the agent into a general-purpose proxy.
const tunnelPortWindow = 64

// allowRelay decides whether the agent will carry traffic to addr.
//
// Relaying is deliberately narrow: the destination host must be an address the
// agent itself discovered on an attached device, and the port must be that
// device's advertised CoreDevice port or sit inside the tunnel window above
// it. Without this an authenticated host could use the agent to reach anything
// the agent's machine can reach.
func (a *Agent) allowRelay(network, addr string) error {
	switch network {
	case "tcp", "udp":
	default:
		return fmt.Errorf("agent: unsupported relay network %q", network)
	}
	if a.cfg.Discovery == nil {
		return fmt.Errorf("agent: relaying is disabled (no device discovery configured)")
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("agent: malformed relay address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("agent: malformed relay port in %q", addr)
	}

	for _, svc := range a.cfg.Discovery.Services() {
		for _, known := range svc.Addrs {
			if !sameHost(known, host) {
				continue
			}
			if port >= svc.Port && port <= svc.Port+tunnelPortWindow {
				return nil
			}
		}
	}
	return fmt.Errorf("agent: relay to %s is not permitted", addr)
}

// sameHost compares two address literals, tolerating differences in IPv6 zone
// notation and bracket form.
func sameHost(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
		if ip, zone, ok := strings.Cut(s, "%"); ok {
			// Zones are interface names, which are agent-local; compare the
			// address and keep the zone only as a tiebreaker.
			_ = zone
			s = ip
		}
		if ip := net.ParseIP(s); ip != nil {
			return ip.String()
		}
		return strings.ToLower(s)
	}
	return norm(a) == norm(b)
}

func (a *Agent) handleRelay(ctx context.Context, req link.Request, st *link.Stream) {
	if err := a.allowRelay(req.Network, req.Address); err != nil {
		a.log.Warn("relay refused", "network", req.Network, "address", req.Address, "err", err)
		st.Fail(err)
		return
	}
	if req.Network == "udp" {
		a.relayUDP(ctx, req, st)
		return
	}
	a.relayTCP(ctx, req, st)
}

func (a *Agent) relayTCP(ctx context.Context, req link.Request, st *link.Stream) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", req.Address)
	if err != nil {
		st.Fail(fmt.Errorf("agent: relay dial %s: %w", req.Address, err))
		return
	}
	if err := st.Reply(link.Response{}); err != nil {
		conn.Close()
		st.Close()
		return
	}
	a.log.Debug("relay open", "network", "tcp", "address", req.Address)
	link.Splice(st, conn)
}

// relayUDP carries datagrams over the stream with a 2-byte big-endian length
// prefix, preserving message boundaries that a raw byte stream would destroy.
// QUIC, which the CoreDevice trusted tunnel runs on, does not tolerate
// coalesced or split datagrams.
func (a *Agent) relayUDP(ctx context.Context, req link.Request, st *link.Stream) {
	raddr, err := net.ResolveUDPAddr("udp", req.Address)
	if err != nil {
		st.Fail(fmt.Errorf("agent: resolve %s: %w", req.Address, err))
		return
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		st.Fail(fmt.Errorf("agent: relay dial udp %s: %w", req.Address, err))
		return
	}
	if err := st.Reply(link.Response{}); err != nil {
		conn.Close()
		st.Close()
		return
	}
	a.log.Debug("relay open", "network", "udp", "address", req.Address)
	go func() {
		<-ctx.Done()
		st.Close()
		conn.Close()
	}()
	link.PumpDatagrams(st, conn)
}
