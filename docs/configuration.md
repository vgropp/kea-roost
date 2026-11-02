# Configuration

[Back to the overview](../README.md)

## Configuration

All settings come from environment variables.

| Variable | Meaning | Default |
|---|---|---|
| `KEA_URL` | URL of the Kea HTTP control socket | required |
| `KEA_USER`, `KEA_PASSWORD` | Basic auth of the socket | empty |
| `KEA_CA_FILE` | CA for HTTPS | system CAs |
| `KEA_CLIENT_CERT`, `KEA_CLIENT_KEY` | Client certificate (for `cert-required`) | empty |
| `KEA_SERVER_NAME` | Display name of the server | `kea` |
| `KEA_PEERS` | Further Kea servers as `name=url,name=url`, same credentials as `KEA_URL` | empty |
| `METRICS_ENABLED` | Set to `false` to remove `/metrics` | `true` |
| `METRICS_USER`, `METRICS_PASSWORD` | Login for `/metrics`, see [Prometheus metrics](metrics.md) | open |
| `DNS_HOSTS_DIR` | Folder for a hosts file of all reservations, see [DNS names](dns-names.md) | off |
| `DNS_HOSTS_DOMAIN` | Domains added to reservation names without one, comma separated | empty |
| `MANAGER_LISTEN` | Listen address | `:8080` |
| `APDASHBOARD_URL` | Base URL of an [openwrt-ap-dashboard](https://github.com/vgropp/openwrt-ap-dashboard), adds WiFi clients to leases and devices | empty |
| `MANAGER_BASE_PATH` | Subpath behind a reverse proxy that does not strip it, e.g. `/kea-roost` | empty |
| `MANAGER_TRUSTED_PROXIES` | Addresses or networks of reverse proxies whose `X-Forwarded-For` names the client, comma separated, e.g. `192.0.2.10` | empty |
| `MANAGER_DATA_DIR` | Accounts, settings, backups and audit log | `/var/lib/kea-roost`, `/data` in the image |
| `ARPMONITOR_URL` | Base URL of an [arpmonitor](https://github.com/vgropp/arpmonitor) with port and without path, enables the devices page | empty |

`KEA_PASSWORD` and `METRICS_PASSWORD` can instead be read from a file through
`KEA_PASSWORD_FILE` and `METRICS_PASSWORD_FILE`, for example a Docker secret.

The accounts of the interface are not part of the environment, see
[Accounts](accounts.md). The interface itself only speaks HTTP, so TLS belongs in a
reverse proxy in front of it. Do not expose the manager unprotected: backups contain the
Kea configuration including plaintext passwords.

## Settings

![Settings page with time windows and thresholds](screenshots/settings.png)

The Settings page holds limits and time windows: how far back the devices page and the
review look, when a device counts as online, and the thresholds of warnings. They are
saved in `settings.json` in the data volume and apply at once. Values that are not
changed there keep their default. Below them, admins manage the [accounts](accounts.md).

Addresses, logins and other settings of the deployment stay in the environment and
never end up in this file.

## Behind a reverse proxy

The manager can be served from a subpath such as `/kea-roost/`, because the
interface only uses relative URLs. The URL needs the trailing slash, or the proxy
has to redirect to it.

- If the proxy strips the subpath, nothing has to be set.
- If it forwards the path unchanged, set `MANAGER_BASE_PATH` to the same subpath.

For writes to pass the cross-origin check, the proxy has to keep the original
`Host` header or send `X-Forwarded-Host`. With HTTPS at the proxy, it should also send
`X-Forwarded-Proto: https`, so that the session cookie is limited to HTTPS.

Failed logins are counted per address. Behind a proxy every request comes from the
proxy, so name it in `MANAGER_TRUSTED_PROXIES`; the address of the client is then taken
from `X-Forwarded-For`. The address Kea Roost sees for the proxy is in its log after a
failed login. With the proxy on the Docker host, it is usually the gateway of the Docker
bridge. Only name proxies there: any host in the list can claim any client address.

## MAC vendors

MAC addresses show their vendor as a tooltip, and the search covers it too.
Locally assigned addresses, as used by phones, bridges and VMs, are marked
`local`. The
vendor table is embedded in the binary, so no internet access is needed at
runtime. `go run ./cmd/oui-update` refreshes it from the IEEE data published by
Wireshark.

## Themes

The header offers two themes, the dense and square Classic and the round and airy
Modern. Each comes in light and dark, and Auto follows the system setting. The
browser remembers the choice.
