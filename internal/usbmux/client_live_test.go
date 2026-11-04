package usbmux

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveUsbmuxd exercises the client against this machine's real usbmuxd.
// It is skipped when no socket is present (CI, Linux) so the suite stays
// portable, and it never requires an attached device.
func TestLiveUsbmuxd(t *testing.T) {
	if _, err := os.Stat(DefaultSocket); err != nil {
		t.Skipf("no usbmuxd socket at %s", DefaultSocket)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := NewClient("")

	buid, err := c.ReadBUID(ctx)
	if err != nil {
		t.Fatalf("ReadBUID: %v", err)
	}
	if buid == "" {
		t.Fatal("ReadBUID returned an empty BUID")
	}
	t.Logf("BUID = %s", buid)

	devs, err := c.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	t.Logf("%d device(s) attached", len(devs))
	for _, d := range devs {
		t.Logf("  id=%d udid=%s conn=%s product=%#x",
			d.DeviceID, d.Properties.Udid(), d.Properties.ConnectionType, d.Properties.ProductID)
	}

	// Listen must at least complete its handshake; events are optional.
	lctx, lcancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer lcancel()
	events, err := c.Listen(lctx)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	for e := range events {
		t.Logf("  event %s id=%d udid=%s", e.MessageType, e.DeviceID, e.Properties.Udid())
	}
}
