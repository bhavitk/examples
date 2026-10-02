# Phase 2: Self-service device registration

Phase 1 was a POC: an operator hand-runs `setup-operator` and `provision-device`, and a shell/Go script mints creds and writes configs for every device. Phase 2 replaces the manual step with a self-service flow: a local device registers itself to the remote side through a UI, authenticated by Keycloak.

## Decisions carried over from Phase 1

1. Remote NATS stays as is (JWT/operator auth, `DEVICES` account, mem-resolver, leafnode port 7422).
2. The remote client that receives data from the devices stays as is.
3. Local NATS instances have friendly names (L1, L2, L3), but their **device id is an auto-generated UUID**. The name is a display label only.
   - Subjects become `device.<uuid>.outgoing.data` and `device.<uuid>.incoming.data`.
   - The current `deviceIDRe` (`^[a-z0-9-]+$`) already accepts a UUID.
   - The id is generated **once**, on the first start of the local stack, and persisted on a volume. It is not generated at registration time, so it is stable before and after registration (see "Offline buffering").
   - The friendly name is stored in the registry and is not part of the subject.

## Target flow

```
 Local site                                           Remote site
┌───────────────────────────────┐                   ┌──────────────────────────┐
│ Local UI ──► Local backend    │──(2) OIDC login──►│ Keycloak                 │
│                │  ▲           │                   │                          │
│     (8) publish│  │(5) token  │──(3) POST ───────►│ Registration API         │
│                ▼  │           │     /register     │  - validate JWT (JWKS)   │
│ Local NATS  ◄── writes leaf.conf, reload (6,7)    │  - mint NATS user creds  │
│     │  ▲                      │◄─(4) url+creds────│  - record in registry    │
│     └──┼──── leafnode ────────┼──(9)─────────────►│ nats-remote ► remote-client
└───────────────────────────────┘                   └──────────────────────────┘
```

1. Local stack boots with NATS running **unregistered** (no leafnode remote). The backend generates the device UUID if none exists.
2. The user opens the local UI and clicks Register. The user logs in through Keycloak (Authorization Code + PKCE, see below).
3. The local backend calls the remote Registration API with the access token, the device UUID and the friendly name.
4. The API validates the token, mints a NATS user for this device and returns the leafnode URL and creds.
5. The backend stores the creds on a volume, with 0600 permissions.
6. The backend renders `leaf.conf` for the local NATS (pulled in via `include`), writing it atomically.
7. The backend reloads NATS (`SIGHUP`, no restart needed) and checks that the leafnode is connected. If it is not, it rolls back and reports the error in the UI.
8. The local app backend keeps publishing to local NATS the whole time, registered or not.
9. After step 7, new messages flow through the leafnode to the remote NATS and are seen by `remote-client`.

## Gaps in the original plan and what to do about them

### A. "Data before registration" is lost, not delayed (most important)
Core NATS does not queue messages. A publish on a server with no subscribers, and with no leafnode, is silently dropped. The statement in step 9 that data "were stopped at local only" and will flow later is only true for *new* messages. Choose one explicitly:

- **Option 1 (live data only, default for this phase):** accept that data published before registration is dropped. Document it and show "not registered, data is not being delivered" in the UI.
- **Option 2 (no data loss):** enable **JetStream on local NATS** and have the backend publish into a local stream, e.g. `device.<uuid>.outgoing.data` with a file store and a size/age limit. After registration, replicate to the remote with a **stream source/mirror across the leafnode**. For that, the leaf must use a JetStream domain, the remote must have JetStream enabled for the `DEVICES` account, and the `remote-client` must consume from a stream instead of a core subscription. This is a bigger change, so decide it up front. Recommendation: do Option 1 in this phase and design the subjects so Option 2 can be added later.

### B. Identity and trust of the registration call
- The local backend, **not the browser**, should call the Registration API (BFF pattern). Then the access token never sits in the local UI's JS and does not need CORS to the remote API.
- Keycloak client: public client with the **Authorization Code + PKCE** flow. The redirect is the local backend, e.g. `http://localhost:<port>/auth/callback`. Each local site's redirect URI must be registered in Keycloak (a wildcard or a dedicated client per site). If the local box has no browser or user, use the **device authorization grant** instead. Decide which of the two applies.
- The API must validate: signature via JWKS, `iss`, `aud`, `exp`, and a role/scope such as `device:register`. Do not just check that "a token exists".
- Decide what a user *owns*: a device is bound to the Keycloak `sub` (and optionally an org/tenant claim). Without this binding anyone with a valid login can register any UUID.
- Token refresh: the access token is short-lived. The registration only needs it once; the registered device needs **no** Keycloak token afterwards, only its NATS creds.

