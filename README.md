# Kea Roost: Web UI for the ISC Kea DHCP Server

[![CI](https://github.com/vgropp/kea-roost/actions/workflows/test.yml/badge.svg)](https://github.com/vgropp/kea-roost/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Kea Roost is a lightweight, self-hosted web interface and dashboard for the
[ISC Kea](https://www.isc.org/kea/) DHCPv4 server. Manage DHCP leases and reservations,
client classes, subnets and pools in the browser, keep an HA pair in step and operate
it, find out why a client gets its address, and see which devices on the network have
no reservation yet.

It is a single Go binary with the frontend embedded, runs in Docker, and needs no
database or agents. It talks to Kea exclusively through the Kea management API and
takes no part in DHCP operation: if it stops, Kea keeps serving.

Tested with Kea 3.0.3 and 3.0.4, on its own or as a hot-standby HA pair.
Kea Roost is an independent project, not affiliated with or endorsed by ISC, and is
licensed under the [MIT License](LICENSE).

## Why Kea Roost

Kea has no web interface of its own and is configured through JSON files and an API.
[ISC Stork](https://www.isc.org/stork/) is the official dashboard for larger
deployments, with agents and a PostgreSQL database. Kea Roost is meant for a home lab
or a small network: one container, no database, and everyday tasks such as turning a
lease into a static DHCP reservation in two clicks.

## Features

- **Leases and reservations:** search, filters, MAC vendors, change the MAC, classes and DHCP
  options, and a reservation from a lease in two clicks.
- **Client classes and subnets:** edit classes, pools, gateway, DNS servers and lease lifetime.
- **[Address map](docs/address-map.md):** every address at a glance, and the free ranges.
- **[Several Kea servers and HA](docs/several-servers.md):** every change reaches all servers,
  differences show up, and HA can be operated: maintenance, sync, reset.
- **[Diagnostics](docs/diagnostics.md):** why a client gets its address, health of all servers,
  DHCP traffic, and a review of the configuration against the real network.
- **[Devices](docs/devices.md):** everything on the network, with
  [arpmonitor](https://github.com/vgropp/arpmonitor) and
  [openwrt-ap-dashboard](https://github.com/vgropp/openwrt-ap-dashboard).
- **[DNS names](docs/dns-names.md):** a hosts file of all reservations for dnsmasq or CoreDNS.
- **[Prometheus metrics](docs/metrics.md):** compatible with the common Kea exporters.
- **Audit log and backups:** every change with its user and result.
- **Interface:** Classic and Modern themes in light and dark, usable on a phone, without
  external resources or telemetry.

## Screenshots

The pictures show invented devices.

| Dashboard | Address map |
|---|---|
| ![Kea Roost dashboard with DHCP server state, HA state, recent leases and pool utilization](docs/screenshots/dashboard.png) | ![Address map of a subnet with reserved, leased and free addresses](docs/screenshots/subnets.png) |

| Devices | Client details |
|---|---|
| ![Devices on the network with reservation state, WiFi access point and last seen time](docs/screenshots/devices.png) | ![Client popup with lease, reservation, network, WiFi and why the client gets its address](docs/screenshots/client.png) |

## Install with Docker Compose

Images for `linux/amd64` and `linux/arm64` are published to the GitHub Container Registry
as `ghcr.io/vgropp/kea-roost`:

    curl -LO https://raw.githubusercontent.com/vgropp/kea-roost/main/compose.yaml
    curl -L -o .env https://raw.githubusercontent.com/vgropp/kea-roost/main/.env.example

In `compose.yaml`, uncomment the `image` line and delete the `build` and `image` lines
below it. Set `KEA_URL` and the passwords in `.env`, then start it:

    docker compose up -d

`latest` follows the newest release. To stay on one, set `KEA_ROOST_VERSION=0.8` for a
release line or `KEA_ROOST_VERSION=0.8.0` for exactly one version in `.env`.
`KEA_ROOST_VERSION=snapshot` follows every tested commit on `main`, for trying out what
comes next.

To build from source instead, keep `compose.yaml` as it is:

    git clone https://github.com/vgropp/kea-roost
    cd kea-roost
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

## Documentation

| Page | Content |
|---|---|
| [Configuration](docs/configuration.md) | Environment, Settings page, reverse proxy, MAC vendors, themes |
| [Kea configuration](docs/kea-configuration.md) | What Kea needs: control socket and hooks |
| [Several servers and HA](docs/several-servers.md) | Peers, comparing, operating HA |
| [Address map](docs/address-map.md) | Free ranges and how they are found |
| [Diagnostics](docs/diagnostics.md) | Client details, health, configuration review |
| [Devices](docs/devices.md) | arpmonitor and openwrt-ap-dashboard |
| [DNS names](docs/dns-names.md) | Hosts file for a DNS server |
| [Prometheus metrics](docs/metrics.md) | Metrics and their names |
| [Development](docs/development.md) | Build and test without Docker |
