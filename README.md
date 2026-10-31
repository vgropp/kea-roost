# Kea Roost: Web UI for the ISC Kea DHCP Server

[![CI](https://github.com/vgropp/kea-roost/actions/workflows/test.yml/badge.svg)](https://github.com/vgropp/kea-roost/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Kea Roost is a lightweight, self-hosted web interface and dashboard for the
[ISC Kea](https://www.isc.org/kea/) DHCPv4 server. Manage DHCP leases and reservations,
client classes, subnets and pools in the browser, see your HA state, and find
devices on the network that have no reservation yet.

It is a single Go binary with the frontend embedded, runs in Docker, and needs no
database or agents. It talks to Kea exclusively through the Kea management API and
takes no part in DHCP operation: if it stops, Kea keeps serving.

Tested with Kea 3.0.3 and 3.0.4. It currently manages exactly one Kea instance.
Kea Roost is an independent project, not affiliated with or endorsed by ISC, and is
licensed under the [MIT License](LICENSE).

## Why Kea Roost

Kea has no web interface of its own and is configured through JSON files and an API.
[ISC Stork](https://www.isc.org/stork/) is the official dashboard for larger
deployments, with agents and a PostgreSQL database. Kea Roost is meant for a home lab
or a small network: one container, no database, and everyday tasks such as turning a
lease into a static DHCP reservation in two clicks.

## Features

- **Dashboard:** server and HA state, lease, reservation and subnet counts, pool
  utilization, warnings, and the most recent leases.
- **Leases:** search, filters, sorting, automatic refresh, MAC vendors, deleting a
  lease, and creating a reservation from a lease.
- **Reservations:** create, edit and delete global and subnet reservations, change
  the MAC, assign client classes and DHCP options, comments, and links to the lease.
- **Client classes:** create, edit and delete classes and their DHCP options.
- **Subnets:** gateway, DNS servers, lease lifetime and pools with their utilization,
  and editing all of them.
- **HA:** state of this server and its partner.
- **Devices:** everything on the network with and without a reservation, how recently
  it was seen, and addresses used by the wrong device.
- **[arpmonitor](https://github.com/vgropp/arpmonitor) integration (optional):**
  finds wired, static and otherwise silent devices through ARP, including DNS names and
  when a device was first and last seen. An active ARP scan shows what is online now.
- **[openwrt-ap-dashboard](https://github.com/vgropp/openwrt-ap-dashboard) integration
  (optional):** shows the access point, network and signal of WiFi clients on the
  devices and leases pages.
- **Audit log:** every change with its user and result, and downloadable backups.
- **Safety checks:** see [How changes are applied](#how-changes-are-applied).
- **Interface:** Classic and Modern themes in light and dark, usable on a phone,
  without external resources or telemetry.

Editing subnets and client classes needs the matching Kea hooks, see
[Kea configuration](#kea-configuration). The devices page needs at least one of the two
integrations, see [Devices](#devices).

## Screenshots

The pictures show invented devices.

| Dashboard (Modern, dark) | Devices (Modern, dark) |
|---|---|
| ![Kea Roost dashboard with DHCP server state, HA state, recent leases and pool utilization](docs/screenshots/dashboard.png) | ![Devices on the network with reservation state, WiFi access point and last seen time](docs/screenshots/devices.png) |

| Leases (Classic, light) | Subnets (Classic, light) |
|---|---|
| ![Table of Kea DHCP leases with search, filters and MAC vendors](docs/screenshots/leases.png) | ![Kea subnet with gateway, DNS servers and pool utilization](docs/screenshots/subnets.png) |

| Reservations (Classic, light) | New reservation with an address conflict |
|---|---|
| ![Table of Kea DHCP host reservations with client classes](docs/screenshots/reservations.png) | ![Dialog for a new DHCP reservation showing an address conflict](docs/screenshots/reservation-dialog.png) |

## Install with Docker Compose

    go run ./cmd/oui-update  # fetches the vendor table, needed once
    cp .env.example .env     # then set KEA_URL and the passwords
    docker compose up -d --build

The interface is served on port 8080 (`MANAGER_PORT`). Backups and the audit log
live in the `data` volume. The container runs as an unprivileged user with a
read-only root filesystem.

The container must be able to reach the Kea control socket. If Kea runs on the
Docker host, use `host.docker.internal` in `KEA_URL`. `127.0.0.1` would mean the
container itself. Kea must then listen on an address the container can reach,
not on `127.0.0.1`. The service runs on Docker's default bridge network because
hosts can block project networks from reaching local services.

## Build and test without Docker

The vendor table is not part of the repository. Fetch it once before the first
build, also before building the Docker image:

    go run ./cmd/oui-update

Then:

    go build -o kea-roost ./cmd/kea-roost
    go test ./...

To work on the interface without a real server, run it against the test double:

    go run ./cmd/fake-kea -listen 127.0.0.1:8000 &
    KEA_URL=http://127.0.0.1:8000 MANAGER_USER=admin MANAGER_PASSWORD=admin \
      MANAGER_DATA_DIR=./data go run ./cmd/kea-roost

To also run the browser tests, which need a local Chromium or Chrome:

    go test -tags e2e ./internal/web

The integration tests run the manager against the real `kea-dhcp4` binary. Each
test starts a private server with a synthetic configuration, without any DHCP
interface, and restarts it to check what was stored. They are skipped if Kea is not
installed:

    go test -tags kea ./internal/manager

The screenshots above are written by a test from invented data:

    SCREENSHOT_DIR=docs/screenshots go test -tags e2e -run TestScreenshots ./internal/web

## Configuration

All settings come from environment variables.

| Variable | Meaning | Default |
|---|---|---|
| `KEA_URL` | URL of the Kea HTTP control socket | required |
| `KEA_USER`, `KEA_PASSWORD` | Basic auth of the socket | empty |
| `KEA_CA_FILE` | CA for HTTPS | system CAs |
| `KEA_CLIENT_CERT`, `KEA_CLIENT_KEY` | Client certificate (for `cert-required`) | empty |
| `KEA_SERVER_NAME` | Display name of the server | `kea` |
| `MANAGER_LISTEN` | Listen address | `:8080` |
| `MANAGER_USER`, `MANAGER_PASSWORD` | Login of the interface | empty |
| `APDASHBOARD_URL` | Base URL of an [openwrt-ap-dashboard](https://github.com/vgropp/openwrt-ap-dashboard), adds WiFi clients to leases and devices | empty |
| `MANAGER_BASE_PATH` | Subpath behind a reverse proxy that does not strip it, e.g. `/kea-roost` | empty |
| `MANAGER_DATA_DIR` | Backups and audit log | `/var/lib/kea-roost`, `/data` in the image |
| `ARPMONITOR_URL` | Base URL of an [arpmonitor](https://github.com/vgropp/arpmonitor) with port and without path, enables the devices page | empty |
| `ARPMONITOR_DAYS` | How many days back the devices page lists devices (optional) | `7` |

`KEA_PASSWORD` and `MANAGER_PASSWORD` can instead be read from a file through
`KEA_PASSWORD_FILE` and `MANAGER_PASSWORD_FILE`, for example a Docker secret.

Without a login the manager runs read-only: changes, backups and the audit log
are locked. The interface itself only speaks HTTP, so TLS belongs in a reverse
proxy in front of it. Do not expose the manager unprotected: backups contain the
Kea configuration including plaintext passwords.

## Kea configuration

The manager does not change the Kea installation. Tested with Kea 3.0.3 and 3.0.4.
`kea-dhcp4` is expected to have:

- a `control-sockets` entry of type `http` or `https`,
- the hooks `libdhcp_lease_cmds.so` and `libdhcp_host_cmds.so`,
- `libdhcp_ha.so` for the HA view,
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

## Devices

The devices page shows what is on the network and has no reservation for its MAC or
address; a filter also shows the reserved ones. **Reserve** opens the reservation
dialog with address, MAC and name filled in. Devices are marked current (seen within
the last 15 minutes), recent (last day) or stale, newest first, and an address used by
another device than the reserved one is marked as a MAC mismatch.

The page gets its data from two optional integrations. It needs at least one of them,
and it does not exist without any. Only devices inside a configured subnet are listed.
If a source is unreachable, only its information is missing.

### arpmonitor

[arpmonitor](https://github.com/vgropp/arpmonitor) records MAC and IP addresses from
ARP traffic, so it also finds wired and static devices that Kea knows nothing about.
It resolves names through DNS and knows when a device was first and last seen. Run it
with `--arp-scan-interval=5m` to also find silent devices and to make the last seen
time reflect what is online right now.

`ARPMONITOR_URL` is the base URL, for example `http://host.docker.internal:8253`; Kea
Roost appends the API path itself. `ARPMONITOR_DAYS` only sets the window of each
query. How long data is kept is decided by the `--retention-days` flag of the monitor,
so a larger window than that shows little more.

### openwrt-ap-dashboard

[openwrt-ap-dashboard](https://github.com/vgropp/openwrt-ap-dashboard) collects the
WiFi clients of several OpenWrt access points. With `APDASHBOARD_URL` set, the devices
and leases pages show the access point and signal of WiFi clients, with the network in
the tooltip. A connected client counts as seen now, and clients the monitor does not
know get their own entry. Kea Roost only reads from the dashboard.

## MAC vendors

MAC addresses show their vendor as a tooltip, and the search covers it too.
Locally assigned addresses, as used by phones, bridges and VMs, are marked
`local`. The
vendor table is embedded in the binary, so no internet access is needed at
runtime. `go run ./cmd/oui-update` refreshes it from the IEEE data published by
Wireshark.

## Behind a reverse proxy

The manager can be served from a subpath such as `/kea-roost/`, because the
interface only uses relative URLs. The URL needs the trailing slash, or the proxy
has to redirect to it.

- If the proxy strips the subpath, nothing has to be set.
- If it forwards the path unchanged, set `MANAGER_BASE_PATH` to the same subpath.

For writes to pass the cross-origin check, the proxy has to keep the original
`Host` header or send `X-Forwarded-Host`. The login of the manager uses Basic Auth
as well, so the proxy must not demand a login of its own for this path.

## Themes

The header offers two themes, the dense and square Classic and the round and airy
Modern. Each comes in light and dark, and Auto follows the system setting. The
browser remembers the choice.

