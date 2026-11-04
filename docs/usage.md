# Usage

## Pair the two Macs

Once, on either Mac:

```sh
crossmap init
```

It prints a key and the command to run on the other Mac:

```sh
crossmap join <key>
```

Both machines must hold the same key. It is only ever used to prove each side
belongs to your deployment — it is never sent over the wire.

The key can also come from `CROSSMAP_PSK`, which suits launchd and CI.

## On the Mac with the iPhone

```sh
crossmap agent
```

It binds this machine's tailnet address on port 47841.

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | tailnet address, port 47841 | Address to serve on. The default avoids exposing the agent on whatever other network the machine is on. |
| `--usb-only` | `true` | Only expose devices attached over USB, not other phones on the same Wi-Fi. |
| `--coredevice` | `true` | Advertise attached devices' CoreDevice service to hosts. |
| `--usbmuxd` | `/var/run/usbmuxd` | The usbmuxd socket to proxy. |
| `-v` | | Verbose logging. |

## On the Mac with Xcode

### User mode — libimobiledevice tools, no root

```sh
crossmap host --agent wes-macbook-pro-rex
```

Then point tools at it:

```sh
export USBMUXD_SOCKET_ADDRESS=UNIX:$HOME/.config/crossmap/usbmuxd.sock
idevice_id -l
ideviceinstaller -i MyApp.ipa
```

The `UNIX:` prefix is required. Without it libusbmuxd parses the value as a
`host:port` pair.

### System mode — Xcode and devicectl

Xcode and `devicectl` do not read `USBMUXD_SOCKET_ADDRESS`. For them:

```sh
sudo crossmap host --agent wes-macbook-pro-rex --system
```

`--system` moves Apple's `/var/run/usbmuxd` aside and binds that path itself,
so every tool on the machine finds crossmap with no configuration. See
[architecture.md](architecture.md#--system-mode) for exactly what it does and
how it unwinds.

**Restart Xcode after starting the host.** It caches its usbmuxd connection at
launch.

### Host flags

| Flag | Default | Meaning |
|---|---|---|
| `--agent` | — | Agent to connect to, as `name=host:port` or just a hostname. Repeatable. |
| `--system` | `false` | Take over `/var/run/usbmuxd`. Needs root. |
| `--socket` | `~/.config/crossmap/usbmuxd.sock` | Socket path in user mode. |
| `--coredevice` | `true` | Mirror the CoreDevice service so Xcode and devicectl see remote devices. |
| `--interface` | first usable | Local interface to advertise remote devices on. |
| `--tunnel-window` | `64` | Ports above the CoreDevice port to relay. |
| `-v` | | Verbose logging. |

Several agents can be connected at once:

```sh
crossmap host --agent office=mac-mini --agent home=studio.tailnet.ts.net:47841
```

## Checking on things

```sh
crossmap doctor          # what this machine can see
crossmap doctor --scan   # also browse for CoreDevice advertisements
```

The host logs `agent connected` when a link comes up and
`remote device attached` when a device arrives.

## Recovering from a crash

If a `--system` host was killed with `SIGKILL`, `/var/run/usbmuxd` is left
swapped and iOS tooling on that Mac will not work:

```sh
sudo crossmap repair
```

A reboot fixes it too.
