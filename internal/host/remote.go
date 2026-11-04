package host

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/wtfchewy/crossmap/internal/link"
	"github.com/wtfchewy/crossmap/internal/wire"
)

// dialTimeout bounds a single connection attempt to an agent.
const dialTimeout = 10 * time.Second

// serviceRefresh is how often the host re-reads an agent's CoreDevice
// advertisements. Device attach/detach arrives as events, but a device's
// RemotePairing service appears a moment after the device itself and has no
// event of its own, so it is polled.
const serviceRefresh = 5 * time.Second

// Run maintains the link to one agent, reconnecting for as long as ctx lives.
// onChange fires whenever this source's device set may have changed.
func (r *remoteSource) Run(ctx context.Context, log *slog.Logger, onChange func()) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := r.connectAndServe(ctx, log, onChange)
		if ctx.Err() != nil {
			return
		}

		r.mu.Lock()
		r.sess = nil
		r.svcs = nil
		r.lastErr = err
		r.mu.Unlock()
		// The agent is unreachable, so none of its devices are either.
		r.reg.Replace(nil)
		onChange()

		log.Warn("agent link down", "agent", r.name, "addr", r.addr, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (r *remoteSource) connectAndServe(ctx context.Context, log *slog.Logger, onChange func()) error {
	sess, err := link.Dial(r.addr, r.psk, dialTimeout)
	if err != nil {
		return err
	}
	defer sess.Close()

	r.mu.Lock()
	r.sess = sess
	r.lastErr = nil
	r.mu.Unlock()

	var info wire.AgentInfo
	if err := r.control(link.Request{Kind: link.KindControl, Op: wire.OpDescribe}, &info); err != nil {
		return err
	}
	r.mu.Lock()
	r.info = info
	r.mu.Unlock()
	log.Info("agent connected", "agent", r.name, "addr", r.addr, "hostname", info.Hostname, "version", info.Version)

	// Subscribe before the initial list so nothing slips between the two.
	events, _, err := sess.Open(link.Request{Kind: link.KindEvents})
	if err != nil {
		return err
	}
	defer events.Close()

	if err := r.syncDevices(onChange); err != nil {
		return err
	}
	r.syncServices(log)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		events.Close()
	}()
	go func() {
		t := time.NewTicker(serviceRefresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.syncServices(log)
			}
		}
	}()

	dec := json.NewDecoder(events)
	for {
		var ev wire.DeviceEvent
		if err := dec.Decode(&ev); err != nil {
			return err
		}
		ev.Device.Agent = r.name
		switch ev.Type {
		case wire.EventAttached:
			log.Info("remote device attached", "agent", r.name, "udid", ev.Device.Udid)
			r.reg.Attach(ev.Device)
		case wire.EventDetached:
			log.Info("remote device detached", "agent", r.name, "udid", ev.Device.Udid)
			r.reg.Detach(ev.Device.Udid)
		}
		onChange()
		// A newly attached device brings its RemotePairing service up a
		// moment later; look for it without waiting for the poll.
		if ev.Type == wire.EventAttached {
			go func() {
				time.Sleep(1500 * time.Millisecond)
				r.syncServices(log)
			}()
		}
	}
}

func (r *remoteSource) syncDevices(onChange func()) error {
	var list wire.ListDevicesResult
	if err := r.control(link.Request{Kind: link.KindControl, Op: wire.OpListDevices}, &list); err != nil {
		return err
	}
	for i := range list.Devices {
		list.Devices[i].Agent = r.name
	}
	r.reg.Replace(list.Devices)
	onChange()
	return nil
}

func (r *remoteSource) syncServices(log *slog.Logger) {
	var res wire.ListServicesResult
	if err := r.control(link.Request{Kind: link.KindControl, Op: wire.OpListServices}, &res); err != nil {
		return
	}
	r.mu.Lock()
	changed := len(res.Services) != len(r.svcs)
	r.svcs = res.Services
	r.mu.Unlock()
	if changed {
		for _, svc := range res.Services {
			log.Info("remote CoreDevice service", "agent", r.name, "name", svc.Name, "port", svc.Port, "addrs", svc.Addrs)
		}
	}
}
