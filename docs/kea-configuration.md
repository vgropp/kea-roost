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

## kea-dhcp6

For [IPv6](ipv6.md), `kea-dhcp6` needs the same: a control socket with the same login on
a port of its own, such as 8052, `libdhcp_lease_cmds.so`, `libdhcp_host_cmds.so`,
`libdhcp_subnet_cmds.so` for editing subnets, and write access to `kea-dhcp6.conf`. A
subnet for clients on a local link also needs `"interface"`, or Kea cannot tell which
subnet a request belongs to.