### C. Registration API design (this is new, Phase 1 had no server)
- `POST /v1/devices/register` with `{device_id, name}`, authenticated with a Bearer token. The response is `{device_id, leaf_url, creds, expires_at, account}`.
- **Idempotent**: re-registering the same `device_id` by the same owner returns/rotates the creds and does not create a duplicate. A different owner gets 409.
- Reject unknown shapes: validate the UUID format and the name length/charset; limit per user (quota) and rate limit.
- Additional endpoints that the UI will need: `GET /v1/devices/{id}` (status), `DELETE /v1/devices/{id}` (deregister/revoke), `POST /v1/devices/{id}/rotate` (new creds).
- Returned creds are secrets: HTTPS only, no logging of the response body, `Cache-Control: no-store`.
- The API needs a **registry** (small Postgres/SQLite table: device_id, name, owner_sub, public nkey, status, created_at, last_registered_at, revoked_at). Without it there is no ownership, no listing, and no revocation.

### D. Signing keys and the account
- The API now holds the DEVICES account signing key online. Phase 1 reads the **account identity** seed from `nsc-data`. In Phase 2:
  - create a dedicated **account signing key** for the API and issue user JWTs with `IssuerAccount` set; keep the account identity key offline;
  - store the key in a secret store / mounted secret, not in the repo tree.
- The existing mem-resolver preload (`resolver.conf`) means any change to the account JWT (e.g. adding a signing key, or **revocations**) needs a restart of `nats-remote`. Phase 1 states "nats-remote is never touched", which will not hold for revocation. Plan to move to the **full NATS resolver** (`resolver: { type: full }`) so account updates are pushed through the system account without restarts. Do this before building revocation.
- Per-device permissions stay as in Phase 1: publish only to `device.<uuid>.outgoing.data`, subscribe only to `device.<uuid>.incoming.data`. Add `_INBOX.>` / response permissions only if request/reply is introduced. Also set limits on the user JWT (max payload, subs).
- Creds lifetime: set an `exp` on the user JWT (e.g. 90 days) and define how the device renews (re-register, or a `/rotate` call that requires a fresh Keycloak login).

### E. Applying the config on the local side
- Keep the base `nats.conf` static and `include` a generated `leaf.conf` that contains only the `leafnodes.remotes` block. The backend then only writes one small file.
- Write atomically (temp file + rename) and keep the previous version so it can **roll back** when the reload fails or the leafnode does not connect within N seconds.
- Reload: `nats-server --signal reload` needs a PID. In Docker that means either `docker kill -s HUP <container>` (needs the docker socket in the backend - a security cost) or a shared PID namespace, or a tiny sidecar that watches `leaf.conf` and signals NATS. Pick one. Verify that this NATS version (2.10) applies leafnode remote changes on reload; if not, restart the container instead, which makes publishers reconnect (the backend must use reconnect-with-buffer, see F).
- Verify success by querying the local monitoring endpoint (`/leafz`) for a connected remote, not just "reload returned 0". Surface the state to the UI: `unregistered`, `registering`, `connecting`, `connected`, `error: ...`.
- Local NATS user/permission config (the `authorization.users` block) keeps working. The leaf connection binds to the remote `DEVICES` account through the creds. Confirm that local-only clients' subjects (`device.<uuid>.*`) are what is exported over the leaf, since the local server has no accounts of its own.

### F. Backend resilience during the transition
- Backend publisher: use `nats.MaxReconnects(-1)`, `ReconnectBufSize` and a connected/disconnected handler so a NATS reload/restart does not crash it or lose the in-flight messages.
- Add a heartbeat or last-publish status in the local UI so users can see data is generated even when not delivered.
- The remote side: `remote-client` already auto-discovers devices from `device.*.outgoing.data`, so it works with UUIDs without code changes. The friendly name should be looked up from the registry for display, not parsed from the subject.

