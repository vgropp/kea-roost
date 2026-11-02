# DNS names for reservations

[Back to the overview](../README.md)

Kea Roost can write a hosts file of all reservations, so that a DNS server resolves the
names you give in the interface. Kea needs no change. With dnsmasq:

    mkdir -p /var/lib/kea-roost-dns
    chown 65532 /var/lib/kea-roost-dns

In `.env`, then `docker compose up -d`:

    DNS_HOSTS_DIR=/var/lib/kea-roost-dns
    DNS_HOSTS_DOMAIN=lan

In the dnsmasq configuration, which then follows the file by itself:

    hostsdir=/var/lib/kea-roost-dns

Every reservation with an address and a name gets a line:

    192.0.2.10 nas.lan nas
    fd00:db8:1::10 nas.lan nas

The IPv6 line comes from an [IPv6](ipv6.md) reservation. If that one is by MAC and has
no name, it takes the name of the IPv4 reservation of the same MAC.

- `DNS_HOSTS_DOMAIN=lan,home` resolves `nas`, `nas.lan` and `nas.home`; reverse lookups
  answer with the first domain. A name with a domain stays as it is.
- Characters DNS does not allow become a dash: `living room tv` is `living-room-tv`.
- A device with a static address gets its name by a reservation as well; it never asks,
  so the reservation does no harm.
- Leases are left out on purpose: a client picks that name itself and could take the
  name of another device.

The file is rewritten after every change and checked every minute, which catches changes
made in Kea directly. If Kea is unreachable, the old file stays and the Health page says
so. CoreDNS reads the file with its `hosts` plugin.
