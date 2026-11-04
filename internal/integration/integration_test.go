// Package integration exercises a complete Crossmap deployment in one
// process: a fake usbmuxd with a synthetic device, an agent in front of it, a
// host linked to that agent, and a real usbmuxd client driving the host.
//
// This is the test that proves the plumbing without a physical iPhone.
package integration

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wtfchewy/crossmap/internal/agent"
	"github.com/wtfchewy/crossmap/internal/host"
	"github.com/wtfchewy/crossmap/internal/usbmux"
	"github.com/wtfchewy/crossmap/internal/usbmuxtest"
)

const (
	testUdid = "00008150-000000000000TEST"
	lockdown = 62078
)

// shortDir returns a temp directory short enough for a unix socket path.
// t.TempDir() under /var/folders routinely exceeds Darwin's 104-byte sun_path.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// deployment is a running agent+host pair over loopback.
type deployment struct {
	fake       *usbmuxtest.Fake
	hostSocket string
}

// echoDevice serves a device port by echoing everything written to it, with a
// per-port banner so the test can tell ports apart.
func echoDevice(t *testing.T) func(port int) (net.Conn, error) {
	return func(port int) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			banner := []byte{byte(port >> 8), byte(port)}
			if _, err := server.Write(banner); err != nil {
				return
			}
			buf := make([]byte, 4096)
			for {
				n, err := server.Read(buf)
				if n > 0 {
					if _, werr := server.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
		return client, nil
	}
}

func setup(t *testing.T) *deployment {
	t.Helper()
	dir := shortDir(t)
	psk := []byte("integration-test-key")

	fake, err := usbmuxtest.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fake.Close() })
	fake.Attach(usbmuxtest.Device{
		Udid:       testUdid,
		DeviceID:   7,
		PairRecord: []byte("<plist>fake-pair-record</plist>"),
		Handle:     echoDevice(t),
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, err := agent.New(agent.Config{
		Listen:        "127.0.0.1:0",
		PSK:           psk,
		UsbmuxdSocket: fake.Path,
		Version:       "test",
		Logger:        quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bind before Run so the port is known; Run takes over from there.
	agentReady := make(chan struct{})
	go func() {
		close(agentReady)
		_ = a.Run(ctx)
	}()
	<-agentReady

	var agentAddr string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.Addr(); addr != "127.0.0.1:0" {
			agentAddr = addr
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agentAddr == "" {
		t.Fatal("agent never reported a bound address")
	}

	hostSocket := filepath.Join(dir, "host.sock")
	h, err := host.New(host.Options{
		SocketPath: hostSocket,
		Agents:     []host.AgentRef{{Name: "testagent", Addr: agentAddr}},
		PSK:        psk,
		CoreDevice: false,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = h.Run(ctx) }()

	// Wait for the host socket to carry the remote device.
	cli := usbmux.NewClient(hostSocket)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
		devs, err := cli.ListDevices(cctx)
		ccancel()
		if err == nil && len(devs) > 0 {
			return &deployment{fake: fake, hostSocket: hostSocket}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("remote device never appeared on the host socket")
	return nil
}

func TestRemoteDeviceAppearsOnHost(t *testing.T) {
	d := setup(t)
	cli := usbmux.NewClient(d.hostSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	devs, err := cli.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devs))
	}
	got := devs[0]
	if got.Properties.Udid() != testUdid {
		t.Errorf("udid = %q, want %q", got.Properties.Udid(), testUdid)
	}
	if got.Properties.ConnectionType != "USB" {
		t.Errorf("connection type = %q, want USB", got.Properties.ConnectionType)
	}
	// The host assigns its own DeviceID rather than passing the agent's
	// through, so it must differ from the fake's id of 7.
	if got.DeviceID == 7 {
		t.Errorf("host passed through the agent's DeviceID (%d) instead of assigning its own", got.DeviceID)
	}
	t.Logf("device %s visible as host id %d", got.Properties.Udid(), got.DeviceID)
}

func TestPairRecordTravelsFromAgent(t *testing.T) {
	d := setup(t)
	cli := usbmux.NewClient(d.hostSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec, err := cli.ReadPairRecord(ctx, testUdid)
	if err != nil {
		t.Fatalf("ReadPairRecord: %v", err)
	}
	if want := "<plist>fake-pair-record</plist>"; string(rec) != want {
		t.Fatalf("pair record = %q, want %q", rec, want)
	}
	t.Log("the agent's pair record reached the host, so lockdown can authenticate")
}

func TestConnectProxiesDeviceStream(t *testing.T) {
	d := setup(t)
	cli := usbmux.NewClient(d.hostSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	devs, err := cli.ListDevices(ctx)
	if err != nil || len(devs) == 0 {
		t.Fatalf("ListDevices: %v", err)
	}
	conn, err := cli.Connect(ctx, devs[0].DeviceID, lockdown)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// The device handler greets with its port, proving the port number
	// survived the two byte-order conversions between client and device.
	banner := make([]byte, 2)
	if _, err := io.ReadFull(conn, banner); err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if gotPort := int(banner[0])<<8 | int(banner[1]); gotPort != lockdown {
		t.Fatalf("device saw port %d, want %d", gotPort, lockdown)
	}

	payload := bytes.Repeat([]byte("lockdown-handshake"), 512)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatal("payload was corrupted in transit")
	}
	t.Logf("proxied %d bytes to device port %d and back intact", len(payload), lockdown)
}

func TestDetachPropagatesToHost(t *testing.T) {
	d := setup(t)
	cli := usbmux.NewClient(d.hostSocket)

	d.fake.Detach(testUdid)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		devs, err := cli.ListDevices(ctx)
		cancel()
		if err == nil && len(devs) == 0 {
			t.Log("unplugging on the agent removed the device from the host")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("device never disappeared from the host after detach")
}

func TestListenStreamsAttachEvents(t *testing.T) {
	d := setup(t)
	cli := usbmux.NewClient(d.hostSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events, err := cli.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// The existing device must be replayed to a new listener, as usbmuxd does.
	select {
	case ev := <-events:
		if ev.MessageType != usbmux.MsgAttached || ev.Properties.Udid() != testUdid {
			t.Fatalf("unexpected replay event: %+v", ev)
		}
		t.Log("listener received the current device on connect")
	case <-time.After(5 * time.Second):
		t.Fatal("no replay event for the already-attached device")
	}

	// A second device attaching on the agent must stream through live.
	d.fake.Attach(usbmuxtest.Device{
		Udid:     "00008150-00000000SECOND",
		DeviceID: 9,
		Handle:   echoDevice(t),
	})
	for {
		select {
		case ev := <-events:
			if ev.MessageType == usbmux.MsgAttached && ev.Properties.Udid() == "00008150-00000000SECOND" {
				t.Log("a device plugged in on the agent streamed through to the host live")
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("second device never streamed to the host")
		}
	}
}

// TestRealLibimobiledeviceClient drives the host socket with the actual
// idevice_id binary rather than Crossmap's own client, which is the only way
// to show the stand-in daemon is wire-compatible with libusbmuxd.
func TestRealLibimobiledeviceClient(t *testing.T) {
	bin, err := exec.LookPath("idevice_id")
	if err != nil {
		t.Skip("idevice_id not installed")
	}
	d := setup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-l")
	// libusbmuxd takes a unix socket through this variable, prefixed so it
	// is not mistaken for a host:port pair.
	cmd.Env = append(os.Environ(), "USBMUXD_SOCKET_ADDRESS=UNIX:"+d.hostSocket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("idevice_id -l failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), testUdid) {
		t.Fatalf("idevice_id did not list the remote device.\ngot:\n%s\nwant it to contain %s", out, testUdid)
	}
	t.Logf("idevice_id -l reported the remote device:\n%s", strings.TrimSpace(string(out)))
}
