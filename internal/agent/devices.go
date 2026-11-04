package agent

import (
	"github.com/wtfchewy/crossmap/internal/usbmux"
	"github.com/wtfchewy/crossmap/internal/wire"
)

// deviceFromEvent converts a usbmuxd Attached event into Crossmap's
// device record.
func deviceFromEvent(ev usbmux.Event) wire.DeviceInfo {
	p := ev.Properties
	id := ev.DeviceID
	if id == 0 {
		id = p.DeviceID
	}
	return wire.DeviceInfo{
		Udid:            p.Udid(),
		DeviceID:        id,
		ConnectionType:  p.ConnectionType,
		ProductID:       p.ProductID,
		LocationID:      p.LocationID,
		ConnectionSpeed: p.ConnectionSpeed,
	}
}
