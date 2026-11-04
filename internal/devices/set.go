// Package devices holds a live set of iOS devices and fans changes out to
// subscribers. Both the agent (tracking what is plugged into it) and the host
// (tracking each source's devices) need exactly this, keyed by UDID.
package devices

import (
	"sort"
	"sync"

	"github.com/wtfchewy/crossmap/internal/wire"
)

// Set is a concurrency-safe, observable collection of devices keyed by UDID.
//
// UDID is the key rather than usbmuxd's DeviceID because DeviceID is assigned
// per-host and is recycled on every replug, while the UDID identifies the
// physical device everywhere in the system.
type Set struct {
	mu        sync.RWMutex
	devices   map[string]wire.DeviceInfo
	listeners map[chan wire.DeviceEvent]struct{}
}

// New returns an empty Set.
func New() *Set {
	return &Set{
		devices:   make(map[string]wire.DeviceInfo),
		listeners: make(map[chan wire.DeviceEvent]struct{}),
	}
}

// Replace swaps in a complete device set, emitting the attach and detach
// events needed to take subscribers from the old set to the new one. It is the
// correct way to resynchronise after a reconnect, where individual events may
// have been missed.
func (s *Set) Replace(devs []wire.DeviceInfo) {
	next := make(map[string]wire.DeviceInfo, len(devs))
	for _, d := range devs {
		if d.Udid != "" {
			next[d.Udid] = d
		}
	}

	s.mu.Lock()
	var events []wire.DeviceEvent
	for udid, old := range s.devices {
		if _, still := next[udid]; !still {
			events = append(events, wire.DeviceEvent{Type: wire.EventDetached, Device: old})
		}
	}
	for udid, d := range next {
		if prev, had := s.devices[udid]; !had || prev.DeviceID != d.DeviceID {
			events = append(events, wire.DeviceEvent{Type: wire.EventAttached, Device: d})
		}
	}
	s.devices = next
	s.mu.Unlock()

	for _, ev := range events {
		s.broadcast(ev)
	}
}

// Attach records a device, emitting an event only if it is new or has been
// reassigned a different DeviceID.
func (s *Set) Attach(d wire.DeviceInfo) {
	if d.Udid == "" {
		return
	}
	s.mu.Lock()
	prev, had := s.devices[d.Udid]
	s.devices[d.Udid] = d
	s.mu.Unlock()
	if had && prev.DeviceID == d.DeviceID {
		return
	}
	s.broadcast(wire.DeviceEvent{Type: wire.EventAttached, Device: d})
}

// Detach removes a device by UDID.
func (s *Set) Detach(udid string) {
	s.mu.Lock()
	gone, found := s.devices[udid]
	if found {
		delete(s.devices, udid)
	}
	s.mu.Unlock()
	if found {
		s.broadcast(wire.DeviceEvent{Type: wire.EventDetached, Device: gone})
	}
}

// DetachByDeviceID removes whichever device currently holds deviceID. usbmuxd
// Detached events carry only the DeviceID, never the UDID.
func (s *Set) DetachByDeviceID(deviceID int) {
	s.mu.Lock()
	var gone wire.DeviceInfo
	found := false
	for udid, d := range s.devices {
		if d.DeviceID == deviceID {
			gone, found = d, true
			delete(s.devices, udid)
			break
		}
	}
	s.mu.Unlock()
	if found {
		s.broadcast(wire.DeviceEvent{Type: wire.EventDetached, Device: gone})
	}
}

// List returns the current devices, ordered by UDID for stable output.
func (s *Set) List() []wire.DeviceInfo {
	s.mu.RLock()
	out := make([]wire.DeviceInfo, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Udid < out[j].Udid })
	return out
}

// Lookup resolves a UDID to its current device record.
func (s *Set) Lookup(udid string) (wire.DeviceInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[udid]
	return d, ok
}

// Subscribe returns a channel of future events and a cancel function.
//
// Delivery is lossy under backpressure: a subscriber that stops reading has
// events dropped rather than stalling every other subscriber. Consumers are
// expected to treat the event stream as a hint and reconcile against List.
func (s *Set) Subscribe() (<-chan wire.DeviceEvent, func()) {
	ch := make(chan wire.DeviceEvent, 64)
	s.mu.Lock()
	s.listeners[ch] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.listeners, ch)
			close(ch)
			s.mu.Unlock()
		})
	}
}

func (s *Set) broadcast(ev wire.DeviceEvent) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.listeners {
		select {
		case ch <- ev:
		default:
		}
	}
}
