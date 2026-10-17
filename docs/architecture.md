# Architecture

The Kasa Exporter is a single Go binary that reads TP-Link Kasa and Tapo devices
on the local network and turns what they report into Prometheus metrics. It
speaks the devices' own protocols directly, without the TP-Link cloud. Each
scrape reads every device once and shares the readings across all the
collectors.

## Components

| Component           | Code                                                                                    | Responsibility                                                                                                                                                                                                                                                                                       |
|---------------------|-----------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Command line        | `kasa_exporter.go`                                                                      | Parses flags and environment variables. The device flags and the collector flags are defined by the packages that use them and joined into one command line.                                                                                                                                        |
| HTTP server         | `kasa_exporter.go`, [exporter-toolkit](https://github.com/prometheus/exporter-toolkit) | Serves the metrics endpoint (`--web.telemetry-path`, `/metrics` by default), a landing page and, when enabled, pprof. exporter-toolkit adds TLS and basic authentication from `--web.config.file`.                                                                                                 |
| Metrics handler     | `handler` in `kasa_exporter.go`                                                         | Serves scrapes from a registry built at start-up. The `collect[]` and `exclude[]` query parameters build a narrower one for a single scrape, and `--web.max-requests` limits parallel scrapes. The exporter's own `go_*`, `process_*` and `promhttp_*` metrics come from a separate registry.       |
| Collector framework | `collector/collector.go`                                                                | Each collector registers itself at start-up and gets a `--[no-]collector.<name>` flag. On each scrape, `KasaCollector` runs the enabled collectors in parallel and reports how long each took and whether it succeeded.                                                                           |
| Collectors          | `collector/*.go`                                                                        | One per kind of device, plus `device` for what every device has in common. Each takes the scrape's shared snapshot of readings, keeps the devices of its kind and emits their metrics.                                                                                                             |
| Configuration       | `config`                                                                                | Holds the device settings and builds the shared device registry on the first scrape, so a configuration that cannot work fails that scrape with the reason rather than stopping the process.                                                                                                    |
| Device registry     | `kasa/registry.go`                                                                      | Owns the fleet: the configured addresses and whatever discovery finds. It reads every device once per scrape, at most `--kasa.concurrency` at a time and each within `--kasa.timeout`, and serves that snapshot to every collector for `--kasa.cache-ttl`.                                    |
| Discovery           | `kasa/discover.go`                                                                      | Broadcasts a sysinfo request on UDP 9999 and a fixed probe on UDP 20002 to `--kasa.discovery-target`, `--kasa.discovery-packets` times per sweep. A reply names the device's transport, port and login version.                                                                                |
| Transports          | `kasa/xor.go`, `kasa/klap.go`, `kasa/aes.go`, `kasa/connect.go`                         | The three ways a device can be reached: the legacy XOR protocol on TCP 9999, KLAP over HTTP and AES secure passthrough over HTTP. `connect.go` picks one from the discovery reply, or tries them newest first for a device that did not answer discovery. KLAP and AES sessions are reused across scrapes. |
| Dialects            | `kasa/iot.go`, `kasa/smart.go`                                                          | The two message formats the transports carry, IOT and SMART, decoded into one protocol-independent `kasa.Reading`, so nothing above the `kasa` package knows which protocol a device speaks.                                                                                                     |

## Collectors

| Collector  | Default | Devices                                                          |
|------------|---------|------------------------------------------------------------------|
| device     | enabled | Every device: reachability, identity, signal strength and uptime |
| plug       | enabled | Single-outlet plugs, with energy for the metered ones            |
| strip      | enabled | Power strips, as a whole and outlet by outlet                    |
| bulb       | enabled | Bulbs and their light settings                                   |
| lightstrip | enabled | Light strips                                                     |
| dimmer     | enabled | Dimmer switches                                                  |
| wallswitch | enabled | Non-dimming wall switches                                        |
| hub        | enabled | Hubs and the sensors paired to them                              |

## What the exporter asks a device

| Dialect | Requests                                                                                                       |
|---------|----------------------------------------------------------------------------------------------------------------|
| IOT     | `system.get_sysinfo` and `emeter.get_realtime`, the latter per outlet on a power strip                         |
| SMART   | `get_device_info`, `get_energy_usage`, `get_current_power` and `get_child_device_list`, batched into one `multipleRequest` |

Every one of them only reads state.

## Request flow

1. Prometheus sends `GET /metrics`, optionally with `collect[]` or `exclude[]`.
2. exporter-toolkit applies TLS and basic authentication, if configured.
3. The handler uses the start-up registry, or builds a narrower one for
   `collect[]` or `exclude[]`, and `KasaCollector` runs the chosen collectors in
   parallel.
4. The first collector to ask the device registry for a snapshot starts one round
   of device queries, and the others wait for it. When a discovery sweep is due it
   runs first: the very first sweep is waited for, and later ones run in the
   background every `--kasa.discovery-interval`.
5. Each device is read over its transport, connecting or handshaking first if it
   has no session, and its reply is decoded into a `kasa.Reading`. A device that
   fails is kept in the snapshot with its error.
6. Each collector emits metrics for its kind of device; `device` reports
   `kasa_device_up` for every device, including the ones that failed.
7. The registry gathers the metrics, adds each collector's duration and success,
   and writes the response.

## Key properties

* **Read-only:** nothing the exporter sends changes a device's state.
* **Local only:** devices are reached directly on the LAN. The TP-Link account is
  used only to authenticate to the devices themselves.
* **One query per device per scrape:** the collectors share the registry's
  snapshot, and the cache TTL keeps a scrape that runs every collector from
  querying the fleet more than once.
* **Failure isolation:** a device that cannot be reached reports
  `kasa_device_up 0` under its last known name, and a collector that fails
  reports `kasa_scrape_collector_success 0` while the others still report.
