# DNS names for reservations

[Back to the overview](../README.md)

Kea Roost can keep a hosts file of all reservations, so that a DNS server resolves the
names you give in the interface. It needs no change to Kea and no script. With dnsmasq:

    mkdir -p /var/lib/kea-roost-dns
    chown 65532 /var/lib/kea-roost-dns

Add this to `.env` and run `docker compose up -d`:

    DNS_HOSTS_DIR=/var/lib/kea-roost-dns
    DNS_HOSTS_DOMAIN=lan

And this to the dnsmasq configuration, which then follows the file by itself:

    hostsdir=/var/lib/kea-roost-dns

The file has a line for every reservation with an address and a name:

    192.0.2.10 nas.lan nas

With `DNS_HOSTS_DOMAIN=lan,home` a name resolves as `nas`, `nas.lan` and `nas.home`.
Reverse lookups answer with the first domain. A name that already has a domain stays as
it is. Characters DNS does not allow become a dash, so `living room tv` resolves as
`living-room-tv`.

A device with a static address gets its name the same way: create a reservation for it.
It does no harm, because the device never asks. Leases are left out on purpose, because
a client picks that name itself and could claim the name of another device.

The file is rewritten right after a change and checked every minute, which catches
changes made in Kea directly. If Kea cannot be reached, the old file stays, and the
Health page says so. Other hosts files work as well: the `hosts` plugin of CoreDNS can
read it, for example.
