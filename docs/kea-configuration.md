# Kea configuration

[Back to the overview](../README.md)

What Kea needs so that Kea Roost can work with it. Kea Roost changes nothing in the
installation itself.

## kea-dhcp4

| Needed | For |
|---|---|
| a `control-sockets` entry of type `http` or `https` | everything |
| `libdhcp_lease_cmds.so`, `libdhcp_host_cmds.so` | leases and reservations |
| `libdhcp_subnet_cmds.so` | editing subnets |
| `libdhcp_class_cmds.so` | editing client classes |
| `libdhcp_ha.so` | HA state and actions |
| write access of the Kea user to `kea-dhcp4.conf` | saving changes |

Without a hook, the dashboard warns and the rest keeps working. Inside the `Dhcp4` block:

```json
"control-sockets": [
  {
    "socket-type": "http",
    "socket-address": "192.0.2.1",
    "socket-port": 8051,
    "authentication": {
      "type": "basic",
      "clients": [ { "user": "manager", "password": "change-me" } ]
    }
  }
],
"hooks-libraries": [
  { "library": "libdhcp_lease_cmds.so" },
  { "library": "libdhcp_host_cmds.so" },
  { "library": "libdhcp_subnet_cmds.so" },
  { "library": "libdhcp_class_cmds.so" }
]
```

Add the entries to existing lists, and keep the parameters a hook such as
`libdhcp_ha.so` already has. The socket can change the whole configuration, so let only
the host of Kea Roost reach its port. In `.env`:

    KEA_URL=http://192.0.2.1:8051
    KEA_USER=manager
    KEA_PASSWORD=change-me

Changes are saved with `config-write`: Kea rewrites its file with all default values,
and comments are lost. Editing a subnet keeps the reservations inside it. Writes are
refused while a binary option has no value, such as `vendor-encapsulated-options` given
as hex, because `config-write` would erase it.

## Global options

Options for every client, such as `domain-name-servers`, are edited above the subnets
on the Network page. A client class, subnet, pool or reservation that sets the same
option wins over them. A subnet that only inherits its gateway or DNS servers shows
them with the level they come from, and its edit form leaves the field empty with the
inherited value as a hint: saving never copies an inherited value into the subnet, and
clearing the field lets the subnet inherit again.

Kea has no command for this part of its configuration, so Kea Roost sends the whole
configuration with `config-set`, to the primary first and then to each peer, each
with its own configuration. Each server reloads completely and answers no DHCP request
for a few seconds; an HA pair synchronizes its leases again. Leases and reservations
stay, and Kea refuses an invalid option without changing anything. The dialog says
this before saving.

From Kea 3.0.4 on, Kea Roost checks the configuration with `config-test` first. Older
releases skip it, because there `config-test` left the HA listener closed (ISC issues
#4145 and #4616), and the dialog names those servers. Each server is then given time
to return to its normal HA state before the next one reloads, so the partners of a
pair never reload at once.

The configuration sent back holds what `config-get` returned, including the passwords
of databases and hooks in plain text, as Kea 3.0 reports them. Global options in
another option space, such as vendor options, cannot be edited here.

## kea-dhcp6

For [IPv6](ipv6.md), `kea-dhcp6` needs the same: a control socket with the same login on
a port of its own, such as 8052, `libdhcp_lease_cmds.so`, `libdhcp_host_cmds.so`,
`libdhcp_subnet_cmds.so` for editing subnets, and write access to `kea-dhcp6.conf`. A
subnet for clients on a local link also needs `"interface"`, or Kea cannot tell which
subnet a request belongs to.
