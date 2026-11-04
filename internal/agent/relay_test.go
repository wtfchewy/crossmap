package agent

import (
	"io"
	"log/slog"
	"testing"

	"github.com/wtfchewy/crossmap/internal/wire"
)

// stubDiscovery reports a fixed service set.
type stubDiscovery struct{ svcs []wire.Service }

func (s stubDiscovery) Services() []wire.Service { return s.svcs }

func testAgent(svcs []wire.Service) *Agent {
	return &Agent{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: Config{Discovery: stubDiscovery{svcs: svcs}},
	}
}

// The allowlist is the only thing stopping an authenticated host from using
// the agent as a general proxy into the agent's network, so it gets explicit
// coverage for both what it permits and what it must refuse.
func TestAllowRelay(t *testing.T) {
	svcs := []wire.Service{{
		Name:  "Wes iPhone",
		Port:  49152,
		Addrs: []string{"fe80::1cbd:4bff:fe12:3456%en7", "169.254.12.9"},
	}}
	a := testAgent(svcs)

	allowed := []struct{ network, addr string }{
		{"tcp", "[fe80::1cbd:4bff:fe12:3456%en7]:49152"},
		{"udp", "[fe80::1cbd:4bff:fe12:3456%en7]:49152"},
		// The trusted tunnel lands just above the advertised port.
		{"tcp", "[fe80::1cbd:4bff:fe12:3456%en7]:49180"},
		{"tcp", "[fe80::1cbd:4bff:fe12:3456%en7]:49216"},
		{"tcp", "169.254.12.9:49152"},
		// Zone suffixes are host-local notation, not identity.
		{"tcp", "[fe80::1cbd:4bff:fe12:3456]:49152"},
	}
	for _, c := range allowed {
		if err := a.allowRelay(c.network, c.addr); err != nil {
			t.Errorf("allowRelay(%s, %s) = %v, want allowed", c.network, c.addr, err)
		}
	}

	refused := []struct {
		network, addr, why string
	}{
		{"tcp", "10.0.0.5:22", "a host that was never discovered"},
		{"tcp", "127.0.0.1:49152", "loopback on the agent itself"},
		{"tcp", "169.254.12.9:22", "a discovered host but an unrelated port"},
		{"tcp", "169.254.12.9:49217", "one port past the tunnel window"},
		{"tcp", "169.254.12.9:443", "a discovered host on a low port"},
		{"sctp", "169.254.12.9:49152", "an unsupported network"},
		{"tcp", "not-an-address", "a malformed address"},
	}
	for _, c := range refused {
		if err := a.allowRelay(c.network, c.addr); err == nil {
			t.Errorf("allowRelay(%s, %s) was allowed; it must be refused (%s)", c.network, c.addr, c.why)
		}
	}
}

// With no discovery configured the agent has no basis for trusting any
// destination, so relaying must be refused outright rather than defaulting to
// permissive.
func TestAllowRelayRefusesWithoutDiscovery(t *testing.T) {
	a := &Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.allowRelay("tcp", "169.254.12.9:49152"); err == nil {
		t.Fatal("relaying was permitted with no discovery configured")
	}
}

func TestSameHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"fe80::1%en7", "fe80::1", true},
		{"fe80::1%en7", "fe80::1%en9", true}, // zone is agent-local
		{"[fe80::1]", "fe80::1", true},
		{"FE80::1", "fe80::0001", true},
		{"169.254.12.9", "169.254.12.9", true},
		{"169.254.12.9", "169.254.12.10", false},
		{"fe80::1", "fe80::2", false},
	}
	for _, c := range cases {
		if got := sameHost(c.a, c.b); got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
