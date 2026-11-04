# crossmap

<p>
  <a href="https://github.com/wtfchewy/crossmap/actions/workflows/ci.yml"><img src="https://github.com/wtfchewy/crossmap/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/wtfchewy/crossmap/releases/latest"><img src="https://img.shields.io/github/v/release/wtfchewy/crossmap" alt="latest release"></a>
</p>

Xcode only sees iPhones plugged into the Mac it runs on. crossmap plugs the
phone into one Mac and hands it to another, on a different network.

```sh
brew install wtfchewy/tap/crossmap
crossmap init                                  # pair the two Macs, once
crossmap agent                                 # on the Mac with the iPhone
sudo crossmap host --agent other-mac --system  # on the Mac with Xcode
```

The device now shows up in Xcode's device list, in `xcrun devicectl list
devices`, and to `ideviceinstaller -i MyApp.ipa` — as if the cable ran to this
Mac. No re-pairing: the trust the other Mac already has with the phone travels
over the link.

It works by proxying the two protocols macOS uses to talk to an iPhone, where
each is a plain socket — usbmuxd for lockdown and device services, and
CoreDevice/RemotePairing for the trusted tunnel Xcode needs on iOS 17+. Apple's
own daemons still do the pairing end to end; crossmap never touches the
device's TLS.

> The usbmuxd plane is covered end to end by tests, including against the real
> `idevice_id` binary. The CoreDevice plane is implemented but has not yet been
> exercised against a physical iPhone — expect to tune `--tunnel-window`.

## Docs

- [Install](docs/install.md): Homebrew, a release archive, or from source; what else you need
- [Usage](docs/usage.md): pairing the Macs, every agent and host flag, user mode vs `--system`
- [Architecture](docs/architecture.md): the two planes, pair records, interface scoping, the `/var/run/usbmuxd` takeover
- [Troubleshooting](docs/troubleshooting.md): empty device lists, Xcode not seeing the phone, tunnels that never establish
- [Development](CONTRIBUTING.md): the in-process test rig, working without an iPhone, what to be careful with
- [Security](SECURITY.md): threat model, scope, reporting

## Requirements

macOS on both machines, and a tailnet (or any other route between them).
