# Accounts

[Back to the overview](../README.md)

The interface needs an account. Accounts live in `users.json` in the data volume, with
their passwords hashed by Argon2id.

![Users on the Settings page with an admin and a viewer](screenshots/users.png)

## The first admin

As long as no account exists, the interface asks for the first admin. To keep somebody
else on the network from claiming a fresh instance, it needs a setup code that Kea Roost
prints to its log at the start:

    docker compose logs kea-roost | grep "setup code"

The code changes with every start and is useless once the first account exists.

![Setup of the first admin](screenshots/setup.png)

## Roles

| Role | May |
|---|---|
| `admin` | Everything: change Kea, see backups and the audit log, settings, users |
| `viewer` | Look at every page, change nothing |

Admins add, edit and remove accounts under Users on the Settings page. The last admin cannot be
removed or demoted, and nobody can remove their own account. A changed role applies at
once. A new password, set by the user or an admin, ends the other sessions of the account.
Admins change their own password and 2FA in the account menu like everybody
else, since that asks for the current password. An admin session can still create new
accounts, so log out on a shared computer.

An account keeps at most 20 sessions; another login ends the oldest.

## Logging in

![Login form](screenshots/login.png)

A login starts a session in a cookie that the browser cannot read from scripts and does
not send to other sites. It ends after 12 hours without a request and after 7 days in any
case. Sessions live in memory, so a restart of Kea Roost logs everybody out. Behind a
proxy with HTTPS, send `X-Forwarded-Proto: https` so that the cookie is limited to HTTPS.

Every successful login and every change of an account is written to the audit log.
Failed logins go to the log of Kea Roost with the user name and the address.

A new login in a browser ends the session it had before.

## Two-factor authentication

Every user can turn on 2FA with TOTP (RFC 6238) by the switch in the account menu. Kea
Roost generates a secret, shown as a QR code and as text, for any TOTP app. Turning it on
is confirmed with a TOTP code and the current password. From then on a login asks for a
TOTP code after the password. A code works only once.

Turning it off also asks for a TOTP code and the current password, so that a
browser left logged in is not enough to change it. A setup that is not confirmed within
10 minutes is dropped.

The TOTP secret is stored in `users.json` as it is, since the server needs it to check codes:
treat the data volume like the password of every account. Which code was used last is
kept in memory, so after a restart the code of the last login works once more within
its 30 seconds.

If a device is lost, an admin resets 2FA of that account under Users on the Settings
page.

## Failed logins

After 10 failed logins within 15 minutes, further logins of that address and of that user
name wait until the window has passed. Wrong TOTP codes, wrong setup
codes and wrong passwords when changing the password or 2FA count the
same way. A successful login clears the count of the user name. Both values are set on
the [Settings](configuration.md#settings) page; 0 failed logins turns this off.

The limit per user name also means that anybody who can reach the login form can keep
an account waiting by guessing wrong on purpose. Where that matters, turn the limit off
here and let a proxy in front limit the logins instead.

Behind a reverse proxy all requests come from the address of the proxy, so the limit
per address would block everybody at once. Name the proxy in `MANAGER_TRUSTED_PROXIES`,
see [Behind a reverse proxy](configuration.md#behind-a-reverse-proxy).

## Lost passwords

There is no reset by mail. An admin sets a new password under Users on the Settings page. If nobody can
log in any more, stop Kea Roost, delete `users.json` from the data volume and start it
again: it asks for a new first admin, with a new setup code in the log.

    docker compose down
    docker run --rm -v kea-roost_data:/data alpine rm /data/users.json
    docker compose up -d

## Coming from Basic Auth

Earlier versions used `MANAGER_USER` and `MANAGER_PASSWORD` with Basic Auth. They are no
longer read; Kea Roost warns about them at the start, and they can be removed from `.env`.
After the update, create the first admin with the setup code as described above. Scripts
that called the API with Basic Auth now log in through `POST api/login` and keep the
session cookie.

The `/metrics` endpoint has a login of its own, see [Prometheus metrics](metrics.md).
