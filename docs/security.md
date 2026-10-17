# Security

This document describes what you can and can't expect from the exporter in terms
of security, and why we believe those expectations hold. To report a
vulnerability, see [SECURITY.md](../SECURITY.md).

## What you can expect

* **Read-only access to devices.** The exporter only sends requests that read a
  device's state (see [Architecture](architecture.md)). It never switches,
  dims or reconfigures anything.
* **Local only.** Devices are reached directly on the local network. The
  exporter never contacts the TP-Link cloud; the TP-Link login is used only to
  authenticate to the devices themselves.
* **The login stays out of logs and metrics.** The TP-Link username and password
  aren't logged (debug logs record only whether credentials are set) and never
  appear in metrics or error messages.
* **KLAP never sends the password.** On KLAP, the exporter sends a random seed
  and checks the device's proof that it knows the login hash before sending
  anything derived from the login. A host that cannot prove it fails the
  handshake having received only the seed.
* **A metrics endpoint you can protect.** TLS 1.2 or newer and HTTP basic
  authentication with bcrypt-hashed passwords can be enabled through the
  [web configuration file](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)
  (`--web.config.file`). They apply to every route, including pprof.
* **Robust handling of device replies.** Replies are capped at 8 MiB over HTTP,
  1 MiB on the legacy protocol and 64 KiB per discovery datagram. Every device
  operation times out after `--kasa.timeout`, HTTP redirects are refused, and
  replies are decoded into typed structures. A malformed reply fails that
  device's reading rather than crashing the exporter; fuzz tests check the
  discovery parser, the legacy cipher and KLAP decryption.
* **Validated input.** Flags are type-checked when parsed, and the device
  settings are checked before any device is contacted. The `collect[]` query
  parameter accepts only enabled collector names, and `exclude[]` ignores
  unknown names with a warning.
* **Least privilege.** The container image runs as the unprivileged `nobody`
  user, the exporter warns when it runs as root, and the systemd unit in
  [`examples/systemd`](../examples/systemd) runs it sandboxed under its own user
  with no capabilities.

## What you can't expect

* **The device protocols are only as strong as TP-Link made them.** The exporter
  has to speak what the firmware speaks:
  * The legacy protocol on TCP 9999 is obfuscated, not encrypted, so anyone on
    the network can read or forge that traffic. It carries no credentials.
  * KLAP hashes the login with fast, unsalted digests (MD5 for login version 1,
    SHA-1 and SHA-256 for version 2). Someone who records a handshake can try to
    guess the password offline. Replies are encrypted with AES-128-CBC, but the
    exporter doesn't check their signatures, so an attacker on the network path
    could alter readings.
  * AES secure passthrough agrees its session key through a 1024-bit RSA
    exchange that doesn't authenticate the device, and then sends the login. A
    host that answers as an AES device receives the TP-Link password.
* **Discovery trusts the local network.** Whatever answers the broadcast is
  polled, including a host that only claims to be a device. Where that matters,
  turn discovery off with `--no-kasa.discovery` and list the devices with
  `--kasa.address`.
* **The TP-Link login is powerful.** Whoever holds it can control every device
  bound to the account through the Kasa and Tapo apps. Keep it in a secret store,
  such as a Kubernetes Secret, pass it through the environment rather than the
  command line, where every user on the host can see it, and limit who can read
  the exporter's environment and configuration.
* **Metrics describe your home.** They include device names, models, firmware
  versions, IP and MAC addresses, and power readings that reveal when someone is
  home. Anyone who can reach the metrics endpoint can read them, so protect it
  with the web configuration file or network controls.
* **pprof exposes internals.** `--web.enable-pprof` is off by default; enable it
  only on trusted networks.

## Assurance case

### Threat model

The assets are the TP-Link login, the data in the metrics, and the availability
of the exporter. The threats considered are:

* a network client reading the metrics, or overloading the exporter with scrapes;
* a host on the local network impersonating a device, or an attacker reading or
  altering traffic between the exporter and the devices;
* a compromised or misbehaving device returning oversized or malformed replies;
* a compromised dependency or a tampered release.

Out of scope: an attacker with access to the host or to the exporter's
configuration, who can read the login directly, and weaknesses in TP-Link's
firmware and protocols themselves, which the exporter has to interoperate with.

### Trust boundaries

1. **Scrapers to exporter.** Requests to the HTTP endpoint come from the network
   and are untrusted. TLS and basic authentication, when configured, are enforced
   at this boundary.
2. **Exporter to devices.** Replies from devices, and anything that answers
   discovery, are untrusted input. The network between them is protected only as
   far as each transport allows, as described above.
3. **Operator to exporter.** Flags, environment variables and the web
   configuration file are trusted: they come from whoever runs the exporter.
4. **Supply chain to build.** Go modules come through the Go module proxy and are
   verified against `go.sum`. Releases are built by GitHub Actions from signed
   tags, and their checksum lists are signed with the same GPG key.

### Secure design principles

* **Least privilege:** read-only requests to devices, and the container image
  runs as `nobody`.
* **Fail-safe defaults:** pprof is off, a KLAP session starts only after the
  device proves it knows the login, a configuration with nothing to poll fails
  the scrape with the reason, a reply over its size limit fails the reading
  instead of being truncated, and a failing device reports `kasa_device_up 0`
  rather than disappearing.
* **Economy of mechanism:** no database and no write paths; the only state is the
  device list, the device sessions and one cached snapshot. Cryptography comes
  from Go's standard library, and the weak algorithms the protocols require are
  confined to the transports that use them.
* **Complete mediation:** when basic authentication is configured,
  exporter-toolkit checks it on every request.
* **Open design:** everything is open source. The protocol code is tested
  against fake devices that perform the real handshakes, with key derivations
  pinned to vectors from the reference implementation.

### Common weaknesses countered

| Weakness                     | Countermeasure                                                                                                                                                                         |
|------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Injection                    | Requests to devices are built from fixed structures and encoded with `encoding/json`, and sent only to configured addresses or to the address a discovery reply came from. A lint rule (`depguard`) bans `os/exec` outside tests, so the exporter can't run other programs. |
| Resource exhaustion          | A size limit on every reply (on the legacy protocol, the length a device declares is checked before anything is allocated), a timeout on every device operation, `--kasa.concurrency` to cap parallel device reads, and `--web.max-requests` to cap parallel scrapes. |
| Malformed input              | Typed JSON decoding, length and padding checks before decryption, validated flags and query parameters, and fuzz tests for the discovery parser, the legacy cipher and KLAP decryption. |
| Information exposure         | The login is never logged or exported, and HTTP redirects from a device are refused, so the session cookie never follows one.                                                       |
| Credentials sent to impostors | On KLAP, the device has to prove it knows the login hash before the exporter sends anything derived from it. AES can't offer this, as described above.                             |
| Memory safety and data races | Go is memory-safe, and CI runs the tests with the race detector.                                                                                                                       |
| Vulnerable dependencies      | Dependabot updates, govulncheck and CodeQL in CI, and OSV-Scanner through OpenSSF Scorecard.                                                                                           |
| Insecure transport           | The metrics endpoint supports TLS 1.2 or newer. Device traffic is encrypted wherever the device's transport allows; the legacy protocol can't be, as described above.              |
