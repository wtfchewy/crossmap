# Troubleshooting

Start with `crossmap doctor` on **both** Macs. Most problems show up there.

## `idevice_id -l` is empty

- Is the host linked to the agent? The host logs `agent connected` when the
  link is up, and `remote device attached` when a device arrives. Run it with
  `-v` if you are not sure.
- Did you export the socket with the `UNIX:` prefix?
  ```sh
  export USBMUXD_SOCKET_ADDRESS=UNIX:$HOME/.config/crossmap/usbmuxd.sock
  ```
  Without it, libusbmuxd parses the path as a `host:port` pair and silently
  finds nothing.
- Is the phone actually visible on the agent's Mac? Run `crossmap doctor`
  there; it lists attached devices.

## Xcode does not see the device

- You need `--system` *and* `--coredevice` (the latter is on by default).
- Restart Xcode. It caches its usbmuxd connection at launch.
- Confirm the advertisement is live on the host Mac:
  ```sh
  dns-sd -B _remotepairing._tcp
  ```
  The device should appear. If it does not, the host could not publish the
  proxy record — check the host log for `could not mirror CoreDevice service`.
- Check which interface it is advertising on. `remotepairingd` scopes its
  connection to the interface it discovered the device on, so the interface
  needs a real, routable address. Force one with `--interface en0`.

## Pairing fails, or the tunnel never establishes

The trusted tunnel lands on a port the device picks after the handshake.
crossmap relays a window above the advertised port, 64 by default. If the
device picks something outside it, widen the window:

```sh
sudo crossmap host --agent other-mac --system --tunnel-window 256
```

The agent's relay allowlist follows the same window, so both sides widen
together.

## `link: authentication failed`

The two Macs hold different keys. Re-run `crossmap init` on one and
`crossmap join <key>` on the other. Note that `CROSSMAP_PSK`, if set, takes
precedence over the key file.

## `host: a previous crossmap takeover was not unwound`

A `--system` host was killed without unwinding. Run:

```sh
sudo crossmap repair
```

crossmap refuses to start rather than guess which socket is the real one.

## `socket path ... is N bytes; macOS allows at most 103`

Darwin caps unix socket paths at 104 bytes. Pass a shorter `--socket`.

## `could not get this machine's tailnet address`

The agent defaults to binding the tailnet address, which it reads from
`tailscale ip -4`. If Tailscale is not running or not on `PATH`, pass
`--listen` explicitly:

```sh
crossmap agent --listen 100.64.0.5:47841
```

Binding `0.0.0.0` exposes the agent on every network the machine is attached
to. Only do that if you understand the consequences.

## A device appears but every connection is refused

The device is attached to the agent but not trusted by it. Unlock the phone
and accept the "Trust This Computer" prompt on the agent's Mac, then check:

```sh
idevicepair validate
```

crossmap carries the agent's pair record to the host, so the trust has to
exist on the agent side.
