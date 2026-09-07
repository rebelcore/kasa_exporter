<div align="center">

# Kasa Exporter

[![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/rebelcore/kasa_exporter/test.yml?style=for-the-badge&color=22C55E)](https://github.com/rebelcore/kasa_exporter/actions/workflows/test.yml)
[![GitHub Release](https://img.shields.io/github/v/release/rebelcore/kasa_exporter?style=for-the-badge&color=22C55E)](https://github.com/rebelcore/kasa_exporter/releases/latest)
[![Docker Pulls](https://img.shields.io/docker/pulls/rebelcore/kasa-exporter?style=for-the-badge&color=1D63ED)](https://hub.docker.com/r/rebelcore/kasa-exporter)
[![Docker Image Size](https://img.shields.io/docker/image-size/rebelcore/kasa-exporter/latest?style=for-the-badge&color=1D63ED)](https://hub.docker.com/r/rebelcore/kasa-exporter)
[![OpenSSF Scorecard](https://img.shields.io/ossf-scorecard/github.com/rebelcore/kasa_exporter?style=for-the-badge&label=OpenSSF)](https://scorecard.dev/viewer/?uri=github.com/rebelcore/kasa_exporter)
[![Go Version](https://img.shields.io/github/go-mod/go-version/rebelcore/kasa_exporter?style=for-the-badge&color=00ADD8)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202-g?style=for-the-badge&color=8B5CF6)](LICENSE)

Prometheus exporter for TP-Link Kasa and Tapo smart home device metrics
exposed in Go with pluggable metric collectors.

</div>

---

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
(hardware 2.0), so those two are the best tested. The rest are implemented from
the same protocol families and decoded from the same fields; a model that is not
recognised is still reported by the `device` collector rather than dropped, so
new hardware degrades to "reachable, unclassified" instead of vanishing.

Bug reports for a model that misbehaves are welcome — include the output of
`kasa_device_info` and a debug log.

## Installation

Download the binary for your platform from the
[latest release](https://github.com/rebelcore/kasa_exporter/releases/latest):

```bash
tar xvfz kasa_exporter-*.tar.gz
cd kasa_exporter-*
./kasa_exporter
```

Every release is signed. Each tarball has a detached `.asc` signature beside
it, and `sha256sums.txt` is signed too so the checksum list cannot be swapped
for one matching altered files:

```bash
gpg --verify sha256sums.txt.asc sha256sums.txt
sha256sum --check --ignore-missing sha256sums.txt
```

The same key signs the release tag, so `git tag -v v1.0.0` verifies against the
same fingerprint. It is published at <https://docs.rebelcore.org/security>.

Or run it from the container image:

```bash
docker run -d --network host rebelcore/kasa-exporter:latest
```

Or build it from source — see [Development building and running](#development-building-and-running).

## Usage

The `kasa_exporter` listens on HTTP port
[9498](https://github.com/prometheus/prometheus/wiki/Default-port-allocations)
by default. See the `--help` output for the full flag list.

Nothing is required. On a flat network the exporter finds your devices by
broadcast and starts reporting them:

```bash
./kasa_exporter
```

Devices on newer firmware — an EP25, or an HS300 on hardware 2.0 — want the
TP-Link account they are bound to. Devices on the original firmware need no
login at all, so supply credentials only if part of your fleet asks for them.

### Configuration

Every device flag has an environment variable equivalent, which is the better
way to pass credentials — see [Security](#security).

| Flag | Environment variable | Default | What it does |
|------|----------------------|---------|--------------|
| `--kasa.address` | `KASA_ADDRESS` | — | Device addresses to poll, comma-separated. Repeatable. Polled in addition to anything discovered. |
| `--kasa.username` | `KASA_USERNAME` | — | TP-Link account username, for devices that require a login. |
| `--kasa.password` | `KASA_PASSWORD` | — | TP-Link account password. |
| `--[no-]kasa.discovery` | `KASA_DISCOVERY` | `true` | Find devices by broadcasting on the local network. |
| `--kasa.discovery-target` | `KASA_DISCOVERY_TARGET` | `255.255.255.255` | Broadcast address the probes are sent to. |
| `--kasa.discovery-interval` | `KASA_DISCOVERY_INTERVAL` | `5m` | How often to sweep for new devices. |
| `--kasa.discovery-timeout` | `KASA_DISCOVERY_TIMEOUT` | `5s` | How long each sweep waits for answers. |
| `--kasa.discovery-packets` | `KASA_DISCOVERY_PACKETS` | `3` | Probes per sweep, since broadcast datagrams are lossy. |
| `--kasa.timeout` | `KASA_TIMEOUT` | `5s` | How long to wait for a single device. |
| `--kasa.cache-ttl` | `KASA_CACHE_TTL` | `10s` | How long one set of readings is reused. Keep below the scrape interval. |
| `--kasa.concurrency` | `KASA_CONCURRENCY` | `16` | Maximum devices queried at once. |
| `--[no-]kasa.keep-missing` | `KASA_KEEP_MISSING` | `true` | Keep polling devices that stopped answering discovery, so they report as unreachable rather than disappearing. |

Note that boolean flags are negated with a `--no-` prefix — `--no-kasa.discovery`,
not `--kasa.discovery=false`. The same applies to `--no-collector.NAME`.

The remaining flags come from the Prometheus exporter toolkit and behave as they
do in every other exporter: `--web.listen-address`, `--web.telemetry-path`,
`--web.config.file`, `--log.level`, `--log.format`.

As a dotenv file:

```dotenv
KASA_ADDRESS=192.168.1.20,192.168.1.21
KASA_USERNAME=you@example.com
KASA_PASSWORD=secret
```

### Finding devices

Discovery is a UDP broadcast to ports 9999 and 20002, repeated a few times per
sweep because broadcast datagrams are lossy. It runs every
`--kasa.discovery-interval` (5 minutes by default) in the background, so a
scrape never waits on it after the first one.

Where broadcast cannot reach — a routed VLAN, a container on a bridge network —
name the devices instead:

```bash
./kasa_exporter --no-kasa.discovery --kasa.address=192.168.1.20,192.168.1.21
```

Both can be used together: configured addresses are always polled, and anything
discovered is added to them. A configured address is never dropped, whether or
not it answers discovery.

### Docker

Discovery is a broadcast, which a bridge network cannot put on your LAN. Use
host networking for it to work:

```bash
docker run -d \
  --network host \
  rebelcore/kasa-exporter:latest
```

Without host networking, turn discovery off and list the devices:

```bash
docker run -d \
  -p 9498:9498 \
  rebelcore/kasa-exporter:latest \
  --no-kasa.discovery \
  --kasa.address=192.168.1.20,192.168.1.21
```

For Docker compose:

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

### Kubernetes

See [`examples/kubernetes`](examples/kubernetes), which covers both the
host-networking deployment that can discover devices and the pod-network one
that lists them by address.

## Prometheus configuration

Scrape it like any other exporter:

```yaml
scrape_configs:
  - job_name: kasa
    static_configs:
      - targets: ['localhost:9498']
```

A scrape queries every device, so the interval wants to be comfortably longer
than a scrape takes. 15s suits a small fleet; see
[Tuning for a large fleet](#tuning-for-a-large-fleet) if yours is big enough that
scrapes start running long. Watch `kasa_scrape_collector_duration_seconds` to
see where you actually are.

### Alerting rules

`kasa_device_up` is the signal to alert on — a device that fails a scrape reports
0 under its own name rather than disappearing, so it stays alertable.

```yaml
groups:
  - name: kasa
    rules:
      - alert: KasaDeviceDown
        expr: kasa_device_up == 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Kasa device {{ $labels.alias }} is unreachable"
          description: "{{ $labels.alias }} ({{ $labels.host }}) has not answered for 10 minutes."

      - alert: KasaCollectorFailing
        expr: kasa_scrape_collector_success == 0
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Kasa collector {{ $labels.collector }} is failing"

      - alert: KasaDeviceOverheated
        expr: kasa_device_overheated == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Kasa device {{ $labels.alias }} reports overheating"

      # Sensors paired to a hub run on batteries.
      - alert: KasaSensorBatteryLow
        expr: kasa_hub_child_battery_low == 1
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "Sensor {{ $labels.child_alias }} has a low battery"
```

A `for:` of several minutes matters here more than for most exporters: these are
Wi-Fi devices on consumer firmware, and a single missed scrape is normal rather
than an outage.

## Security

The exporter takes **TP-Link cloud account credentials**, and its metrics
describe your home network. Two things follow.

**Do not put the password in a command line.** It is visible to every user on
the host through `ps`. Use the environment variable instead, from a file only
the service account can read:

```ini
# /etc/sysconfig/kasa_exporter, mode 0600
KASA_USERNAME=you@example.com
KASA_PASSWORD=secret
```

The systemd unit in [`examples/systemd`](examples/systemd) reads exactly that,
and the Kubernetes manifests take the same values from a `Secret`.

**The metrics endpoint is not public data.** `kasa_device_info` carries device
names, models, firmware versions, IP addresses and MAC addresses, and the power
series reveal occupancy patterns — when someone is home, when they sleep, when
they run the washing machine. Bind it to an interface your Prometheus can reach
and nothing else, and use `--web.config.file` to add TLS and basic auth if it
has to cross an untrusted network. See [TLS endpoint](#tls-endpoint).

The exporter only ever reads. It has no code path that switches a relay, changes
a light, or writes anything back to a device.

## Devices and protocols

TP-Link hardware speaks three different protocols, and a single fleet routinely
mixes them. The exporter implements all three and picks per device from what
discovery reports, so nothing has to be configured for it:

| Transport                  | Used by                                              |
|----------------------------|------------------------------------------------------|
| Legacy XOR on TCP 9999     | Kasa hardware on original firmware (HS110, HS300 1.0) |
| KLAP over HTTP             | EP25, KP125M, HS300 on hardware 2.0, newer Tapo       |
| AES secure passthrough     | Tapo-generation firmware from before KLAP             |

KLAP has two login versions that differ in how credentials are hashed. The
device says which it wants in its discovery reply, and that answer is used. This
matters for an HS300 on hardware 2.0: authenticating it as login version 1 fails
with an error that reads exactly like a wrong password and is not one.

A device that is reachable but never answered discovery is probed transport by
transport, newest first. The legacy protocol is tried last on purpose — newer
firmware leaves port 9999 closed, so leading with it would put a guaranteed
timeout in front of every other attempt.

## Grafana Dashboard

Refer to [this repository](https://github.com/rebelcore/kasa_grafana) to check out the official dashboard for the exporter.

## Collectors

There is one collector per kind of device, plus a `device` collector for the
facts every device has in common. **All of them are enabled by default.**

Collectors are enabled by providing a `--collector.NAME` flag.
Collectors that are enabled by default can be disabled
by providing a `--no-collector.NAME` flag.
To enable only some specific collector(s),
use `--collector.disable-defaults --collector.NAME ...`.
For example, `--collector.disable-defaults --collector.plug --collector.strip`
reports only plugs and power strips.

A collector with no device of its kind in the fleet is a successful scrape that
reports nothing, so leaving them all on costs nothing but a metric per scrape.

### Enabled by default

| Name         | Description                                                                                     |
|--------------|-------------------------------------------------------------------------------------------------|
| `device`     | Reachability, static device information, signal strength, uptime and LED state for every device. |
| `plug`       | Single-outlet smart plugs (EP25, KP125, HS110, P110) and the energy the metered ones draw.        |
| `strip`      | Multi-outlet power strips (HS300, KP303, EP40, P300), as a whole and outlet by outlet.            |
| `bulb`       | Smart bulbs (KL, LB, L530), their light settings and power draw.                                  |
| `lightstrip` | Addressable light strips (KL400, KL430, L900), as bulbs with a length.                            |
| `dimmer`     | Dimmer wall switches (HS220, KS220): state and brightness.                                        |
| `wallswitch` | Non-dimming wall switches (HS200, KS200).                                                        |
| `hub`        | Smart hubs (KH100, H100) and the sensors paired to them.                                          |

Hardware whose model is not recognised is still reported by the `device`
collector, so a new model shows up as a reachable device rather than vanishing.

### Reading the energy metrics

Energy is one metric family across the whole fleet rather than one per kind of
device, so the power a deployment draws is a single query:

```promql
sum(kasa_power_watts)
```

A power strip's own figures and its outlets' are separate families
(`kasa_power_watts` and `kasa_strip_socket_power_watts`) precisely so that this
query does not count the same electricity twice. An HS300 has no meter of its
own: its device-level readings are derived from its outlets, summed — except
voltage, which is shared by the outlets and so taken rather than added.

A device that measures a quantity reports it; one that does not leaves it out
entirely rather than reporting zero. An EP25 measures power but not voltage or
current, so it has no `kasa_voltage_volts` series at all — which is different
from a plug reporting 0 V.

#### What the energy counter counts

`kasa_energy_kilowatt_hours_total` is a counter, and the window it accumulates
over is not the same on every device, because the firmware does not offer the
same figure on every device:

| Family | Source | Resets |
|--------|--------|--------|
| IOT (HS300, HS110) | the meter's own running total | on a device reset |
| SMART (EP25, P110) | `month_energy`, the longest window the firmware exposes | at the start of each month |

So an EP25 that has been plugged in for eight months reports the energy it has
used *this month*, while an HS300 next to it reports everything it has ever
metered. Comparing the two as absolute values is meaningless.

Query it as the counter it is and this does not matter — `rate()` and
`increase()` handle a reset correctly, and the IOT total resets too:

```promql
increase(kasa_energy_kilowatt_hours_total[24h])
```

Use `kasa_power_watts` for anything instantaneous; it means the same thing
everywhere.

### Metrics Reference

<details>
<summary>Expand for a full list of exported metrics</summary>

#### Every device

| Metric                              | Type  | Labels                                                                                  |
|-------------------------------------|-------|------------------------------------------------------------------------------------------|
| `kasa_device_up`                    | Gauge | host, alias                                                                              |
| `kasa_device_info`                  | Gauge | host, alias, model, device_id, hardware_version, firmware_version, mac, type, protocol   |
| `kasa_device_signal_strength_dbm`   | Gauge | host, alias                                                                              |
| `kasa_device_signal_level`          | Gauge | host, alias                                                                              |
| `kasa_device_uptime_seconds`        | Gauge | host, alias                                                                              |
| `kasa_device_led_on`                | Gauge | host, alias                                                                              |
| `kasa_device_overheated`            | Gauge | host, alias                                                                              |
| `kasa_device_updating`              | Gauge | host, alias                                                                              |
| `kasa_devices`                      | Gauge | type                                                                                     |

`kasa_device_up` is the liveness signal to alert on. A device that fails a
scrape reports 0 under its last known name rather than disappearing: a device
that vanished from the output is indistinguishable from one that was never
configured.

#### Energy (any metered device)

| Metric                             | Type    | Labels      |
|------------------------------------|---------|-------------|
| `kasa_power_watts`                 | Gauge   | host, alias |
| `kasa_voltage_volts`               | Gauge   | host, alias |
| `kasa_current_amperes`             | Gauge   | host, alias |
| `kasa_energy_kilowatt_hours_total` | Counter | host, alias |

#### Plug

| Metric         | Type  | Labels      |
|----------------|-------|-------------|
| `kasa_plug_on` | Gauge | host, alias |

#### Strip

| Metric                                       | Type    | Labels                                  |
|----------------------------------------------|---------|-----------------------------------------|
| `kasa_strip_on`                              | Gauge   | host, alias                             |
| `kasa_strip_sockets`                         | Gauge   | host, alias                             |
| `kasa_strip_socket_on`                       | Gauge   | host, alias, socket, socket_alias       |
| `kasa_strip_socket_uptime_seconds`           | Gauge   | host, alias, socket, socket_alias       |
| `kasa_strip_socket_power_watts`              | Gauge   | host, alias, socket, socket_alias       |
| `kasa_strip_socket_voltage_volts`            | Gauge   | host, alias, socket, socket_alias       |
| `kasa_strip_socket_current_amperes`          | Gauge   | host, alias, socket, socket_alias       |
| `kasa_strip_socket_energy_kilowatt_hours_total` | Counter | host, alias, socket, socket_alias    |

The `socket` label is the outlet's position on the strip, counted from 1, not
its name: unused outlets on an HS300 all share the name "Empty" and would
otherwise collapse into a single series.

#### Bulb and light strip

| Metric                                  | Type  | Labels      |
|-----------------------------------------|-------|-------------|
| `kasa_bulb_on`                          | Gauge | host, alias |
| `kasa_bulb_brightness_percent`          | Gauge | host, alias |
| `kasa_bulb_color_temperature_kelvin`    | Gauge | host, alias |
| `kasa_bulb_hue_degrees`                 | Gauge | host, alias |
| `kasa_bulb_saturation_percent`          | Gauge | host, alias |
| `kasa_lightstrip_on`                    | Gauge | host, alias |
| `kasa_lightstrip_brightness_percent`    | Gauge | host, alias |
| `kasa_lightstrip_color_temperature_kelvin` | Gauge | host, alias |
| `kasa_lightstrip_hue_degrees`           | Gauge | host, alias |
| `kasa_lightstrip_saturation_percent`    | Gauge | host, alias |
| `kasa_lightstrip_length`                | Gauge | host, alias |

While a light is off, the settings reported are the ones it will return to when
switched back on, so a dimmed bulb does not look reset every evening. The `_on`
metric is what says whether it is currently lit.

#### Dimmer and wall switch

| Metric                           | Type  | Labels      |
|----------------------------------|-------|-------------|
| `kasa_dimmer_on`                 | Gauge | host, alias |
| `kasa_dimmer_brightness_percent` | Gauge | host, alias |
| `kasa_wallswitch_on`             | Gauge | host, alias |

#### Hub

| Metric                              | Type  | Labels                                                        |
|-------------------------------------|-------|---------------------------------------------------------------|
| `kasa_hub_children`                 | Gauge | host, alias                                                   |
| `kasa_hub_child_info`               | Gauge | host, alias, child_id, child_alias, model, category           |
| `kasa_hub_child_up`                 | Gauge | host, alias, child_id, child_alias                            |
| `kasa_hub_child_battery_percent`    | Gauge | host, alias, child_id, child_alias                            |
| `kasa_hub_child_battery_low`        | Gauge | host, alias, child_id, child_alias                            |
| `kasa_hub_child_temperature_celsius` | Gauge | host, alias, child_id, child_alias                           |
| `kasa_hub_child_humidity_percent`   | Gauge | host, alias, child_id, child_alias                            |
| `kasa_hub_child_signal_strength_dbm` | Gauge | host, alias, child_id, child_alias                           |
| `kasa_hub_child_signal_level`       | Gauge | host, alias, child_id, child_alias                            |

Sensors configured in Fahrenheit are converted, so
`kasa_hub_child_temperature_celsius` is always Celsius.

#### Exporter

| Metric                                    | Type  | Labels                                                   |
|-------------------------------------------|-------|----------------------------------------------------------|
| `kasa_exporter_build_info`                | Gauge | branch, goarch, goos, goversion, revision, tags, version |
| `kasa_scrape_collector_duration_seconds`  | Gauge | collector                                                |
| `kasa_scrape_collector_success`           | Gauge | collector                                                |

</details>

## Tuning for a large fleet

Every collector reads the same snapshot of device readings, taken once per
scrape, so the fleet is queried once no matter how many collectors are enabled.
Two flags shape how long that takes:

* `--kasa.concurrency` (default 16) caps how many devices are queried at once.
  With more devices than that, the scrape takes as many rounds as it needs, and
  each round can cost up to `--kasa.timeout`. Raise it for a large fleet.
* `--kasa.cache-ttl` (default 10s) is how long one set of readings is reused. It
  exists to share a single round of queries across the collectors of one scrape,
  not to serve stale data — keep it below your scrape interval.

An HS300 costs one request per outlet plus one for its state, because each
outlet meters itself. An EP25 costs a single request: its state and both energy
methods are batched together.

## Filtering enabled collectors

The `kasa_exporter` will expose all metrics from enabled collectors
by default. This is the recommended way to collect metrics to avoid errors.

For advanced use the `kasa_exporter` can be passed an optional list
of collectors to filter metrics. The `collect[]` parameter may be used
multiple times. In Prometheus configuration you can use this syntax under
the [scrape config](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#scrape_config).

```
  params:
    collect[]:
      - device
      - plug
      - strip
```

This can be useful for having different Prometheus servers collect
specific metrics from nodes.

You can also exclude collectors with `exclude[]` (do not combine `collect[]` and
`exclude[]` in the same request).

```
  params:
    exclude[]:
      - hub
```

## Development building and running

Prerequisites:

* [Go compiler](https://golang.org/dl/)
* RHEL/CentOS: `glibc-static` package.

Building:

    git clone https://github.com/rebelcore/kasa_exporter.git
    cd kasa_exporter
    make build
    ./kasa_exporter FLAGS

To see all available configuration flags:

    ./kasa_exporter --help

## Running tests

    make test

To generate a coverage report:

    go test -count=1 ./... -race -covermode=atomic -coverpkg=./... -coverprofile=coverage.out
    go tool cover -func=coverage.out

The protocol tests run against fake devices that perform the real handshakes and
really encrypt, so a mistake in a key derivation fails there in the same way it
would fail against a plug. The derivations themselves are pinned to known
vectors rather than only checked against their own inverses.

## Troubleshooting

**Nothing is discovered.** Discovery is a broadcast and does not cross a router.
Check that the exporter is on the same segment as the devices, that it is not in
a container on a bridge network, and that your access points do not filter
client-to-client broadcast. Where none of that can change, use
`--no-kasa.discovery --kasa.address=...` instead. On macOS, an unsigned binary
may also need Local Network permission before it can broadcast at all.

**A device reports `kasa_device_up 0`.** Run with `--log.level=debug`; the
failure names every transport that was tried and why each one failed.

**"device rejected the supplied credentials."** The device is bound to a
different TP-Link account from the rest of the fleet, which is common when
devices were added by different people. Check which account owns it in the Kasa
app. A rejected login is not retried: it is a standing fault that another
attempt cannot fix.

## TLS endpoint

**EXPERIMENTAL**

The exporter supports TLS via a new web configuration file.

```console
./kasa_exporter --web.config.file=web-config.yml
```

See
the [exporter-toolkit web-configuration](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)
for more details.

## Profiling

**Disabled by default**

The exporter can serve Go [pprof](https://pkg.go.dev/net/http/pprof) profiling
endpoints under `/debug/pprof/` to help debug CPU or memory issues. They are
disabled by default and enabled with the `--web.enable-pprof` flag.

```console
./kasa_exporter --web.enable-pprof
```

When enabled, links to the profiles are also shown on the exporter's landing
page. Avoid exposing these endpoints publicly, as profiles can reveal internal
runtime details.

## Contributing

Pull requests are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the
sign-off requirement and the guidelines on adding a collector or supporting a
new device.

Report bugs and request features through the
[issue tracker](https://github.com/rebelcore/kasa_exporter/issues). For anything
security-sensitive, follow [SECURITY.md](SECURITY.md) instead of opening a
public issue.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
