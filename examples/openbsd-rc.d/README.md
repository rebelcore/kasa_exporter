# OpenBSD rc.d Script

The script goes in `/etc/rc.d/kasa_exporter` and the binary at
`/usr/local/bin/kasa_exporter`, which is where OpenBSD keeps ports-installed
software.

It runs as a dedicated unprivileged `_kasa_exporter` account, following the
convention for daemons on OpenBSD, so that account has to exist first:

    doas useradd -s /sbin/nologin -d /nonexistent -L daemon _kasa_exporter
    doas cp kasa_exporter /etc/rc.d/
    doas chmod +x /etc/rc.d/kasa_exporter
    doas rcctl enable kasa_exporter
    doas rcctl start kasa_exporter

Flags are in `daemon_flags` in the script, and can be overridden without editing
it:

    doas rcctl set kasa_exporter flags --web.listen-address=:9498
