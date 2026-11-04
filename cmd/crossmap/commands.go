package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/wtfchewy/crossmap/internal/agent"
	"github.com/wtfchewy/crossmap/internal/config"
	"github.com/wtfchewy/crossmap/internal/host"
	"github.com/wtfchewy/crossmap/internal/usbmux"
)

// stringList collects a repeated flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	pskFile := fs.String("psk-file", "", "where to write the key (default ~/.config/crossmap/psk)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := config.GeneratePSK(*pskFile)
	if err != nil {
		return err
	}
	path, _ := config.PSKPath()
	if *pskFile != "" {
		path = *pskFile
	}
	fmt.Printf("Wrote a new Crossmap key to %s\n\n", path)
	fmt.Printf("  %s\n\n", key)
	fmt.Printf("Run this on the other Mac to pair them:\n\n  crossmap join %s\n", key)
	return nil
}

func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	pskFile := fs.String("psk-file", "", "where to write the key (default ~/.config/crossmap/psk)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: crossmap join KEY")
	}
	if err := config.WritePSK(*pskFile, fs.Arg(0)); err != nil {
		return err
	}
	path, _ := config.PSKPath()
	if *pskFile != "" {
		path = *pskFile
	}
	fmt.Printf("Stored the Crossmap key in %s\n", path)
	return nil
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	listen := fs.String("listen", "", "address to serve on (default: this machine's tailnet address)")
	pskFile := fs.String("psk-file", "", "key file (default ~/.config/crossmap/psk)")
	socket := fs.String("usbmuxd", "", "usbmuxd socket to proxy (default "+usbmux.DefaultSocket+")")
	coredevice := fs.Bool("coredevice", true, "advertise attached devices' CoreDevice service to hosts")
	usbOnly := fs.Bool("usb-only", true, "only expose devices attached over USB, not others on the LAN")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*verbose)
	psk, err := config.LoadPSK(*pskFile)
	if err != nil {
		return err
	}

	addr := *listen
	if addr == "" {
		ip, err := tailnetIP()
		if err != nil {
			return fmt.Errorf("%w (pass --listen to choose an address explicitly)", err)
		}
		addr = net.JoinHostPort(ip, DefaultPort)
		log.Info("binding to the tailnet address", "addr", addr)
	}

	cfg := agent.Config{
		Listen:        addr,
		PSK:           psk,
		UsbmuxdSocket: *socket,
		Version:       version,
		Logger:        log,
	}

	ctx, cancel := signalContext()
	defer cancel()

	if *coredevice {
		disc := agent.NewMDNSDiscovery(log, *usbOnly)
		go disc.Run(ctx)
		cfg.Discovery = disc
	}

	a, err := agent.New(cfg)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func runHost(args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	var agents stringList
	fs.Var(&agents, "agent", "agent to connect to as name=host:port (repeatable)")
	pskFile := fs.String("psk-file", "", "key file (default ~/.config/crossmap/psk)")
	system := fs.Bool("system", false, "take over /var/run/usbmuxd so Xcode and all tools see remote devices (needs sudo)")
	socket := fs.String("socket", "", "socket path when not in system mode (default ~/.config/crossmap/usbmuxd.sock)")
	coredevice := fs.Bool("coredevice", true, "mirror the CoreDevice service so Xcode and devicectl see remote devices")
	iface := fs.String("interface", "", "local interface to advertise remote devices on (default: first usable)")
	window := fs.Int("tunnel-window", host.DefaultTunnelWindow, "ports above the CoreDevice port to relay")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*verbose)
	psk, err := config.LoadPSK(*pskFile)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return errors.New("at least one --agent is required, e.g. --agent my-other-mac")
	}

	var refs []host.AgentRef
	for _, a := range agents {
		name, addr, err := parseAgentRef(a)
		if err != nil {
			return err
		}
		refs = append(refs, host.AgentRef{Name: name, Addr: addr})
	}

	socketPath := *socket
	if socketPath == "" && !*system {
		socketPath, err = config.DefaultSocketPath()
		if err != nil {
			return err
		}
	}

	h, err := host.New(host.Options{
		SocketPath:      socketPath,
		SystemMode:      *system,
		Agents:          refs,
		PSK:             psk,
		CoreDevice:      *coredevice,
		MirrorInterface: *iface,
		TunnelWindow:    *window,
		Logger:          log,
	})
	if err != nil {
		return err
	}

	if !*system {
		// libusbmuxd only treats the value as a socket path when it is
		// prefixed with UNIX:; without it the path is parsed as host:port.
		fmt.Fprintf(os.Stderr, "\nPoint libimobiledevice tools at Crossmap with:\n\n    export USBMUXD_SOCKET_ADDRESS=UNIX:%s\n\n", socketPath)
		fmt.Fprintf(os.Stderr, "Xcode and devicectl do not read that variable. For them, run with --system.\n\n")
	}

	ctx, cancel := signalContext()
	defer cancel()
	return h.Run(ctx)
}

func runRepair(args []string) error {
	fs := flag.NewFlagSet("repair", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	state, active := host.ActiveTakeover()
	if !active {
		if _, err := os.Stat(host.StashedSocket); err != nil {
			fmt.Println("Nothing to repair: no Crossmap takeover is recorded.")
			return nil
		}
		fmt.Printf("Found a stashed socket at %s with no state file; restoring anyway.\n", host.StashedSocket)
	} else {
		fmt.Printf("Found a takeover started by pid %d at %s.\n", state.PID, state.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if os.Geteuid() != 0 {
		return errors.New("repair requires root (re-run with sudo)")
	}
	if err := host.Restore(); err != nil {
		return err
	}
	fmt.Printf("Restored %s. Restart Xcode and any running iOS tools.\n", host.SystemSocket)
	return nil
}

// tailnetIP returns this machine's Tailscale IPv4 address.
func tailnetIP() (string, error) {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return "", errors.New("could not get this machine's tailnet address from `tailscale ip -4`")
	}
	ip := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("`tailscale ip -4` returned %q, which is not an address", ip)
	}
	return ip, nil
}
