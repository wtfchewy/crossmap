# Security Policy

## Supported versions

Only the latest release gets security fixes.

| Version      | Supported          |
| ------------ | ------------------ |
| latest       | :white_check_mark: |
| anything older | :x: upgrade      |

## Reporting a vulnerability

Please don't open a public issue for security problems.

Report it privately through GitHub instead:
[**Report a vulnerability**](https://github.com/wtfchewy/crossmap/security/advisories/new)
(the Security tab, then "Report a vulnerability").

Include what you can:

- crossmap version (`crossmap version`), macOS version, and install method
- whether the host was running in `--system` mode (as root) or user mode
- which side you were running (`crossmap agent` or `crossmap host`)
- steps to reproduce, or a proof of concept
- what an attacker gains

### What to expect

- **Acknowledgement** within 3 business days.
- **Assessment** within 10 business days: we confirm the issue, or explain why
  we don't consider it a vulnerability.
- **Updates** at least weekly while we work on a fix.
- **Fix and disclosure**: we ship a patched release, publish a GitHub security
  advisory (with a CVE when warranted), and credit you unless you'd rather we
  didn't. We aim to fix within 90 days and will agree a disclosure date with
  you.

If we decline the report, we'll tell you why, and you're free to disclose it.

## Scope

In scope:

- The `crossmap` agent and host, including `--system` mode running as root
- The link between agent and host: the pre-shared-key handshake, the
  multiplexed transport, and anything that lets an unauthenticated peer drive
  either side
- The agent's relay allowlist, which is the only thing stopping an
  authenticated host from using the agent as a general proxy into the agent's
  network
- The `/var/run/usbmuxd` takeover and its restore path, including any way to
  leave a machine without a working usbmuxd or to have crossmap delete or
  replace a socket it does not own
- Handling of pre-shared keys and of device pair records in transit
- The Homebrew packaging

Out of scope:

- Vulnerabilities in Apple's usbmuxd, CoreDevice or Xcode themselves, and in
  libimobiledevice. Report those to the relevant vendor or project, though
  tell us too if crossmap makes them worse.
- Vulnerabilities in Tailscale. Report those to
  [Tailscale](https://tailscale.com/security).
- Anything that requires an attacker to already be root on one of the Macs.
- The fact that an authenticated host can use any device the agent exposes.
  That is the entire point of the tool; restrict who holds the key.

## Threat model

crossmap assumes:

- The link runs over a tailnet you control. The pre-shared key is a second
  check that both ends belong to the same deployment, not a replacement for
  network-level authentication.
- Anyone holding the pre-shared key is trusted to use the devices attached to
  the agent, including installing software on them.
- The agent binds its tailnet address by default rather than `0.0.0.0`, so it
  is not exposed on whatever untrusted network the machine is also on. If you
  override `--listen`, that is your call to make.
