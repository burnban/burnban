# Release process

Only maintainers with protected-tag and release-environment access may publish
Burnban releases. Releases are built by GitHub Actions from an annotated `v*`
tag; binaries built on a workstation are never uploaded as official artifacts.

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
assets will be published there while the official installers still resolve
`burnban/burnban`; that is not a valid production release.

## Release candidate

1. Ensure CI and CodeQL pass on the intended commit.
2. Review dependency, vulnerability, secret, and third-party-license results.
3. Verify current first-party pricing sources and supported model identifiers.
4. Run the GoReleaser snapshot job and installer matrix.
5. Test the dashboard/desktop launch and Docker deployment on clean systems.
6. Publish an `-rc.N` tag when installer, signing, or migration behavior changed.

## Publish

Create and push an annotated semantic-version tag, for example:

```sh
git tag -s v0.4.0 -m "burnban v0.4.0"
git push origin v0.4.0
```

Before publication, the release workflow pins and runs GoReleaser and Syft,
builds an unpublished snapshot from the tagged commit, verifies its checksums
and license payload, and runs that snapshot through the Linux, macOS, and
Windows installer smoke tests. Publication is also blocked on the non-root
container runtime smoke and the pinned Playwright/axe responsive, keyboard, and
accessibility gate. Only after every candidate job passes does the workflow
create the archives, third-party license bundles, SHA-256 checksums and SPDX
SBOMs, publish the GitHub release, and record signed GitHub build-provenance
attestations.

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
