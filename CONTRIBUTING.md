# Contributing

Rebel Media uses GitHub to manage reviews of pull requests.

* If you have a trivial fix or improvement, go ahead and create a pull request,
  addressing (with `@...`) the maintainer of this repository (see
  [MAINTAINERS.md](MAINTAINERS.md)) in the description of the pull request.

* Relevant coding style guidelines are the [Go Code Review
  Comments](https://code.google.com/p/go-wiki/wiki/CodeReviewComments)
  and the _Formatting and style_ section of Peter Bourgon's [Go: Best
  Practices for Production
  Environments](http://peter.bourgon.org/go-in-production/#formatting-and-style).

* Sign your work to certify that your changes were created by yourself, or you
  have the right to submit it under our license. Read
  https://developercertificate.org/ for all details and append your sign-off to
  every commit message like this:

        Signed-off-by: Random J Developer <example@example.com>

## Collector Implementation Guidelines

The Kasa Exporter is not a general monitoring agent. Its sole purpose is to
expose metrics from TP-Link Kasa and Tapo devices, as opposed to controlling
them: nothing in this exporter switches a relay or changes a light.

There is one collector per kind of device, and every collector is enabled by
default. A new collector belongs here when it covers a kind of hardware the
exporter cannot already report; a new reading for hardware that is already
covered belongs in the collector that owns it.

Adding support for a device that reports something new usually means three
changes: decode the field in the dialect that carries it (`kasa/iot.go` or
`kasa/smart.go`), add it to `kasa.Reading`, and emit it from the collector for
that kind of device. Nothing above the `kasa` package should need to know which
transport a device speaks.

## Testing protocol changes

The protocol tests run against fake devices in `kasa/fakedevice_test.go` that
perform the real handshakes and really encrypt. Prefer extending those over
mocking a transport: a fake that only replays a recorded response cannot catch a
mistake in a key derivation, which is the class of bug that costs the most time
to find against real hardware.

Key derivations are pinned to vectors taken from the reference implementation
rather than checked against their own inverses, since a round-trip test passes
just as happily on the wrong algorithm.
