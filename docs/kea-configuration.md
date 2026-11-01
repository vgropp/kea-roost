# Kea configuration

[Back to the overview](../README.md)

What Kea needs so that Kea Roost can work with it.

The manager does not change the Kea installation. Tested with Kea 3.0.3 and 3.0.4.
`kea-dhcp4` is expected to have:

- a `control-sockets` entry of type `http` or `https`,
- the hooks `libdhcp_lease_cmds.so` and `libdhcp_host_cmds.so`,
- `libdhcp_ha.so` for the HA page,
- `libdhcp_subnet_cmds.so` to edit subnets on the subnets page,
- `libdhcp_class_cmds.so` to edit client classes on the classes page.

Writes are refused while `config-get` reports a binary option without a value.
Kea does that for `vendor-encapsulated-options` given as hex, and `config-write`
would then store the empty value.

If a hook is missing, the dashboard shows a warning and everything else keeps working.

A configuration with all of them, inside the `Dhcp4` block of `kea-dhcp4.conf`:

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

Add the entries to existing lists instead of creating second ones, and keep the
parameters a hook such as `libdhcp_ha.so` already has. Limit the port to the host of
the manager, because the socket can change the whole configuration. Then set in
`.env`:

    KEA_URL=http://192.0.2.1:8051
    KEA_USER=manager
    KEA_PASSWORD=change-me

Changes rewrite `kea-dhcp4.conf` through `config-write`, so Kea needs write access to
that file. It then contains all default values, and comments in it are lost.
