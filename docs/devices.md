# Devices

[Back to the overview](../README.md)

![Devices on the network with reservation state, WiFi access point and last seen time](screenshots/devices.png)

The devices page shows what is on the network and has no reservation for its MAC or
address; a filter also shows the reserved ones. **Reserve** opens the reservation
dialog with address, MAC and name filled in. Devices are marked current (seen within
the last 15 minutes), recent (last day) or stale, newest first, and an address used by
another device than the reserved one is marked as a MAC mismatch.

The page gets its data from two optional integrations. It needs at least one of them,
and it does not exist without any. Only devices inside a configured subnet are listed.
If a source is unreachable, only its information is missing.

## arpmonitor

[arpmonitor](https://github.com/vgropp/arpmonitor) records MAC and IP addresses from
ARP traffic, so it also finds wired and static devices that Kea knows nothing about.
It resolves names through DNS and knows when a device was first and last seen. Run it
with `--arp-scan-interval=5m` to also find silent devices and to make the last seen
time reflect what is online right now.

`ARPMONITOR_URL` is the base URL, for example `http://host.docker.internal:8253`; Kea
Roost appends the API path itself. The [Settings](configuration.md#settings) page sets how far back each
query looks. How long data is kept is decided by the `--retention-days` flag of the
monitor, so a larger window than that shows little more.

## openwrt-ap-dashboard

[openwrt-ap-dashboard](https://github.com/vgropp/openwrt-ap-dashboard) collects the
WiFi clients of several OpenWrt access points. With `APDASHBOARD_URL` set, the devices
and leases pages show the access point and signal of WiFi clients, with the network in
the tooltip. A connected client counts as seen now, and clients the monitor does not
know get their own entry. Kea Roost only reads from the dashboard.

## Leases and reservations

Leases and reservations stay in their own tables, with links between them.

![Table of Kea DHCP leases with search, filters and MAC vendors](screenshots/leases.png)

![Table of Kea DHCP host reservations with client classes](screenshots/reservations.png)

An address conflict shows while typing in the dialog.

![Dialog for a new DHCP reservation showing an address conflict](screenshots/reservation-dialog.png)
