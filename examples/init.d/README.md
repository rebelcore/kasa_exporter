# SysV Init Script

For distributions still using SysV init rather than systemd. The script sources
`/etc/rc.d/init.d/functions`, so it targets the RHEL/CentOS family.

Put the script in `/etc/rc.d/init.d/kasa_exporter` and the binary at
`/usr/sbin/kasa_exporter`.

    sudo cp kasa_exporter /etc/rc.d/init.d/
    sudo chmod +x /etc/rc.d/init.d/kasa_exporter
    sudo chkconfig --add kasa_exporter
    sudo service kasa_exporter start

Flags are set in the `OPTIONS` variable at the top of the script. It defaults to
listening on port 9498 and to finding devices by broadcast discovery; where
broadcast cannot reach them, add `--no-kasa.discovery --kasa.address=...`.
