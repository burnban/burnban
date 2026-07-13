# Linux packages and signed repositories

Burnban's package pipeline builds native `.deb` and `.rpm` artifacts for
amd64 and arm64. Pull requests build the packages twice and compare exact
checksums, inspect their metadata, and confirm `/usr/bin/burnban` plus linked
documentation are present. Those CI snapshots are intentionally unsigned and
short-lived; they are evidence for packaging behavior, not a release channel.

On a canonical `v*` tag, the protected `release` environment must provide:

- `BURNBAN_RELEASE_GPG_PRIVATE_KEY_B64`: the base64-encoded armored private
  signing key;
- `BURNBAN_RELEASE_GPG_FINGERPRINT`: its complete expected fingerprint; and
- `BURNBAN_RELEASE_GPG_PASSPHRASE`: the key passphrase, when one is present.

The workflow fails closed if the identity is absent or its fingerprint does
not match. nFPM signs each Debian and RPM package from the temporary key file.
The repository builder then verifies those signatures, creates
architecture-specific APT indexes and RPM metadata, signs `InRelease`,
`Release.gpg`, and `repomd.xml`, exports only the public key, and creates a
normalized-layout repository tarball. Signed metadata may embed signing time,
so separate signed runs are not promised to be byte-for-byte identical.
Temporary private-key material is held in the runner's restricted temporary
directory and is not uploaded.

The signed package bytes, repository tarball, and public key are hashed into a
second manifest. The publish job verifies that manifest, merges it with the
already-tested archive manifest, attests the combined exact bytes, uploads a
private draft, downloads it again, and publishes only after every digest
matches. A missing protected secret or signing tool blocks the release; there
is no unsigned tagged fallback.

The repository does not claim that signed packages currently exist. That
evidence is created only by a successful protected tag workflow using the
production signing identity. Key generation, offline backup, revocation,
expiry, public-key distribution, and environment approvers remain release
operations outside source control.

## Local snapshot check

```sh
goreleaser check --config .goreleaser-packages.yaml
BURNBAN_RELEASE_GPG_KEY_FILE= \
  goreleaser release --snapshot --clean --config .goreleaser-packages.yaml
(cd package-dist && sha256sum --check linux-package-checksums.txt)
```

Never point a local snapshot command at the production key. A release engineer
may test the signed path with an explicitly disposable key in an isolated
environment; test keys and generated `package-dist/` contents are ignored and
must not be committed.