### G. Deregistration, revocation and lifecycle
- Phase 1 only has "tear it down". Phase 2 needs: **unregister from the UI** (remove `leaf.conf`, reload, call `DELETE`), **revoke on the remote** (add user key to account revocations, push the account JWT through the full resolver), and **re-register** (e.g. creds lost or expired) without changing the UUID or losing the device identity.
- Handle "device wiped but registry says registered": re-register with the same UUID is not possible if the UUID was lost, so the registry needs an admin "orphan" cleanup.

### H. Security and operations
- TLS: the leafnode connection should use TLS (`leafnodes { tls { ... } }` on remote, `tls` in the remote entry locally) and the API behind HTTPS. Plain `leaf://` over the internet is not acceptable beyond the POC.
- Keycloak realm/client setup is part of the deliverable: realm export (JSON) committed, a `docker-compose` service for Keycloak in dev, and documented client config.
- Secrets handling: `.creds`, the signing key, local NATS password must not be in the git tree or logs (`nsc-data/` is already gitignored; extend to the new local data volume).
- Observability: structured logs with `device_id` on both sides, `/healthz` on the API and the local backend, and a metric for registered vs. connected devices (the remote `/leafz`/`/connz`).
- Tests: unit tests for JWT minting permissions (a device cannot publish to another device's subject), an integration test through docker compose covering register -> connected -> data flows -> revoke -> disconnected.

## Proposed components

| Component | Where | Responsibility |
|---|---|---|
| `keycloak` | remote | OIDC provider, realm + public client |
| `registration-api` (Go) | remote | JWT validation, mint creds, registry, revoke. Reuses `natsauth.go` minting code from Phase 1 |
| registry DB | remote | device ownership and status |
| `local-ui` + `local-backend` | local | Login (BFF), UUID bootstrap, call the API, write `leaf.conf`, reload, status; also the existing data publisher |
| `nats-local` | local | NATS with a static base config + included `leaf.conf` |
| `nats-remote`, `remote-client` | remote | unchanged (except full resolver, see D) |

`setup-operator` stays as the one-time bootstrap. On the `phase2` branch the Phase 1 shell scripts, `provisioning/`, the dashboard and the `provision-device` tool were removed (still in git history on `main`); the minting code in `provision-device/natsauth.go` moves into `registration-api`.

## Open decisions (need an answer before building)

1. Data before registration: dropped (Option 1) or buffered with JetStream (Option 2)?
Ans: Option 1
2. Browser-based login on the local box (PKCE) or device authorization grant?
Ans: Browser based login
3. Ownership model: per-user, or per organisation/tenant claim from Keycloak?
Ans: This is also POC with phase 2, so for now we are keeping keycloak as login provider only. We can improve this later.
4. Reload mechanism in Docker: docker socket, shared PID namespace, or sidecar watcher?
Ans: nats reload via system subject? backend connect with sys account/user and reload using system subject!

Result (tested on nats 2.10.29): a system-subject reload **cannot add or change a leafnode remote** ("config reload not supported for LeafNode") and there is no system-subject shutdown/restart. Decision: restart `nats-local` instead. `nats-local` runs under a small supervisor script that restarts `nats-server` when the backend writes a marker file, so no sys user/account and no docker socket are needed. The backend publisher reconnects forever.
5. Do we move to the full resolver now (needed for revocation without restarting `nats-remote`)?
Ans: Yes
6. Creds lifetime and renewal policy.
Ans: No expiration

## Suggested build order

1. Switch device id to UUID everywhere (provision script, subjects, dashboard) and keep the friendly name as metadata.
2. Registration API with a registry, a stubbed auth (static token) and the minting code. Integration test with a local NATS.
3. Local NATS base config + `include leaf.conf` + backend that applies it (reload, verify via `/leafz`, rollback).
4. Keycloak in compose + real JWT validation in the API + login flow in the local backend/UI.
5. Full resolver + deregister/revoke + creds rotation.
6. TLS, observability, docs (update the README, which still describes the Phase 1 flow).

## Implementation status (branch `phase2`)

Build steps 1-5 are implemented (UUID ids, registration API + registry, local backend with restart/rollback, Keycloak login, full resolver + unregister/revoke/rotate). Still open: TLS, observability beyond logs, automated integration tests. See `README.md` for how to run it.
