# multi-nats

One remote `nats-remote` plus any number of **local sites** (L1, L2, ...), each with
its own local NATS. A local site starts **unregistered**; a user logs in
(Keycloak) in the site's UI and registers it, after which its local NATS joins
`nats-remote` as a leafnode and the device's data flows to the remote.
See `Plan.md` (phase 1 brief) and `Plan-phase2.md` (this flow's design).

```
 local site (L1)                                   remote side
 ┌──────────────────────────────┐                ┌─────────────────────────────┐
 │ local-backend (UI :8080) ────┼─ OIDC login ──►│ keycloak :9090              │
 │   │ publishes device data    │─ POST register►│ registration-api :8090      │
 │   ▼                          │◄─ leaf url+creds  (SQLite registry, mints     │
 │ nats-local ◄─ leaf.conf ─────┤                │   scoped NATS user JWTs)    │
 │   └──────── leafnode ────────┼───────────────►│ nats-remote ► remote-client │
 └──────────────────────────────┘                └─────────────────────────────┘
```

## Prerequisites

- **Go 1.25+** (only to run `setup-operator`) and **Docker + Compose v2**.

## 1. Bootstrap the trust chain (once)

Creates the operator, `SYS` and `DEVICES` accounts, `nats-config/resolver.conf`
(full resolver config) and the `remote`/`sys` creds.

```sh
cd scripts/setup-operator && go run . -project-dir ../.. && cd ../..
```

Re-running creates a brand new trust chain and invalidates every registered
device (and the registry's view of them) - treat it as one-time.

## 2. Start the remote side

```sh
docker compose up -d --build
```

- `nats-remote` (4222 client, 7422 leafnodes, 8222 monitoring)
- `remote-client` - subscribes to `device.*.outgoing.data`, pings each device back
- `keycloak` on http://localhost:9090 (realm `devices`, user `testuser` / `testpass`,
  admin `admin` / `admin`). Dev only.
- `registration-api` on :8090

## 3. Start a local site

```sh
scripts/local-up.sh L1 8080
scripts/local-up.sh L2 8081      # more sites: use a free port from 8080-8083
```

(`8080`-`8083` are the redirect URIs allowed by `keycloak/devices-realm.json`.)

Open the UI (http://localhost:8080): it shows `unregistered` and the data it is
publishing locally. **Log in with Keycloak**, then **Register device**. The
backend gets creds from the API, writes `leaf.conf`, restarts `nats-local`, and
waits for the leafnode to connect (rolling back if it doesn't within 30s). The
state becomes `connected`; `docker logs -f remote-client` shows the data.

- The device id is a UUID generated once on first start (`local-data/<name>/device.json`);
  the name (L1, L2...) is only a label.
- Data published while unregistered (or while `nats-local` restarts) is not
  delivered - core NATS does not queue it.
- **Unregister** revokes the device's NATS user on the remote and disconnects it;
  **Re-register** rotates its credentials (the old ones are revoked).

## How the pieces work

- **Auth**: Authorization Code + PKCE, done by `local-backend` (the browser never
  holds the token). `registration-api` validates the Keycloak access token
  (signature via JWKS, issuer, expiry, `azp` = `local-device`). Keycloak is only a
  login provider for now; the owner of a device is the token's `sub`.
- **Creds**: each device gets a NATS user that can only publish
  `device.<uuid>.outgoing.data` and subscribe `device.<uuid>.incoming.data`, no expiry,
  signed by the `DEVICES` account signing key (`IssuerAccount` = the account).
- **Revocation**: the API adds the user key to the `DEVICES` account JWT
  revocations (signed by the operator signing key) and pushes it to
  `nats-remote`'s **full resolver** via `$SYS.REQ.CLAIMS.UPDATE`, so no
  `nats-remote` restart is needed. JWTs live in the `nats-remote-jwt` volume.
- **Why a restart, not a reload**: NATS 2.10 can't add/change a leafnode remote
  on config reload and has no system-subject shutdown. `nats-local` runs under a
  tiny supervisor (`nats/run.sh`) that restarts `nats-server` when
  `local-backend` drops a marker file - no docker socket needed.
  The backend publisher uses reconnect-forever and rides out the restart.

## Layout

| Path | What |
|---|---|
| `docker-compose.yml` | remote side: `nats-remote`, `remote-client`, `keycloak`, `registration-api` |
| `docker-compose.local.yml` | one local site: `local-init`, `nats-local`, `local-backend` (`scripts/local-up.sh`) |
| `registration-api/` | Go API: OIDC validation, SQLite registry, mint/revoke NATS creds |
| `local-backend/` | Go backend + embedded UI: login, publisher, applies `leaf.conf`, restarts NATS |
| `app/` | Go NATS client, used as `remote-client` |
| `keycloak/devices-realm.json` | dev realm import (client `local-device`, user `testuser`) |
| `scripts/setup-operator/` | one-time trust chain bootstrap |
| `nsc-data/`, `local-data/` | generated keys/creds/state - gitignored, never commit |

## Tearing down

```sh
docker compose -p local-l1 -f docker-compose.local.yml down   # a local site (add LOCAL_NAME=L1 if needed)
rm -rf local-data/l1
docker compose down -v                                        # remote side + volumes
rm -rf nsc-data nats-config/resolver.conf
```

## Known limits (POC)

- `registration-api` holds the operator signing key (to re-sign the account JWT on
  revocation) and the `DEVICES` account signing key, mounted from `nsc-data`.
  A real deployment would separate those and use a secret store.
- Plain (non-TLS) leafnode/API/Keycloak; dev passwords everywhere.
- If `nats-remote` is unreachable, rotating creds fails with 502 (nothing changes);
  if the leafnode fails to connect after a successful rotation, the local side
  rolls back to the previous (already revoked) creds - re-register to fix.
