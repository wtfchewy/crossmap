package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wtfchewy/crossmap/internal/wire"
)

// AgentRef names a Crossmap agent to connect to.
type AgentRef struct {
	// Name labels the agent in logs and status output.
	Name string
	// Addr is host:port, normally a tailnet name or address.
	Addr string
}

// Options configures a Host.
type Options struct {
	// SocketPath is where the usbmuxd-compatible socket is served. In
	// system mode this is forced to Apple's canonical path.
	SocketPath string
	// SystemMode takes over /var/run/usbmuxd so unmodified tools and Xcode
	// find Crossmap without an environment variable. Requires root.
	SystemMode bool
	// Agents are the remote machines to pull devices from.
	Agents []AgentRef
	// PSK authenticates to agents.
	PSK []byte
	// CoreDevice enables the Bonjour/RemotePairing mirror that makes remote
	// devices visible to Xcode and devicectl.
	CoreDevice bool
	// MirrorInterface is the local interface remote devices are advertised
	// on. Empty picks the first usable one.
	MirrorInterface string
	// TunnelWindow is how many ports above the CoreDevice port to relay.
	TunnelWindow int
	Logger       *slog.Logger
}

// Host is the Crossmap daemon on the machine that wants to use remote devices.
type Host struct {
	opts    Options
	log     *slog.Logger
	server  *Server
	remotes []*remoteSource

	mu      sync.Mutex
	mirrors map[string]*CoreDeviceMirror
}

// New builds a Host.
func New(opts Options) (*Host, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if len(opts.Agents) == 0 {
		return nil, errors.New("host: at least one agent is required")
	}
	if len(opts.PSK) == 0 {
		return nil, errors.New("host: a pre-shared key is required")
	}
	if opts.SocketPath == "" && !opts.SystemMode {
		return nil, errors.New("host: a socket path is required")
	}
	return &Host{opts: opts, log: opts.Logger, mirrors: make(map[string]*CoreDeviceMirror)}, nil
}

// Run serves until ctx is cancelled. In system mode it moves Apple's usbmuxd
// socket aside and restores it on the way out.
func (h *Host) Run(ctx context.Context) error {
	localSocket := ""
	socketPath := h.opts.SocketPath

	if h.opts.SystemMode {
		stashed, restore, err := TakeOver()
		if err != nil {
			return err
		}
		defer func() {
			if err := restore(); err != nil {
				h.log.Error("could not restore the system usbmuxd socket; run `crossmap repair`", "err", err)
			} else {
				h.log.Info("restored the system usbmuxd socket")
			}
		}()
		localSocket = stashed
		socketPath = SystemSocket
		h.log.Info("took over the system usbmuxd socket",
			"serving", SystemSocket, "apple_daemon_moved_to", stashed)
	}

	h.server = NewServer(localSocket, h.log)

	for _, ref := range h.opts.Agents {
		src := newRemoteSource(ref.Name, ref.Addr, h.opts.PSK)
		h.remotes = append(h.remotes, src)
		h.server.AddSource(src)
		go src.Run(ctx, h.log, h.server.Refresh)
	}

	if h.opts.CoreDevice {
		go h.manageMirrors(ctx)
	} else {
		h.log.Info("CoreDevice mirroring disabled; Xcode and devicectl will not see remote devices")
	}

	return h.server.Run(ctx, socketPath)
}

// Devices returns the merged device table.
func (h *Host) Devices() []wire.DeviceInfo {
	if h.server == nil {
		return nil
	}
	return h.server.Devices()
}

// manageMirrors keeps one CoreDevice mirror running per remote service.
func (h *Host) manageMirrors(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			h.stopAllMirrors()
			return
		case <-t.C:
			h.reconcileMirrors(ctx)
		}
	}
}

func (h *Host) reconcileMirrors(ctx context.Context) {
	want := make(map[string]struct {
		svc wire.Service
		src *remoteSource
	})
	for _, src := range h.remotes {
		for _, svc := range src.Services() {
			if len(svc.Addrs) == 0 || svc.Port == 0 {
				continue
			}
			key := src.Name() + "/" + svc.Name
			want[key] = struct {
				svc wire.Service
				src *remoteSource
			}{svc, src}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for key, m := range h.mirrors {
		if _, still := want[key]; !still {
			h.log.Info("stopping CoreDevice mirror", "service", key)
			m.Stop()
			delete(h.mirrors, key)
		}
	}
	for key, w := range want {
		if _, running := h.mirrors[key]; running {
			continue
		}
		m := NewCoreDeviceMirror(h.log, w.src, h.opts.MirrorInterface, h.opts.TunnelWindow)
		if err := m.Start(ctx, w.svc); err != nil {
			h.log.Warn("could not mirror CoreDevice service", "service", key, "err", err)
			continue
		}
		h.mirrors[key] = m
	}
}

func (h *Host) stopAllMirrors() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, m := range h.mirrors {
		m.Stop()
		delete(h.mirrors, key)
	}
}

// Status renders a human-readable snapshot for `crossmap status`.
func (h *Host) Status() string {
	var b []byte
	appendf := func(format string, args ...any) {
		b = append(b, []byte(fmt.Sprintf(format, args...))...)
	}
	appendf("agents:\n")
	for _, src := range h.remotes {
		info, err := src.Info()
		if err != nil {
			appendf("  %-16s %-24s unreachable: %v\n", src.Name(), src.addr, err)
			continue
		}
		appendf("  %-16s %-24s %s (%s)\n", src.Name(), src.addr, info.Hostname, info.Version)
	}
	appendf("devices:\n")
	for _, d := range h.Devices() {
		appendf("  %-42s id=%-6d via %s\n", d.Udid, d.DeviceID, d.Agent)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	appendf("coredevice mirrors:\n")
	for key, m := range h.mirrors {
		svc := m.Service()
		appendf("  %-32s %s:%d\n", key, m.bindIP, svc.Port)
	}
	return string(b)
}
