package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wtfchewy/crossmap/internal/link"
	"github.com/wtfchewy/crossmap/internal/wire"
)

// DefaultTunnelWindow is how many ports above the advertised CoreDevice port
// Crossmap mirrors. After the RemotePairing handshake the device opens the
// trusted tunnel on a nearby port and remotepairingd connects to it at the
// same address, so those ports must be relayed too.
const DefaultTunnelWindow = 64

// CoreDeviceMirror makes one remote device's CoreDevice front door look local.
//
// Two things have to be true for Xcode and devicectl to use a device:
// remotepairingd must discover a _remotepairing._tcp advertisement on an
// interface it considers local, and it must be able to open connections to the
// address in that advertisement. The mirror supplies both - a proxy Bonjour
// record pointing at this Mac, and port relays that carry the traffic to the
// agent and on to the device.
//
// Crossmap never terminates or inspects the connection. CoreDevice's
// certificate-based pairing runs end to end between this Mac's remotepairingd
// and the device, exactly as it would over a cable.
type CoreDeviceMirror struct {
	log     *slog.Logger
	source  *remoteSource
	bindIP  net.IP
	bindIf  string
	window  int
	service wire.Service

	mu        sync.Mutex
	publisher *exec.Cmd
	listeners []io_Closer
	running   bool
}

// io_Closer avoids importing io for a single interface in a file that is
// otherwise net-only.
type io_Closer interface{ Close() error }

// NewCoreDeviceMirror prepares a mirror. bindInterface selects which local
// interface the device is advertised on; remotepairingd scopes its outbound
// connection to the interface it made the discovery on, so this must be a
// real, up interface rather than loopback.
func NewCoreDeviceMirror(log *slog.Logger, src *remoteSource, bindInterface string, window int) *CoreDeviceMirror {
	if log == nil {
		log = slog.Default()
	}
	if window <= 0 {
		window = DefaultTunnelWindow
	}
	return &CoreDeviceMirror{log: log, source: src, bindIf: bindInterface, window: window}
}

// BindAddress resolves the interface to advertise on into an IPv4 address.
func BindAddress(ifaceName string) (net.IP, string, error) {
	candidates := []string{ifaceName}
	if ifaceName == "" {
		// Prefer Wi-Fi, then wired, matching where a Mac normally has its
		// primary address.
		candidates = []string{"en0", "en1", "en2", "en3"}
	}
	for _, name := range candidates {
		ifi, err := net.InterfaceByName(name)
		if err != nil || ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4, name, nil
		}
	}
	if ifaceName != "" {
		return nil, "", fmt.Errorf("host: interface %s has no usable IPv4 address", ifaceName)
	}
	return nil, "", fmt.Errorf("host: no interface with a usable IPv4 address (tried %s)", strings.Join(candidates, ", "))
}

// Start brings up the relays and the proxy advertisement for svc.
func (m *CoreDeviceMirror) Start(ctx context.Context, svc wire.Service) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil
	}

	ip, ifname, err := BindAddress(m.bindIf)
	if err != nil {
		return err
	}
	m.bindIP, m.bindIf, m.service = ip, ifname, svc

	target := svc.Addrs[0]
	// Relay the front door plus the window the trusted tunnel lands in.
	for port := svc.Port; port <= svc.Port+m.window; port++ {
		remote := joinHostPort(target, port)
		tcpLn, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err != nil {
			m.stopLocked()
			return fmt.Errorf("host: bind tcp %s:%d: %w", ip, port, err)
		}
		m.listeners = append(m.listeners, tcpLn)
		go m.serveTCP(ctx, tcpLn, remote)

		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: port})
		if err != nil {
			m.stopLocked()
			return fmt.Errorf("host: bind udp %s:%d: %w", ip, port, err)
		}
		m.listeners = append(m.listeners, udpConn)
		go m.serveUDP(ctx, udpConn, remote)
	}

	if err := m.publishLocked(ctx, svc, ip); err != nil {
		m.stopLocked()
		return err
	}
	m.running = true
	m.log.Info("mirroring CoreDevice service",
		"name", svc.Name, "advertised_on", ifname, "address", ip.String(),
		"ports", fmt.Sprintf("%d-%d", svc.Port, svc.Port+m.window),
		"device_address", target, "agent", m.source.Name())
	return nil
}

