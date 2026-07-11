# Security policy

Burnban sits in the request path for provider credentials and spend controls.
Please report security problems privately so users have time to update before
details are public.

## Supported versions

Until Burnban reaches 1.0, security fixes are made for the latest tagged
release. When practical, the maintainers may also patch the immediately prior
minor release. Development snapshots are not supported releases.

## Reporting a vulnerability

Use GitHub's **Report a vulnerability** button in the repository Security tab.
That opens a private security advisory visible only to repository maintainers.
If private advisories are unavailable, email `oday@syft8.com` with
`[BURNBAN SECURITY]` in the subject.

Do not include live API keys, gateway tokens, raw prompts, request bodies,
webhook URLs, databases, or local agent logs. Revoke any credential that may
already have been exposed. Synthetic reproductions are preferred.

Please include:

- affected version, operating system, architecture, and install method;
- whether Burnban was localhost-only, behind a reverse proxy, or directly
  bound to a network interface;
- a minimal reproduction and the security impact;
- whether the issue is already public or actively exploited; and
- a safe way to contact you for follow-up.

We aim to acknowledge a report within three business days and provide an
initial assessment within seven. Complex fixes may take longer; the reporter
will receive progress updates during coordinated disclosure.

## Scope

In scope are the Burnban binary, official release archives, installers,
container definition, dashboard, authentication boundaries, credential/header
handling, ledger privacy, and budget enforcement bypasses.

Third-party model providers, user-configured upstreams and webhooks, the host
operating system, and unsupported deployment configurations are normally out
of scope unless Burnban creates or materially worsens the vulnerability.

The `--allow-insecure-http` option is intentionally unsafe on an exposed
network. Reports that only demonstrate plaintext interception after explicitly
enabling that option are not vulnerabilities, though documentation improvements
are welcome.

The tokenless loopback default is not an operating-system user boundary; use a
token on shared hosts. Team mode has one shared gateway secret, and client-sent
agent/session labels are cooperative attribution rather than authenticated
identity. Reports premised only on a shared-token client relabeling itself are
therefore expected behavior; bypassing the shared token or an enforced global
cap remains in scope.

## Disclosure and credit

Please allow the maintainers to prepare a fix and release before publication.
We will coordinate timing, request a CVE when appropriate, and credit reporters
who want attribution. Good-faith research that follows this policy will not be
met with legal action by the project.
