# Contributing to Burnban

Thanks for helping make local AI spend controls more useful and trustworthy.
Small, focused pull requests are easiest to review.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
Security vulnerabilities must be reported through [SECURITY.md](SECURITY.md),
not a public issue.

## Development setup

Burnban requires Go 1.25.12 or newer. Tests use local fixtures and loopback servers;
they must not consume paid model APIs or require provider credentials.

```sh
git clone https://github.com/burnban/burnban.git
cd burnban
make build
make test
```

Before opening a pull request, run:

```sh
test -z "$(gofmt -l $(git ls-files '*.go'))"
go mod tidy -diff
go mod verify
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
```

CI additionally runs Staticcheck, `govulncheck`, secret and license checks,
release snapshot/install tests, pinned Chromium/Firefox/WebKit Playwright/axe
responsive and accessibility checks, CodeQL, and a non-root container smoke
test.

## Pull requests

- Explain the user problem and why the proposed behavior is appropriate.
- Add tests for behavior changes, including failure and privacy boundaries.
- Keep provider fixtures synthetic; never commit real logs, prompts, keys, or
  account identifiers.
- Preserve localhost-only and fail-closed network defaults.
- Call out schema, CLI, metric, export, MCP, or configuration compatibility
  changes explicitly.
- Pricing changes must link to a first-party provider source and identify the
  date and region/tier to which the price applies.
- Avoid unrelated formatting or refactoring in the same pull request.

Maintainers may ask for a changelog or migration note before merging a breaking
change. Pull requests require passing CI and maintainer review.

## Licensing

Burnban is MIT licensed. By submitting a contribution, you confirm that you
have the right to contribute it and license it under the repository's MIT
license. Contributors retain copyright in their work. The project does not
require a copyright assignment.