// Stop tears the mirror down.
func (m *CoreDeviceMirror) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *CoreDeviceMirror) stopLocked() {
	if m.publisher != nil && m.publisher.Process != nil {
		_ = m.publisher.Process.Kill()
		_ = m.publisher.Wait()
		m.publisher = nil
	}
	for _, l := range m.listeners {
		_ = l.Close()
	}
	m.listeners = nil
	m.running = false
}

// Running reports whether the mirror is live.
func (m *CoreDeviceMirror) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Service returns the mirrored service.
func (m *CoreDeviceMirror) Service() wire.Service {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.service
}

// publishLocked starts a `dns-sd -P` proxy registration. Proxy mode exists for
// exactly this case: advertising a service on behalf of a host that is not
// itself on this link.
func (m *CoreDeviceMirror) publishLocked(ctx context.Context, svc wire.Service, ip net.IP) error {
	// The SRV target must not collide with the device's own name if the
	// device is ever on this LAN too.
	proxyHost := "crossmap-" + sanitizeHostLabel(svc.Name)

	args := []string{
		"-P",
		svc.Name,
		svc.Type,
		orDefault(svc.Domain, "local"),
		strconv.Itoa(svc.Port),
		proxyHost,
		ip.String(),
	}
	args = append(args, svc.TXT...)

	cmd := exec.Command("dns-sd", args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("host: start dns-sd proxy registration: %w", err)
	}
	m.publisher = cmd

	// dns-sd exits immediately on a malformed registration; catch that
	// rather than reporting success for an advertisement that is not live.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		m.publisher = nil
		return fmt.Errorf("host: dns-sd proxy registration exited immediately: %w", err)
	case <-time.After(750 * time.Millisecond):
	}

	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	return nil
}

func (m *CoreDeviceMirror) serveTCP(ctx context.Context, ln net.Listener, remote string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			stream, err := m.source.RelayTCP(remote)
			if err != nil {
				m.log.Warn("coredevice relay failed", "remote", remote, "err", err)
				conn.Close()
				return
			}
			m.log.Debug("coredevice tcp relay", "remote", remote)
			link.Splice(conn, stream)
		}()
	}
}

// serveUDP bridges a local UDP socket to the device.
//
// The trusted tunnel is QUIC, so datagram boundaries matter. Each distinct
// local peer gets its own relay stream, and replies are written back to that
// peer's address.
func (m *CoreDeviceMirror) serveUDP(ctx context.Context, conn *net.UDPConn, remote string) {
	type session struct {
		stream net.Conn
		mu     sync.Mutex
	}
	sessions := make(map[string]*session)
	var mu sync.Mutex
	buf := make([]byte, 65535)

	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			mu.Lock()
			for _, s := range sessions {
				s.stream.Close()
			}
			mu.Unlock()
			return
		}
		key := peer.String()

		mu.Lock()
		sess, ok := sessions[key]
		if !ok {
			stream, err := m.source.RelayUDP(remote)
			if err != nil {
				mu.Unlock()
				m.log.Warn("coredevice udp relay failed", "remote", remote, "err", err)
				continue
			}
			sess = &session{stream: stream}
			sessions[key] = sess
			go func(p *net.UDPAddr, s *session) {
				defer func() {
					mu.Lock()
					delete(sessions, p.String())
					mu.Unlock()
					s.stream.Close()
				}()
				link.ReadDatagrams(s.stream, func(b []byte) bool {
					_, err := conn.WriteToUDP(b, p)
					return err == nil
				})
			}(peer, sess)
		}
		mu.Unlock()

		sess.mu.Lock()
		err = link.WriteDatagram(sess.stream, buf[:n])
		sess.mu.Unlock()
		if err != nil {
			mu.Lock()
			delete(sessions, key)
			mu.Unlock()
			sess.stream.Close()
		}
	}
}

// joinHostPort builds an address, preserving an IPv6 zone suffix.
func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var hostLabelRE = regexp.MustCompile(`[^A-Za-z0-9-]+`)

// sanitizeHostLabel turns a Bonjour instance name into a legal DNS label.
func sanitizeHostLabel(s string) string {
	s = hostLabelRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		s = "device"
	}
	return strings.ToLower(s)
}
