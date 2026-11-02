# Accounts

[Back to the overview](../README.md)

The interface needs an account. Accounts live in `users.json` in the data volume, with
their passwords hashed by Argon2id.

![Users on the Settings page with an admin and a viewer](screenshots/users.png)

## The first admin

As long as no account exists, the interface asks for the first admin. So that nobody
else on the network can claim a fresh instance, this needs a setup code from the log:

    docker compose logs kea-roost | grep "setup code"

The code changes with every start and is useless once the first account exists.

![Setup of the first admin](screenshots/setup.png)

## Roles

| Role | May |
|---|---|
| `admin` | Everything: change Kea, backups, audit log, settings, users |
| `viewer` | Look at every page, change nothing; the changes in the client popup come from the audit log and stay hidden |

Admins manage accounts under Users on the Settings page. The last admin cannot be
removed or demoted, and nobody can remove their own account. Everybody, admins
included, changes their own password and 2FA in the account menu, which asks for the
current password.

## Sessions

![Login form](screenshots/login.png)

- A login starts a session cookie that scripts cannot read and other sites do not get.
- It ends after 12 hours without a request, and after 7 days in any case.
- A new password ends the other sessions of the account; a changed role applies at once.
- An account keeps at most 20 sessions; another login ends the oldest.
- Sessions live in memory: a restart of Kea Roost logs everybody out.

Logins and account changes go to the audit log, failed logins to the log of Kea Roost.

## Two-factor authentication

Every user can turn on 2FA with TOTP (RFC 6238) by the switch in the account menu. Kea
Roost shows a secret as QR code and text for any TOTP app. Turning it on or off asks
for a TOTP code and the current password; a setup not confirmed within 10 minutes is
dropped. A login then asks for a code after the password, and each code works once.

The TOTP secret is stored in `users.json` as it is, because the server needs it: treat
the data volume like the passwords of all accounts. The last used code is kept in
memory only, so after a restart it works once more within its 30 seconds. If a device
is lost, an admin resets 2FA of that account under Users.

## Failed logins

After 10 failed logins within 15 minutes, further logins of that address and of that
user name wait until the window has passed. Wrong TOTP codes, setup codes and current
passwords count the same way. Both values are on the [Settings](configuration.md#settings)
page; 0 turns the limit off.

Anybody who can reach the login form can keep an account waiting by guessing wrong on
purpose. Where that matters, turn the limit off and let a proxy limit logins instead.
Behind a proxy, name it in `MANAGER_TRUSTED_PROXIES`, see
[Behind a reverse proxy](configuration.md#behind-a-reverse-proxy).

## Lost passwords

There is no reset by mail. An admin sets a new password under Users. If nobody can log
in any more, delete `users.json` and create a new first admin:

    docker compose down
    docker run --rm -v kea-roost_data:/data alpine rm /data/users.json
    docker compose up -d

## Coming from Basic Auth

`MANAGER_USER` and `MANAGER_PASSWORD` are no longer read; Kea Roost warns about them at
the start. Create the first admin as above. Scripts log in through `POST api/login` and
keep the session cookie. `/metrics` has a login of its own, see
[Prometheus metrics](metrics.md).
