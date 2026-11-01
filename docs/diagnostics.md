# Diagnostics

[Back to the overview](../README.md)

Everything here only reads. It needs no extra setup, no access to the Kea logs and no
packet capture: it works from the Kea API and, if configured, the two integrations.

## Client details

Click an address or MAC in the leases, reservations or devices table, or in the recent
leases on the dashboard. The tables stay where they are, and a popup shows:

- the lease, the reservation, what the network saw and the WiFi connection,
- a step by step answer to why the client has its address: which subnet, whether a
  reservation applies, where the address comes from, its classes and its lease lifetime,
- every DHCP option it receives, which level sets it, and the values that one
  overrides. From weak to strong: global, client class, shared network, subnet, pool,
  reservation,
- the changes made to this client, from the audit log.

It also names what goes wrong: a reservation Kea ignores because
`reservations-global` or `reservations-in-subnet` is off, a reserved address held by
another client, a client that moves at its next renewal, or a device using an address
inside a pool without a lease.

The explanation is retraced from the configuration, not read from Kea. What depends on
the packet of the client is listed instead of guessed: a class with a `test` only shows
up as "applies if the client matches". A device without a lease outside of the pools
counts as *probably* static, because a foreign DHCP server would look the same.

![Client popup with lease, reservation, network, WiFi and why the client gets its address](screenshots/client.png)

## Health

The Health page runs its checks each time it loads, per server. The number on top opens
the list of all checks, with open points first:

| Area | What is checked |
|---|---|
| Server | Reachable, Kea version, uptime, DHCP sockets |
| HA | State, partner, clock skew, lease updates held back while the partner was away |
| Hooks | Which are loaded and what is missing without them |
| Configuration | Write lock from a lost option, same state on all servers, same Kea release |
| Integrations | arpmonitor and openwrt-ap-dashboard reachable |

The DHCP traffic shows Discover, Offer, Request and Ack as bars, with counters for NAKs,
declined addresses, drops, malformed packets, failed allocations and reservation
conflicts. All of them count since Kea started. Shares are only rated from 50 packets
on, and drops on a standby do not count, because it leaves requests to the primary.

![Health page with checks per Kea server and DHCP traffic](screenshots/health.png)

## Configuration review

Kea starts happily with many mistakes. The review looks for them on every load of the
Health page and lists them worst first. The number above the table opens all rules with
their result. Each finding links to its entry, and a finding that is intended can be
hidden with its button: it stops counting and stays in the filter of the table. This
holds for that rule and that entry only. Hiding is saved in the data volume.

The levels mean:

- **Problem:** something is broken or will not work as configured.
- **Warning:** probably a mistake, worth a look.
- **Note:** can be intended. A note does not change the state of the page.

### Settings of Kea

| Rule | Level | Finds |
|---|---|---|
| Global reservations are switched on in Kea | Problem | Global reservations exist, but `reservations-global` is off, so Kea ignores them |
| Subnet reservations are switched on in Kea | Problem | Subnet reservations exist, but `reservations-in-subnet` is off |

### Reservations

| Rule | Level | Finds |
|---|---|---|
| No address is reserved twice | Problem | Two reservations with one address |
| Reservations lie inside their subnet | Problem | A reserved address outside of the subnet of the reservation |
| Reserved addresses lie in a configured subnet | Warning | A global reservation whose address fits no subnet |
| No hostname is used by two reservations | Warning | One name on several reservations, which breaks DNS names |
| Reservations only use classes that exist | Warning | A class in a reservation that is not configured |
| Reserved addresses lie outside of the pools | Note | A reserved address inside a pool: Kea keeps it for the client, but a dynamic client may hold it first |
| Reservations set no option to the value they inherit | Note | An option with the value it would get anyway |

### Classes

| Rule | Level | Finds |
|---|---|---|
| Every class can apply | Note | A class without a test that no reservation, pool or subnet assigns |

### Subnets and pools

| Rule | Level | Finds |
|---|---|---|
| Pools do not overlap | Problem | Two pools sharing addresses |
| Pools lie inside their subnet | Problem | A pool reaching outside of its subnet |
| Every subnet has a pool or a reservation | Warning | A subnet that serves nobody |
| Gateways lie inside their subnet | Warning | A `routers` address outside of the subnet |
| Every subnet offers a gateway | Note | No `routers` option |
| Every subnet offers a DNS server | Note | No `domain-name-servers` option |

### Network and devices

These rules compare Kea with what the [ARP monitor](devices.md#arpmonitor) and the AP
dashboard report, so they need the ARP monitor. If a source is unreachable or empty, they
say so instead of reporting nothing. Clients the AP dashboard reports count as seen.

"Online" means seen within the online window, 15 minutes by default.

| Rule | Level | Finds |
|---|---|---|
| No address is online on two devices | Problem | Two MAC addresses online with one IP address |
| Reserved addresses are only online on their device | Problem | A device online with an address reserved for another one |
| Leased addresses are only online on their device | Warning | A device online with an address leased to another one |
| Reserved devices are online with their reserved address | Warning | A reserved device online with another address. Only a note if it was also seen with the reserved one, for example with a second address on its interface |
| Leased devices are online with their leased address | Warning | A device online with another address than its lease |
| Online addresses inside a pool have a lease | Warning | A device using a pool address without a lease, so Kea can give it to another client |
| Devices with an active lease are seen on the network | Note | An active lease whose device was not seen for an hour |
| Reserved devices are seen on the network | Note | A reservation whose device was not seen for a year, probably a leftover |

The time spans are set on the [Settings](configuration.md#settings) page. Addresses are
only compared for reservations by MAC address. Devices that sleep or stay silent can show
up as notes.

Warnings in the log of Kea are not part of this, because its API does not offer them.
