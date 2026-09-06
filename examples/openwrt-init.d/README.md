# OpenWrt Init Script

OpenWrt runs the exporter under procd. The script goes in
`/etc/init.d/kasa_exporter` and the binary at `/usr/bin/kasa_exporter`.

    cp kasa_exporter /etc/init.d/
    chmod +x /etc/init.d/kasa_exporter
    /etc/init.d/kasa_exporter enable
    /etc/init.d/kasa_exporter start

Flags are set in the `OPTIONS` variable at the top of the script.

Running on the router itself is the case discovery is happiest with: it puts the
exporter on the same segment as the devices, so the UDP broadcast reaches them
without any addresses being configured by hand.
