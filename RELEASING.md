# Release process

Native Linux package and repository signing is documented in
[`LINUX_PACKAGES.md`](LINUX_PACKAGES.md). A canonical tag release requires the
protected GPG identity and has no unsigned `.deb`/`.rpm` fallback.

Only maintainers with protected-tag and release-environment access may publish
Burnban releases. Releases are built by GitHub Actions only from a signed
stable `vX.Y.Z` or release-candidate `vX.Y.Z-rc.N` tag; binaries built on a
workstation are never uploaded as official artifacts.

## First public release

Before creating the first public tag:

- make `github.com/burnban/burnban` the canonical public repository with
  `main` as its protected default branch;
- enable private vulnerability reporting, required CI/CodeQL checks, a `v*`
  tag ruleset, and required reviewers on the `release` environment;
- allow GitHub artifact attestations and confirm the workflow's least-privilege
  token permissions are accepted by the organization; and
- stage `burnban.sh/install` and the PowerShell install URL, but do not replace
  a prelaunch placeholder until the public release assets and checksums exist.

If the workflow is run in a fork or private staging repository, its release
candidate and installer gates run, but the publish and anonymous-install jobs
are skipped. Only `burnban/burnban` may create the official release resolved by
the installers.

## Release candidate

1. Ensure CI and CodeQL pass on the intended commit.
2. Review dependency, vulnerability, secret, and third-party-license results.
3. Verify current first-party pricing sources and supported model identifiers.
4. Run the GoReleaser snapshot job and installer matrix.
5. Test the dashboard/desktop launch and Docker deployment on clean systems.
6. Run `burnban bench --requests 2000 --concurrency 4`, a larger-ledger pass,
   and the 100k-row Go benchmarks on the tagged candidate. Record commit,
   hardware, OS, date, and repeated runs; update or remove README performance
   claims when they no longer match.
7. Publish an `-rc.N` tag when installer, signing, or migration behavior changed.

## Publish

Create and push an annotated semantic-version tag, for example:

```sh
git tag -s v0.4.0 -m "burnban v0.4.0"
git verify-tag v0.4.0
git push burnban v0.4.0
```

The workflow pins the expected signer fingerprint and imports
`.github/release-signing-key.asc` into a fresh keyring before running
`git verify-tag`. A signer rotation must update both files in a reviewed,
protected-main change before any tag is created.

Wait for the canonical publish and anonymous-install verification to pass, then
mirror that exact annotated tag to the private backup remotes. Their workflows
validate the candidate but cannot create public or private releases:

```sh
git push origin v0.4.0
git push op8 v0.4.0
```

Before publication, the release workflow pins and runs GoReleaser and Syft,
builds the actual tagged archives once without publishing, verifies their
checksums and license payload, and runs those exact bytes through the Linux,
macOS, and Windows installer smoke tests. Publication is also blocked on the
non-root container runtime smoke and the pinned Playwright/axe responsive,
keyboard, and accessibility gate. Only after every candidate job passes does
the workflow attest those same archives, third-party license bundles, SHA-256
checksums, and SPDX SBOMs; upload them to a private draft release; download and
hash-check the draft assets; and make that draft public as its final step.
If a publish job is rerun, it resumes only when the existing draft or public
release has the exact same asset count, checksum manifest, and bytes. Review
and delete a mismatched private draft before retrying; never replace mismatched
assets on a public release.

After publication, the workflow performs one final check without release API
credentials: it downloads `checksums.txt` and every listed asset through the
public GitHub release URLs, verifies all hashes, and runs the Linux installer,
normal-uninstall, purge, and corrupt-archive tests against the published
archive. A failure leaves the release published for incident analysis but it
must not be announced or marked production-ready; follow the rollback guidance
below and publish a new patch version rather than replacing assets.

When Apple or Windows signing credentials are configured, platform signing and
notarization must complete before moving a release out of prerelease status.
Unsigned artifacts must never be described as signed or notarized.

## Verify before announcement

- Confirm the workflow's `anonymous-install-verify` job passed. Repeat the
  commands below from a signed-out browser or credential-free clean system when
  release visibility or CDN behavior changed.
- Download every archive anonymously and verify it against `checksums.txt`.
- Verify provenance with `gh attestation verify <artifact> --repo burnban/burnban`.
- Run `scripts/smoke_install.sh` on Linux and both Apple architectures and
  `scripts/smoke_install.ps1` on supported Windows architectures.
- On Windows 11 AMD64 and ARM64, run the public PowerShell installer from a
  standard non-administrator account with UAC installer detection enabled.
  Confirm that installation completes without an elevation request and that
  `burnban version` exits successfully. GitHub-hosted Windows runners have UAC
  disabled, so their installer smoke job does not replace this release check.
- Confirm normal uninstall preserves unrelated files and user data, while an
  explicit purge removes only the marked `.burnban` data directory.
- For a stable release, confirm `https://burnban.sh/install`, the raw
  PowerShell URL, and `releases/latest` all resolve to this release. For an RC,
  confirm its tag-specific URLs work and `releases/latest` still resolves to
  the prior stable version.
- Confirm the release notes list limitations, breaking changes, migrations,
  security fixes, and newly unpriced/deprecated models.

If a release is bad, mark it prerelease, publish a fixed patch version, and keep
the compromised or incorrect artifacts available only as needed for incident
analysis. Never replace assets under an existing tag without a public incident
note because that invalidates checksums and attestations.
