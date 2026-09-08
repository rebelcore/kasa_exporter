# Changelog

## 1.0.1

* [BUGFIX] A strip whose outlets could not all be read no longer publishes a partial cumulative energy total, which Prometheus read as a counter reset

## 1.0.0

* [FEATURE] Prometheus exporter for TP-Link Kasa and Tapo devices, listening on port 9498
* [FEATURE] One collector per kind of device — `device`, `plug`, `strip`, `bulb`, `lightstrip`, `dimmer`, `wallswitch`, `hub` — all enabled by default
* [FEATURE] Native implementation of all three device transports: legacy XOR on TCP 9999, KLAP over HTTP (login versions 1 and 2), and AES secure passthrough
* [FEATURE] Broadcast device discovery on UDP 9999 and 20002, with the transport, port and login version taken from what each device advertises
* [FEATURE] Per-outlet metrics for power strips, with a strip's own figures derived from its outlets
* [FEATURE] Hub child sensors: battery, temperature, humidity and signal strength
* [FEATURE] `collect[]` and `exclude[]` scrape parameters for filtering collectors
* [FEATURE] TLS and basic auth via `--web.config.file`, and optional pprof endpoints via `--web.enable-pprof`
* [ENHANCEMENT] A device is queried once per scrape and shared across every collector, rather than once per collector
* [ENHANCEMENT] An HS300 on hardware 2.0 authenticates with the KLAP login version its firmware advertises, instead of failing with what reads as a wrong password
* [ENHANCEMENT] A device that cannot be reached reports `kasa_device_up 0` under its last known name rather than disappearing from the endpoint
* [ENHANCEMENT] A quantity the hardware does not measure is left unexported instead of reported as zero
* [ENHANCEMENT] Sessions are reused across scrapes and rebuilt automatically after a device reboots
* [ENHANCEMENT] Optional methods a device does not answer are dropped after the first miss, so a plug with no meter is not asked for one on every scrape
