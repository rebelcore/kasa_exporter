# Kasa Exporter

[![GitHub Release](https://img.shields.io/github/v/release/rebelcore/kasa_exporter?style=for-the-badge&color=22C55E)](https://github.com/rebelcore/kasa_exporter/releases/latest)
[![Docker Image Size](https://img.shields.io/docker/image-size/rebelcore/kasa-exporter/latest?style=for-the-badge&color=1D63ED)](https://hub.docker.com/r/rebelcore/kasa-exporter)
[![OpenSSF Scorecard](https://img.shields.io/ossf-scorecard/github.com/rebelcore/kasa_exporter?style=for-the-badge&label=OpenSSF)](https://scorecard.dev/viewer/?uri=github.com/rebelcore/kasa_exporter)
[![License](https://img.shields.io/badge/license-Apache%202-g?style=for-the-badge&color=8B5CF6)](https://github.com/rebelcore/kasa_exporter/blob/master/LICENSE)

Prometheus exporter for TP-Link Kasa and Tapo smart home device metrics, written
in Go with pluggable metric collectors.

This page covers running the exporter as a container. The
[full documentation](https://github.com/rebelcore/kasa_exporter#readme) lives on
GitHub, including the complete metrics reference, alerting rules and tuning
guidance.

## Quick start

Discovery is a UDP broadcast, and a bridge network cannot put a broadcast on
your LAN. Host networking is the shortest path to a working exporter:

```bash
docker run -d --network host rebelcore/kasa-exporter:latest
```

Metrics are then on `http://<host>:9498/metrics`.

Devices on newer firmware — an EP25, or an HS300 on hardware 2.0 — want the
TP-Link account they are bound to. Devices on the original firmware need no
login at all, so supply credentials only if part of your fleet asks for them:

```bash
docker run -d \
  --network host \
  -e KASA_USERNAME=you@example.com \
  -e KASA_PASSWORD=secret \
  rebelcore/kasa-exporter:latest
```

## Networking

This is the one thing that decides whether the container works, so it is worth
being deliberate about.

**Host networking** lets the exporter broadcast, so it finds devices by itself
and picks up new ones as they appear. It also means the exporter binds port 9498
on the node directly.

**Bridge networking** cannot broadcast to your LAN. Turn discovery off and name
the devices instead:

```bash
docker run -d \
  -p 9498:9498 \
  rebelcore/kasa-exporter:latest \
  --no-kasa.discovery \
  --kasa.address=192.168.1.20,192.168.1.21
```

Both approaches can be combined: configured addresses are always polled, and
anything discovered is added to them. A configured address is never dropped,
whether or not it answers discovery.

Note that boolean flags are negated with a `--no-` prefix — `--no-kasa.discovery`,
not `--kasa.discovery=false`.

## Compose

```yaml
---
services:
  kasa_exporter:
    image: rebelcore/kasa-exporter:latest
    container_name: kasa_exporter
    # Needed for broadcast discovery; drop it and use --no-kasa.discovery
    # with --kasa.address if your devices are on a routed network.
    network_mode: host
    environment:
      KASA_USERNAME: you@example.com
      KASA_PASSWORD: secret
    restart: unless-stopped
```

## Kubernetes

See
[`examples/kubernetes`](https://github.com/rebelcore/kasa_exporter/tree/master/examples/kubernetes),
which covers both the host-networking deployment that can discover devices and
the pod-network one that lists them by address.

## Configuration

Every flag has an environment variable equivalent, which is the better way to
pass credentials.

| Environment variable | Flag | Default | What it does |
|----------------------|------|---------|--------------|
| `KASA_ADDRESS` | `--kasa.address` | — | Device addresses to poll, comma-separated. Polled in addition to anything discovered. |
| `KASA_USERNAME` | `--kasa.username` | — | TP-Link account username, for devices that require a login. |
| `KASA_PASSWORD` | `--kasa.password` | — | TP-Link account password. |
| `KASA_DISCOVERY` | `--[no-]kasa.discovery` | `true` | Find devices by broadcasting on the local network. |
| `KASA_DISCOVERY_TARGET` | `--kasa.discovery-target` | `255.255.255.255` | Broadcast address the probes are sent to. |
| `KASA_DISCOVERY_INTERVAL` | `--kasa.discovery-interval` | `5m` | How often to sweep for new devices. |
| `KASA_DISCOVERY_TIMEOUT` | `--kasa.discovery-timeout` | `5s` | How long each sweep waits for answers. |
| `KASA_DISCOVERY_PACKETS` | `--kasa.discovery-packets` | `3` | Probes per sweep, since broadcast datagrams are lossy. |
| `KASA_TIMEOUT` | `--kasa.timeout` | `5s` | How long to wait for a single device. |
| `KASA_CACHE_TTL` | `--kasa.cache-ttl` | `10s` | How long one set of readings is reused. Keep below the scrape interval. |
| `KASA_CONCURRENCY` | `--kasa.concurrency` | `16` | Maximum devices queried at once. |
| `KASA_KEEP_MISSING` | `--[no-]kasa.keep-missing` | `true` | Keep polling devices that stopped answering discovery, so they report as unreachable rather than disappearing. |

The remaining flags come from the Prometheus exporter toolkit and behave as they
do in every other exporter: `--web.listen-address`, `--web.telemetry-path`,
`--web.config.file`, `--log.level`, `--log.format`.

## Supported devices

The exporter speaks the TP-Link protocols directly, so it works with a device
family rather than a fixed model list. Anything in these ranges is reported:

| Kind | Models |
|------|--------|
| Plugs | EP10, EP25, HS100, HS103, HS110, KP115, KP125, KP125M, P100, P110, P115 |
| Power strips | HS300, HS107, KP200, KP303, KP400, EP40, P300, P304 |
| Bulbs and light strips | KL110, KL130, KL400, KL430, LB series, L510, L530, L900, L930 |
| Wall switches and dimmers | HS200, HS210, HS220, KS200, KS205, KS225, KS230 |
| Hubs and their sensors | KH100, H100, H200, T100, T110, T310, T315 |

Development is done against a fleet of **EP25** plugs and **HS300** power strips
(hardware 2.0), so those two are the best tested. Hardware whose model is not
recognised is still reported by the `device` collector, so new hardware degrades
to "reachable, unclassified" instead of vanishing.

Three transports are implemented — legacy XOR on TCP 9999, KLAP over HTTP, and
AES secure passthrough — and the right one is chosen per device from what
discovery reports. Nothing has to be configured for it.

## Collectors

There is one collector per kind of device — `plug`, `strip`, `bulb`,
`lightstrip`, `dimmer`, `wallswitch`, `hub` — plus a `device` collector for the
facts every device has in common. **All are enabled by default**, and a
collector with no device of its kind in the fleet is a successful scrape that
reports nothing.

Disable one with `--no-collector.NAME`, or select a specific set with
`--collector.disable-defaults --collector.NAME ...`.

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: kasa
    static_configs:
      - targets: ['localhost:9498']
```

A scrape queries every device, so the interval wants to be comfortably longer
than a scrape takes. 15s suits a small fleet.

`kasa_device_up` is the signal to alert on — a device that fails a scrape
reports 0 under its last known name rather than disappearing, so it stays
alertable. Total draw across a deployment is a single query:

```promql
sum(kasa_power_watts)
```

Note that `kasa_energy_kilowatt_hours_total` does not accumulate over the same
window on every device: the IOT range reports a running total, while the SMART
range exposes only a monthly one that resets. Read it with `increase()` rather
than comparing absolute values.

The
[full metrics reference](https://github.com/rebelcore/kasa_exporter#metrics-reference)
lists every series and its labels.

## Grafana

The official dashboard lives in
[rebelcore/kasa_grafana](https://github.com/rebelcore/kasa_grafana).

## Security

The exporter takes **TP-Link cloud account credentials**, and its metrics
describe your home network. Two things follow.

**Do not put the password on the command line.** It is visible to every user on
the host through `ps`. Use the environment variable, from a Docker secret, an
`--env-file` readable only by the service account, or a Kubernetes `Secret`.

**The metrics endpoint is not public data.** `kasa_device_info` carries device
names, models, firmware versions, IP addresses and MAC addresses, and the power
series reveal occupancy patterns. Bind it to an interface your Prometheus can
reach and nothing else, and use `--web.config.file` to add TLS and basic auth if
it has to cross an untrusted network.

The exporter only ever reads. It has no code path that switches a relay, changes
a light, or writes anything back to a device.

## Image tags

| Tag | Points at |
|-----|-----------|
| `latest` | The most recent stable release |
| `vX.Y.Z` | That exact release |
| `vX.Y.Z-beta` | A pre-release; never tagged `latest` |

Built for `linux/amd64`, `linux/arm64`, `linux/386`, `linux/ppc64le`,
`linux/riscv64` and `linux/s390x`. Each image ships an SBOM.

## Troubleshooting

**Nothing is discovered.** Discovery is a broadcast and does not cross a router,
nor a bridge network. Check that the container runs with `--network host` and
that the host is on the same segment as the devices. Where neither can change,
use `--no-kasa.discovery --kasa.address=...` instead.

**A device reports `kasa_device_up 0`.** Run with `--log.level=debug`; the
failure names every transport that was tried and why each one failed.

**"device rejected the supplied credentials."** The device is bound to a
different TP-Link account from the rest of the fleet, which is common when
devices were added by different people. Check which account owns it in the Kasa
app. A rejected login is not retried.

## Source and issues

Source, full documentation and the issue tracker are at
[github.com/rebelcore/kasa_exporter](https://github.com/rebelcore/kasa_exporter).
For anything security-sensitive, follow
[SECURITY.md](https://github.com/rebelcore/kasa_exporter/blob/master/SECURITY.md)
rather than opening a public issue.

Licensed under the Apache License 2.0.
