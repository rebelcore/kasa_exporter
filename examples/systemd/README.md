# Systemd Unit

The unit files (`*.service` and `*.socket`) go in `/etc/systemd/system`.

The binary is expected at `/usr/sbin/kasa_exporter`, and the service runs as a
`kasa_exporter` account whose shell should be `/sbin/nologin` and which needs no
special privileges.

## Environment file: RHEL and Debian differ

The two families put service environment files in different places, so the unit
reads **both** and treats each as optional:

| Family | Path | Sample in this directory |
|--------|------|--------------------------|
| RHEL, CentOS, Fedora, Rocky, Alma | `/etc/sysconfig/kasa_exporter` | `sysconfig.kasa_exporter` |
| Debian, Ubuntu | `/etc/default/kasa_exporter` | `default.kasa_exporter` |

The two samples are identical; install whichever matches your distribution. One
unit file works on both, and a missing file is not an error — the `-` prefix on
`EnvironmentFile=` is what makes it optional, and without it the service would
refuse to start on a host that has no such file.

## Socket activation

The service is socket-activated: `kasa_exporter.socket` owns the listening port
and the service starts on the first connection to it, which is why the unit
passes `--web.systemd-socket` rather than `--web.listen-address`. **Change the
port in the socket unit, not the service**, and enable the socket rather than
the service.

## RHEL family

    sudo useradd --system --no-create-home --shell /sbin/nologin kasa_exporter
    sudo cp kasa_exporter.service kasa_exporter.socket /etc/systemd/system/
    sudo install -m 0600 -o kasa_exporter sysconfig.kasa_exporter /etc/sysconfig/kasa_exporter
    sudo systemctl daemon-reload
    sudo systemctl enable --now kasa_exporter.socket

## Debian family

    sudo adduser --system --no-create-home --group kasa_exporter
    sudo cp kasa_exporter.service kasa_exporter.socket /etc/systemd/system/
    sudo install -m 0600 -o kasa_exporter default.kasa_exporter /etc/default/kasa_exporter
    sudo systemctl daemon-reload
    sudo systemctl enable --now kasa_exporter.socket

## Checking it

    systemctl status kasa_exporter.socket kasa_exporter.service
    journalctl -u kasa_exporter -f
    curl -s localhost:9498/metrics | head

## Hardening

The unit drops every capability and makes the filesystem read-only, which the
exporter tolerates because it writes nothing and needs no privilege to open its
sockets — device discovery is an ordinary UDP broadcast. If you add flags that
need to write (a `--web.config.file` with TLS keys is only read, so it does
not), review `ProtectSystem=strict` first.

Verify the sandbox on your own host with:

    systemd-analyze security kasa_exporter.service

## Networking

Discovery is a UDP broadcast, so the host has to be on the same network segment
as the devices. Where it is not, set `--no-kasa.discovery` and `--kasa.address`
in the environment file instead.
