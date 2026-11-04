package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wtfchewy/crossmap/internal/config"
	"github.com/wtfchewy/crossmap/internal/dnssd"
	"github.com/wtfchewy/crossmap/internal/host"
	"github.com/wtfchewy/crossmap/internal/usbmux"
)

// check is one line of doctor output.
type check struct {
	name   string
	ok     bool
	detail string
	// warn marks a condition that is informational rather than a failure.
	warn bool
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	scan := fs.Bool("scan", false, "also browse for CoreDevice services (takes a few seconds)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var checks []check
	add := func(c check) { checks = append(checks, c) }

	// --- usbmuxd ---
	if fi, err := os.Stat(usbmux.DefaultSocket); err != nil {
		add(check{"usbmuxd socket", false, usbmux.DefaultSocket + " is missing", false})
	} else if fi.Mode()&os.ModeSocket == 0 {
		add(check{"usbmuxd socket", false, usbmux.DefaultSocket + " is not a socket", false})
	} else if c, err := net.DialTimeout("unix", usbmux.DefaultSocket, 2*time.Second); err != nil {
		add(check{"usbmuxd socket", false, "nothing is accepting on " + usbmux.DefaultSocket, false})
	} else {
		c.Close()
		add(check{"usbmuxd socket", true, usbmux.DefaultSocket, false})
	}

	if state, active := host.ActiveTakeover(); active {
		add(check{"usbmuxd takeover", false,
			fmt.Sprintf("a Crossmap takeover from pid %d is recorded; run `sudo crossmap repair` if no host is running", state.PID), true})
	} else {
		add(check{"usbmuxd takeover", true, "none active", false})
	}

	// --- devices visible to this machine ---
	cli := usbmux.NewClient("")
	if devs, err := cli.ListDevices(ctx); err != nil {
		add(check{"attached devices", false, err.Error(), false})
	} else if len(devs) == 0 {
		add(check{"attached devices", true, "none attached to this Mac", false})
	} else {
		var names []string
		for _, d := range devs {
			names = append(names, fmt.Sprintf("%s (%s)", d.Properties.Udid(), d.Properties.ConnectionType))
		}
		add(check{"attached devices", true, strings.Join(names, ", "), false})
	}

	// --- iOS USB network interfaces ---
	if ifaces := iosUSBPorts(); len(ifaces) > 0 {
		add(check{"iOS USB interface", true, strings.Join(ifaces, ", "), false})
	} else {
		add(check{"iOS USB interface", true, "none (expected when no iPhone is plugged in here)", false})
	}

	// --- tailnet ---
	if ip, err := tailnetIP(); err != nil {
		add(check{"tailscale", false, err.Error(), false})
	} else {
		add(check{"tailscale", true, ip, false})
	}

	// --- shared key ---
	if _, err := config.LoadPSK(""); err != nil {
		add(check{"crossmap key", false, err.Error(), false})
	} else {
		path, _ := config.PSKPath()
		add(check{"crossmap key", true, path, false})
	}

	// --- tooling ---
	for _, tool := range []string{"dns-sd", "ideviceinstaller", "idevice_id"} {
		if p, err := exec.LookPath(tool); err == nil {
			add(check{tool, true, p, false})
		} else {
			detail := "not installed"
			if tool == "ideviceinstaller" {
				detail = "not installed (brew install ideviceinstaller) - needed to install .ipa files"
			}
			add(check{tool, tool == "ideviceinstaller", detail, tool == "ideviceinstaller"})
		}
	}
	if out, err := exec.Command("xcrun", "devicectl", "--version").Output(); err == nil {
		add(check{"devicectl", true, strings.TrimSpace(string(out)), false})
	} else {
		add(check{"devicectl", false, "not available (install Xcode command line tools)", false})
	}

	// --- bonjour scan ---
	if *scan {
		svcs, err := dnssd.Browse(ctx, dnssd.RemotePairingService, 3*time.Second)
		if err != nil {
			add(check{"coredevice scan", false, err.Error(), false})
		} else if len(svcs) == 0 {
			add(check{"coredevice scan", true, "no _remotepairing._tcp advertisements visible", false})
		} else {
			var names []string
			for _, s := range svcs {
				names = append(names, fmt.Sprintf("%q on %s", s.Instance, s.Interface))
			}
			add(check{"coredevice scan", true, strings.Join(names, ", "), false})
		}
	}

	// --- render ---
	failed := 0
	fmt.Println()
	for _, c := range checks {
		mark := "ok  "
		if !c.ok {
			mark = "FAIL"
			failed++
		} else if c.warn {
			mark = "warn"
		}
		fmt.Printf("  [%s] %-20s %s\n", mark, c.name, c.detail)
	}
	fmt.Println()
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

// iosUSBPorts lists network interfaces macOS attributes to a connected iOS
// device.
func iosUSBPorts() []string {
	body, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return nil
	}
	var out []string
	blocks := strings.Split(string(body), "Hardware Port: ")
	for _, b := range blocks[1:] {
		lines := strings.SplitN(b, "\n", 3)
		if len(lines) < 2 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(lines[0]))
		if !strings.Contains(name, "iphone") && !strings.Contains(name, "ipad") {
			continue
		}
		dev := strings.TrimSpace(strings.TrimPrefix(lines[1], "Device:"))
		out = append(out, fmt.Sprintf("%s (%s)", dev, strings.TrimSpace(lines[0])))
	}
	return out
}
