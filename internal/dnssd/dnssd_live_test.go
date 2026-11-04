package dnssd

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestBrowseLookupResolveLive exercises the dns-sd parsers against whatever
// Bonjour services this machine can see. It uses _companion-link._tcp, which
// every Mac advertises about itself, so it does not need an iOS device.
func TestBrowseLookupResolveLive(t *testing.T) {
	if _, err := exec.LookPath("dns-sd"); err != nil {
		t.Skip("dns-sd unavailable")
	}
	ctx := context.Background()

	results, err := Browse(ctx, "_companion-link._tcp", 3*time.Second)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(results) == 0 {
		t.Skip("no _companion-link._tcp instances visible on this network")
	}
	for _, r := range results {
		t.Logf("browse: instance=%q type=%s domain=%s if=%d(%s)",
			r.Instance, r.Type, r.Domain, r.IfIndex, r.Interface)
		if r.Instance == "" || r.Type == "" {
			t.Errorf("browse result missing fields: %+v", r)
		}
	}

	lr, err := Lookup(ctx, results[0].Instance, "_companion-link._tcp", "local", 3*time.Second)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", results[0].Instance, err)
	}
	t.Logf("lookup: host=%s port=%d if=%d(%s) txt=%d keys",
		lr.Host, lr.Port, lr.IfIndex, lr.Interface, len(lr.TXT))
	if lr.Host == "" || lr.Port == 0 {
		t.Fatalf("lookup returned no host/port: %+v", lr)
	}
	if len(lr.TXT) == 0 {
		t.Error("expected TXT records on a _companion-link._tcp instance")
	}

	addrs, err := GetAddrs(ctx, lr.Host, 3*time.Second)
	if err != nil {
		t.Fatalf("GetAddrs(%s): %v", lr.Host, err)
	}
	if len(addrs) == 0 {
		t.Fatalf("no addresses for %s", lr.Host)
	}
	for _, a := range addrs {
		t.Logf("addr: %s if=%d(%s)", a.IP, a.IfIndex, a.Interface)
	}
}

func TestUnescape(t *testing.T) {
	cases := map[string]string{
		`Wes'\032Macbook\032Pro`: "Wes' Macbook Pro",
		`plain`:                  "plain",
		`a\.b`:                   "a.b",
		`My\032iPhone\03214`:     "My iPhone 14",
	}
	for in, want := range cases {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}
