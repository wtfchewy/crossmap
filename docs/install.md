# Install

crossmap is macOS-only. It talks to usbmuxd, `dns-sd` and CoreDevice, all of
which are Apple frameworks.

You need it on **both** Macs.

## Homebrew

```sh
brew install wtfchewy/tap/crossmap
```

## From a release

Download the archive for your architecture from the
[releases page](https://github.com/wtfchewy/crossmap/releases), then:

```sh
tar xzf crossmap_*_darwin_arm64.tar.gz
sudo install -m755 crossmap /usr/local/bin/crossmap
```

## Build from source

Needs Go (the version in `go.mod` or newer).

```sh
git clone https://github.com/wtfchewy/crossmap.git
cd crossmap
make && sudo make install
```

## Also worth installing

To install `.ipa` files through the usbmuxd plane:

```sh
brew install ideviceinstaller
```

Xcode's own tooling (`xcrun devicectl`) comes with the Xcode command line
tools and needs no extra install.

## Verify

```sh
crossmap doctor
```

It reports what it can see on this machine: the usbmuxd socket, attached
devices, any iOS USB network interface, your tailnet address, the shared key,
and which tools are present. Run it on both Macs.
