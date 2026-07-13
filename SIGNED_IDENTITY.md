# Device-bound signed identity

Burnban can authenticate request attribution supplied by an enrolled Burnban
Personal or Teams sidecar without replacing the provider credential. The
provider API key remains in the provider's normal `Authorization` or API-key
header. A separate `X-Burnban-Identity` proof is consumed by the local Burnban
proxy and is never forwarded upstream.

This feature is optional. A meter with no Personal or Teams enrollment keeps
working with its existing cooperative attribution behavior.

## What is authenticated

Enrollment creates an Ed25519 signing key on the device. Only its public key is
registered with the Personal or Teams control plane. Each successful sync
delivers a short-lived trust grant that binds the public key to server-authorized
attribution:

- Personal binds a principal to the enrolled account email;
- Teams binds a service account to the meter ID and the cost center to the
  meter's current team; and
- an exact project entry in the grant authenticates that project. The current
  built-in Personal and Teams grants use `"*"`, which permits a device-signed
  project assertion for attribution but deliberately leaves it
  `self_reported` for policy enforcement.

A claim is valid for at most two minutes and is bound to the exact POST method,
canonical Burnban provider route, raw query SHA-256, request-body SHA-256, and
the fixed `burnban-proxy/v1` audience. It also carries a random one-time nonce.
The proxy verifies the canonical payload and Ed25519 signature, checks every
binding and authorized attribution field, and atomically consumes the nonce in
SQLite. Reuse, expiry, route/query/body changes, unknown fields, alternate JSON
encodings, or a revoked/untrusted key fail before the request reaches the
provider.

Request rows record the signed envelope's tenant/device, principal or service
account, project, cost center, and overall confidence. Policy-decision context
also records separate team, user, and project confidence. This preserves an
authenticated principal while making a wildcard-granted project explicitly
`self_reported`. The proof header itself and the private key are not recorded.

## Issuing a request proof

The issuing command is deliberately low-level: the calling client must hash
and send the exact same bytes. For Personal Sync:

```sh
body='{"model":"gpt-5","input":"hello"}'
body_sha="$(printf %s "$body" | openssl dgst -sha256 -r | awk '{print $1}')"
claim="$(burnban-sync --issue-identity-claim \
  --identity-route /openai/v1/responses \
  --identity-body-sha256 "$body_sha" \
  --identity-project example)"

curl http://127.0.0.1:4141/openai/v1/responses \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Burnban-Identity: $claim" \
  --data-binary "$body"
```

For a Teams enrollment, use the same request fields with
`burnban-teams connector --issue-identity-claim`. A nonempty query must also
provide `--identity-query-sha256` for the exact raw query string, preserving
ordering and percent-encoding. Omitting that flag binds the proof to an empty
query. A generated proof is one-use; issue a new one for every request and do
not put it in a URL or log.

The device must have completed enrollment and a recent successful sync. The
key can be rotated explicitly:

```sh
burnban-sync --rotate-identity-key
burnban-teams connector --rotate-identity-key
```

Rotation is explicit and crash-safe. A pending key is saved before the network
request, survives an ambiguous response, and can be retried idempotently. Once
the new key is active, the control plane revokes the old public key. Device or
meter revocation also revokes its active signing key.

## Trusted, self-reported, and unverified attribution

When a valid signed proof is present, `X-Burnban-Team`, `X-Burnban-User`,
`X-Burnban-Project`, `X-Burnban-Service-Account`, and
`X-Burnban-Cost-Center` cannot override it; the request is rejected. Unsigned
service-account and cost-center headers are rejected outright. The older
unsigned team, user, and project headers remain available for cooperative
reporting and are stored as `self_reported`; a request with none is
`unverified`.

An enforcing policy scoped by team, user, or project requires exact server
authorization for each scoped dimension. A signature alone does not upgrade a
wildcard project assertion. Omitting a project, asserting a different project,
or spoofing an unsigned header cannot make a project-scoped rule disappear:
the proxy ignores that untrusted dimension for applicability and returns 401
`authenticated_identity_required`. Trusted dimensions still match normally,
so an untrusted project cannot broaden a rule across a different authenticated
team. Agent/session labels remain cooperative and cannot be used as an
enforcing identity scope.

The built-in control planes do not yet expose a project-assignment API and
therefore issue wildcard project grants. Project-scoped enforcement is
intentionally fail-closed with those grants. Supporting project authorization
requires the control plane to persist an administrator-owned exact project
allow-list and emit those values in `attribution.projects`.

Run `burnban doctor` to see identity state as `not_configured`, `trusted`, or
`untrusted`. A stale, corrupt, source-mismatched, or rolled-back enrolled grant
is `untrusted` and fails closed for signed requests.

## Online, offline, and revocation behavior

The server-issued trust grant is cached in the local ledger with its monotonic
revision and sync-source binding. The sync client rejects revision rollback or
a same-revision key/attribution change. A grant normally expires 15 minutes
after the successful sync that issued it; a request proof expires after two
minutes. The control plane can therefore be briefly unavailable while a device
continues issuing proofs from an unexpired cached grant.

After the grant expires, signed requests fail until a successful sync refreshes
trust. Revocation is immediate after the meter learns it online, but a device
that is already offline can retain authenticated attribution only until its
cached grant expires. Existing non-identity global controls retain their normal
offline behavior. An identity-scoped enforcing policy will deny unsigned
traffic during the outage rather than silently downgrade it.

## Security boundary

“Device-bound” means the software key is bound to the enrolled server, device,
and local ledger and is persisted atomically in a mode-`0600` regular file
(with the containing directory protected). It is not hardware attestation and
does not prove which process or interactive OS user made a request. Another
process running as the same account, or an administrator/root user, can read or
use the key and can sign as that enrolled device. A compromised caller can also
ask the sidecar to sign malicious request bytes. Use OS account isolation,
private state directories, full-disk protection, and ordinary host hardening.

The claim is signed, not encrypted: its attribution metadata can be decoded by
anyone who sees the header. Use TLS to a network-exposed Burnban gateway and the
normal `BURNBAN_TOKEN` gateway authentication as well. The identity proof is
not a provider credential, cannot authenticate directly to a model provider,
and does not create a Burnban virtual-key or provider-key vault.

## Protocol and local contract

The version-1 compact form is
`bbic1.<base64url-canonical-json>.<base64url-ed25519-signature>`, signed with
the domain separator `burnban.identity.v1` followed by a NUL byte. Unknown
versions fail closed. Trust is delivered through the existing authenticated
sync response and stored under the reserved settings keys documented in
[EXTERNAL_POLICY.md](EXTERNAL_POLICY.md). The control plane stores public keys,
status, revision, and audit events only; private signing material never leaves
the device.
