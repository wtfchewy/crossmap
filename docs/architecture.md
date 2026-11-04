# Architecture

## Why this is not a USB proxy

The obvious approach is USB/IP: forward raw USB packets from the Mac the phone
is plugged into. crossmap deliberately does not do that. Forwarding USB over a
WAN link is latency-sensitive and fragile, and on macOS it means fighting the
kernel for control of the device.

Instead crossmap proxies the two *protocols* macOS already uses to talk to an
iPhone, at the point where each is a plain socket.

## The two planes

| Plane | Carries | Used by |
|---|---|---|
| **usbmuxd** | lockdown and device services | `ideviceinstaller`, `idevice_id`, `idevicesyslog`, anything on libimobiledevice |
| **CoreDevice / RemotePairing** | the trusted tunnel Xcode uses on iOS 17+ | Xcode, `xcrun devicectl` |

Both are needed. Since iOS 17, Xcode's protocol is **not** carried by usbmuxd —
a usbmuxd-only proxy gets you app installs through libimobiledevice but leaves
Xcode blind.

```
   iPhone ──USB──> Mac B (agent) ──tailnet──> Mac A (host) ──> Xcode, ideviceinstaller
```

## usbmuxd plane

The agent connects to Mac B's `/var/run/usbmuxd` and exposes its devices over
the link. The host runs a **usbmuxd-compatible daemon of its own** that merges
local devices with every agent's devices into one device list, then serves it
on a socket.

Devices are addressed by UDID across the link, not by usbmuxd's `DeviceID`. A
`DeviceID` is assigned per host and recycled on every replug, so it cannot
survive a reconnect; the UDID identifies the physical device everywhere. The
host assigns its own `DeviceID` space, because two agents can legitimately hand
out the same `DeviceID` for different devices.

### Pair records travel with the device

usbmuxd has a `ReadPairRecord` message, so Mac A can complete a lockdown
handshake using the trust relationship Mac B already has with the phone.

**You do not need to pair the phone with Mac A, and no files are copied out of
`/var/db/lockdown`.**

### Connection lifecycle

Each logical operation uses its own connection, mirroring libusbmuxd: usbmuxd
turns a connection into a raw device stream after a successful `Connect`, so a
shared multiplexed connection would be unusable afterwards.

`PortNumber` in a `Connect` request carries a big-endian port inside a
little-endian integer. crossmap swaps it on the way in and back on the way out,
so the port the device sees is the port the caller asked for.

## CoreDevice plane

An iPhone plugged into a Mac raises a CDC-NCM ethernet interface and advertises
`_remotepairing._tcp` on it — the same service it advertises over Wi-Fi for
wireless debugging. The agent captures that advertisement, including its TXT
record and the device's link-local address on that interface.

The host then:

1. Re-publishes the Bonjour record locally with `dns-sd -P` (proxy
   registration), pointing it at an address on this Mac.
2. Relays TCP and UDP from those local ports across the link to the phone.

`remotepairingd` on Mac A discovers what looks like a local device, connects to
this Mac, and its traffic is carried to the phone.

**crossmap never terminates or inspects the device's TLS.** CoreDevice's
certificate-based pairing runs end to end between Mac A's `remotepairingd` and
the iPhone, exactly as it would over a cable. crossmap only fixes *discovery*
and *reachability*.

### Interface scoping

`remotepairingd` scopes its outbound connection to the interface it made the
discovery on. That is why the host advertises on a real interface (`en0` by
default) and binds the relay listeners to that interface's address, rather than
using loopback.

### The tunnel window

After the RemotePairing handshake, the device opens the trusted tunnel on a
port it picks itself, and `remotepairingd` connects to it at the same address.
crossmap therefore relays a window of ports above the advertised CoreDevice
port — 64 by default, set with `--tunnel-window`.

The trusted tunnel runs QUIC, so datagram boundaries matter. UDP relays frame
each payload with a 2-byte length prefix over the stream, because a raw byte
stream would coalesce or split datagrams and break the tunnel.

## The link

One TCP connection over the tailnet, multiplexed with yamux. Every logical
channel — control RPCs, the device event stream, each device service stream,
each port relay — is a stream on that connection.

The tailnet already provides encryption and machine identity. On top of it,
both sides prove knowledge of a pre-shared key over fresh nonces
(HMAC-SHA256). The key itself is never sent. This exists so a stray or
misconfigured peer cannot drive an agent.

### Relay allowlist

The agent will only relay to an address it discovered on an attached device,
and only to that device's advertised CoreDevice port or a bounded window above
it. Without this restriction, an authenticated host could use the agent as a
general proxy into the agent's network.

## `--system` mode

`/var/run/usbmuxd` is created by launchd, which hands the listening descriptor
to Apple's `usbmuxd`. Renaming a unix socket moves the live binding with it, so
crossmap renames it to `/var/run/usbmuxd.crossmap-orig` — where Apple's daemon
keeps serving normally — and binds the canonical path itself. Local devices
still work; they are just reached through the renamed socket.

crossmap unwinds this on exit, including on Ctrl-C and `SIGTERM`. If it is
killed with `SIGKILL` or the machine panics, the swap is left in place and iOS
tooling on that Mac will not work until you run `sudo crossmap repair`. A
reboot also fixes it, because launchd recreates the socket from scratch.

Safety properties:

- Refuses to run unless a live daemon is actually accepting on the socket.
- Verifies Apple's daemon followed the rename before anything depends on it,
  and rolls back if it did not.
- Records the swap in `/var/run/crossmap-takeover.json` so `repair` knows what
  to undo.
- Refuses to start if a previous takeover was never unwound, rather than
  guessing which socket is which.
- Never removes a socket it does not believe is its own stand-in.

## Package layout

```
cmd/crossmap/      CLI: init, join, agent, host, doctor, repair
internal/
  usbmux/          usbmuxd wire protocol: codec, client
  usbmuxtest/      a fake usbmuxd, for tests without hardware
  link/            PSK handshake, yamux multiplexing, splicing, datagram framing
  wire/            payloads shared by agent and host
  devices/         observable device set, keyed by UDID
  dnssd/           dns-sd(1) browse, resolve and proxy-publish
  agent/           the side the iPhone is plugged into
  host/            the side that wants the iPhone
  integration/     agent + host + fake usbmuxd + a real client, in one process
```
