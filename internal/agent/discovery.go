package agent

import (
	"context"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wtfchewy/crossmap/internal/dnssd"
	"github.com/wtfchewy/crossmap/internal/wire"
)

// Discovery windows. Browsing is cheap but not instant; these values favour
// noticing a freshly plugged device within a few seconds over minimising
// dns-sd invocations.
const (
	browseWindow  = 2 * time.Second
	resolveWindow = 2 * time.Second
	scanInterval  = 8 * time.Second
)

// MDNSDiscovery finds the RemotePairing advertisements of USB-attached iOS
// devices.
//
// An iPhone plugged into a Mac brings up a CDC-NCM ethernet interface and
// advertises _remotepairing._tcp on it - the same service it would advertise
// over Wi-Fi for wireless debugging. Capturing that advertisement here is what
// lets the host republish it and have Xcode treat the device as local.
type MDNSDiscovery struct {
	log *slog.Logger

	mu       sync.RWMutex
	services []wire.Service

	// usbOnly restricts results to interfaces macOS identifies as an iOS
	// device's USB ethernet port.
	usbOnly bool
}

// NewMDNSDiscovery returns a discovery that scans until its context ends.
// When usbOnly is true, only services seen on an "iPhone USB"-style hardware
// port are reported, which keeps other people's phones on the office Wi-Fi out
// of the results.
func NewMDNSDiscovery(log *slog.Logger, usbOnly bool) *MDNSDiscovery {
	if log == nil {
		log = slog.Default()
	}
	return &MDNSDiscovery{log: log, usbOnly: usbOnly}
}

// Services implements Discovery.
func (d *MDNSDiscovery) Services() []wire.Service {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]wire.Service(nil), d.services...)
}

// Run scans until ctx is cancelled.
func (d *MDNSDiscovery) Run(ctx context.Context) {
	t := time.NewTicker(scanInterval)
	defer t.Stop()
	d.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.scan(ctx)
		}
	}
}

func (d *MDNSDiscovery) scan(ctx context.Context) {
	found, err := dnssd.Browse(ctx, dnssd.RemotePairingService, browseWindow)
	if err != nil {
		d.log.Debug("remotepairing browse failed", "err", err)
		return
	}

	usbIfaces := map[string]bool{}
	if d.usbOnly {
		usbIfaces = iosUSBInterfaces()
	}

	var services []wire.Service
	seen := map[string]bool{}
	for _, b := range found {
		if d.usbOnly && !usbIfaces[b.Interface] {
			continue
		}
		// The same instance shows up once per interface; one entry each.
		if seen[b.Instance] {
			continue
		}

		lr, err := dnssd.Lookup(ctx, b.Instance, b.Type, b.Domain, resolveWindow)
		if err != nil {
			d.log.Debug("remotepairing lookup failed", "instance", b.Instance, "err", err)
			continue
		}
		addrs, err := dnssd.GetAddrs(ctx, lr.Host, resolveWindow)
		if err != nil || len(addrs) == 0 {
			d.log.Debug("remotepairing address lookup failed", "host", lr.Host, "err", err)
			continue
		}

		svc := wire.Service{
			Name:      b.Instance,
			Type:      b.Type,
			Domain:    b.Domain,
			Host:      lr.Host,
			Port:      lr.Port,
			TXT:       lr.TXT,
			Interface: b.Interface,
		}
		for _, a := range addrs {
			// Only addresses on the interface the service was seen on are
			// usable; the device's other addresses belong to networks this
			// machine may not share with it.
			if a.Interface != "" && b.Interface != "" && a.Interface != b.Interface {
				continue
			}
			svc.Addrs = append(svc.Addrs, a.IP)
		}
		if len(svc.Addrs) == 0 {
			continue
		}
		seen[b.Instance] = true
		services = append(services, svc)
	}

	d.mu.Lock()
	changed := len(services) != len(d.services)
	d.services = services
	d.mu.Unlock()
	if changed {
		for _, s := range services {
			d.log.Info("discovered CoreDevice service",
				"name", s.Name, "port", s.Port, "iface", s.Interface, "addrs", s.Addrs)
		}
	}
}

// hardwarePort matches a `networksetup -listallhardwareports` stanza.
var hardwarePortRE = regexp.MustCompile(`(?m)^Hardware Port: (.+)\nDevice: (\S+)`)

// iosUSBInterfaces returns the set of interface names macOS attributes to a
// connected iOS device, e.g. {"en7": true} for a hardware port named
// "iPhone USB".
func iosUSBInterfaces() map[string]bool {
	out := map[string]bool{}
	body, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return out
	}
	for _, m := range hardwarePortRE.FindAllStringSubmatch(string(body), -1) {
		name := strings.ToLower(m[1])
		if strings.Contains(name, "iphone") || strings.Contains(name, "ipad") {
			out[m[2]] = true
		}
	}
	return out
}
