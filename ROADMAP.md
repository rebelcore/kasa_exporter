# Roadmap

Where the Kasa Exporter is heading over the next year, through October 2027.
Plans can change; to suggest something,
[open an issue](https://github.com/rebelcore/kasa_exporter/issues).

## Planned

* **Keep up with TP-Link firmware.** Follow changes to the device protocols, such
  as new KLAP login versions, and recognise new models as they are reported.
* **Test against more hardware.** Turn the device responses that come with bug
  reports into fixtures for the fake-device tests, so models beyond the EP25 and
  HS300 are covered by CI and not only by the code paths they share.
* **Verifiable releases.** Sign release checksums and container images, make
  release binaries bit-for-bit reproducible from a tagged commit, and publish
  build provenance.
* **Dashboard in step.** Keep the
  [Grafana dashboard](https://github.com/rebelcore/kasa_grafana) in line with
  metric changes.
* **Ongoing upkeep.** Stay on current Go and dependency versions, and keep CI,
  linting, fuzzing and vulnerability scanning running on every change.

## Not planned

* Controlling devices: the exporter stays read-only, and nothing in it switches a
  relay or changes a light.
* Talking to the TP-Link cloud. The exporter reaches devices on the local
  network only.
* Outputs other than the Prometheus exposition format.
