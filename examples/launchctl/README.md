# MacOS LaunchDaemon

If you're installing through a package manager, you probably don't need to deal
with this file.

The `plist` file should be put in `/Library/LaunchDaemons/` (user defined daemons), and the binary installed at
`/usr/local/bin/kasa_exporter`.

Ex. install globally by

    sudo cp -n kasa_exporter /usr/local/bin/
    sudo cp -n examples/launchctl/io.prometheus.kasa_exporter.plist /Library/LaunchDaemons/
    sudo launchctl bootstrap system/ /Library/LaunchDaemons/io.prometheus.kasa_exporter.plist

    # Optionally configure by dropping CLI arguments in a file
    echo -- '--web.listen-address=:9498' | sudo tee /usr/local/etc/kasa_exporter.args

    # Check it's running
    sudo launchctl list | grep kasa_exporter

    # See full process state
    sudo launchctl print system/io.prometheus.kasa_exporter

    # View logs
    sudo tail /tmp/kasa_exporter.log
