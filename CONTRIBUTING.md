# Contributing

## Getting set up

```sh
git clone https://github.com/wtfchewy/crossmap.git
cd crossmap
make
```

## Tests

```sh
make test          # or: go test ./...
```

Tests that need hardware, a live usbmuxd or `dns-sd` skip themselves rather
than fail, so the suite runs anywhere. The interesting ones:

- `internal/integration` stands up a fake usbmuxd, an agent, a host and a
  real client in one process. This is where to prove a change to either
  plane end to end without an iPhone.
- `internal/integration` also drives the host socket with the real
  `idevice_id` binary, which is the only check that the stand-in daemon is
  wire-compatible with libusbmuxd.
- `internal/dnssd` runs its parsers against live Bonjour traffic.
- `internal/agent` covers the relay allowlist. Treat that one as
  security-critical: it is the only thing stopping an authenticated host from
  using an agent as a general proxy.

If you add a protocol behaviour, add a case to `internal/usbmuxtest` so it can
be exercised without a device.

## Lint

```sh
make lint
golangci-lint run ./...
```

CI also runs `golangci-lint` with `GOOS=darwin`, builds for darwin and linux,
and runs `govulncheck`.

## Working without an iPhone

Most of the system can be developed with no device attached:

```sh
# terminal 1
crossmap agent --listen 127.0.0.1:47841 -v

# terminal 2
crossmap host --agent local=127.0.0.1:47841 --socket /tmp/cm.sock -v

# terminal 3
USBMUXD_SOCKET_ADDRESS=UNIX:/tmp/cm.sock idevice_id -l
```

For a device without hardware, use `internal/usbmuxtest` as the agent's
usbmuxd via `--usbmuxd`.

## Things to be careful about

- **`--system` mode touches machine-wide state.** Any change there needs to
  keep the restore path correct, including when crossmap is killed. Test
  `crossmap repair`.
- **The relay allowlist.** Widening it widens what an authenticated host can
  reach on the agent's network.
- **Datagram framing.** The CoreDevice tunnel is QUIC; coalescing or splitting
  datagrams breaks it silently.
