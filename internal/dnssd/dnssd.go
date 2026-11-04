// Package dnssd drives macOS's dns-sd(1) to browse, resolve and proxy-publish
// Bonjour services.
//
// Crossmap shells out rather than linking the DNSServiceDiscovery C API
// because the one capability it depends on most - proxy registration, where a
// Mac advertises a record on behalf of a host that is not on this link - is
// exposed cleanly by `dns-sd -P` and keeps the daemon free of cgo.
package dnssd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// RemotePairingService is the Bonjour service type CoreDevice uses to find
// iOS devices for wireless debugging. A USB-attached iPhone advertises it on
// its CDC-NCM ethernet interface, which is what lets Crossmap pick it up.
const RemotePairingService = "_remotepairing._tcp"

// BrowseResult is one service instance seen on one interface.
type BrowseResult struct {
	Instance  string
	Type      string
	Domain    string
	IfIndex   int
	Interface string
}

// LookupResult resolves an instance to the host and port behind it.
type LookupResult struct {
	Instance  string
	Host      string
	Port      int
	TXT       []string
	IfIndex   int
	Interface string
}

// Addr is one address a hostname resolves to.
type Addr struct {
	IP        string // may carry a %zone suffix for link-local addresses
	IfIndex   int
	Interface string
}

// runFor starts dns-sd, streams its output through scan until the command is
// killed, and returns whatever scan collected. dns-sd never exits on its own
// for browse-style queries, so every call is bounded by a window.
func runFor(ctx context.Context, window time.Duration, args []string, scan func(line string)) error {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	cmd := exec.CommandContext(ctx, "dns-sd", args...)
	// dns-sd ignores SIGTERM's default disposition in some macOS builds;
	// Kill guarantees the window is honoured.
	cmd.Cancel = func() error { return cmd.Process.Kill() }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("dnssd: start dns-sd: %w", err)
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		scan(sc.Text())
	}
	_, _ = io.Copy(io.Discard, stdout)
	_ = cmd.Wait()

	// A killed process is the expected outcome, not an error.
	if ctx.Err() == context.DeadlineExceeded {
		return nil
	}
	return ctx.Err()
}

// ifName maps an interface index to a name, returning "" when unknown.
func ifName(idx int) string {
	if idx <= 0 {
		return ""
	}
	if ifi, err := net.InterfaceByIndex(idx); err == nil {
		return ifi.Name
	}
	return ""
}

// unescape converts dns-sd's \032-style escapes back to literal characters.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
			n, err := strconv.Atoi(s[i+1 : i+4])
			if err == nil && n < 256 {
				b.WriteByte(byte(n))
				i += 4
				continue
			}
		}
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Browse lists instances of a service type seen within window.
//
// Output lines look like:
//
//	14:14:15.063  Add        3   1 local.  _remotepairing._tcp.  My iPhone
func Browse(ctx context.Context, serviceType string, window time.Duration) ([]BrowseResult, error) {
	seen := make(map[string]BrowseResult)
	err := runFor(ctx, window, []string{"-B", serviceType}, func(line string) {
		fields := strings.Fields(line)
		// timestamp, A/R, flags, if, domain, type, then the instance name,
		// which may contain spaces and so takes the rest of the line.
		if len(fields) < 7 || (fields[1] != "Add" && fields[1] != "Rmv") {
			return
		}
		idx, err := strconv.Atoi(fields[3])
		if err != nil {
			return
		}
		// Recover the instance name verbatim, spaces included.
		typePos := strings.Index(line, fields[5])
		if typePos < 0 {
			return
		}
		instance := strings.TrimSpace(line[typePos+len(fields[5]):])
		if instance == "" {
			return
		}
		key := fmt.Sprintf("%d/%s", idx, instance)
		if fields[1] == "Rmv" {
			delete(seen, key)
			return
		}
		seen[key] = BrowseResult{
			Instance:  instance,
			Type:      strings.TrimSuffix(fields[5], "."),
			Domain:    strings.TrimSuffix(fields[4], "."),
			IfIndex:   idx,
			Interface: ifName(idx),
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]BrowseResult, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	return out, nil
}

// Lookup resolves an instance to host, port and TXT.
//
// Output looks like:
//
//	14:14:18.079  My\032iPhone._remotepairing._tcp.local. can be reached at iPhone.local.:49152 (interface 15)
//	 key=value key2=value2
func Lookup(ctx context.Context, instance, serviceType, domain string, window time.Duration) (*LookupResult, error) {
	if domain == "" {
		domain = "local"
	}
	var res *LookupResult
	var pendingTXT bool

	err := runFor(ctx, window, []string{"-L", instance, serviceType, domain}, func(line string) {
		if idx := strings.Index(line, " can be reached at "); idx >= 0 {
			rest := line[idx+len(" can be reached at "):]
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return
			}
			hostPort := fields[0]
			colon := strings.LastIndex(hostPort, ":")
			if colon < 0 {
				return
			}
			port, err := strconv.Atoi(hostPort[colon+1:])
			if err != nil {
				return
			}
			host := strings.TrimSuffix(hostPort[:colon], ".")
			ifIdx := 0
			if i := strings.Index(rest, "(interface "); i >= 0 {
				fmt.Sscanf(rest[i:], "(interface %d)", &ifIdx)
			}
			res = &LookupResult{
				Instance:  unescape(instance),
				Host:      host,
				Port:      port,
				IfIndex:   ifIdx,
				Interface: ifName(ifIdx),
			}
			pendingTXT = true
			return
		}
		// TXT records follow the reachability line, indented.
		if pendingTXT && strings.HasPrefix(line, " ") && res != nil {
			txt := strings.Fields(strings.TrimSpace(line))
			if len(txt) > 0 && len(res.TXT) == 0 {
				res.TXT = txt
			}
			pendingTXT = false
		}
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, fmt.Errorf("dnssd: %q did not resolve within %s", instance, window)
	}
	return res, nil
}

// GetAddrs resolves a .local hostname to addresses.
//
// Output looks like:
//
//	14:14:21.093  Add  40000003  15  iPhone.local.  FE80:0000:...:EF1F%en5  4500
func GetAddrs(ctx context.Context, hostname string, window time.Duration) ([]Addr, error) {
	seen := make(map[string]Addr)
	err := runFor(ctx, window, []string{"-G", "v4v6", hostname}, func(line string) {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[1] != "Add" {
			return
		}
		idx, err := strconv.Atoi(fields[3])
		if err != nil {
			return
		}
		raw := fields[5]
		// dns-sd writes an unknown zone as "%<0>"; drop it so the address
		// parses, and attach the real interface name when we know it.
		if i := strings.Index(raw, "%"); i >= 0 {
			zone := raw[i+1:]
			raw = raw[:i]
			if zone == "<0>" || zone == "" {
				zone = ifName(idx)
			}
			if ip := net.ParseIP(raw); ip != nil && ip.IsLinkLocalUnicast() && zone != "" {
				raw = raw + "%" + zone
			}
		}
		if ip := net.ParseIP(strings.Split(raw, "%")[0]); ip == nil || ip.IsUnspecified() {
			return
		}
		seen[raw] = Addr{IP: raw, IfIndex: idx, Interface: ifName(idx)}
	})
	if err != nil {
		return nil, err
	}
	out := make([]Addr, 0, len(seen))
	for _, a := range seen {
		out = append(out, a)
	}
	return out, nil
}
