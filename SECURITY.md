# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
discussions, or pull requests.**

Instead, use GitHub's private
[**Report a vulnerability**](https://github.com/yolorouter/yolorouter/security/advisories/new)
flow (Security → Advisories). This delivers the report privately to the
maintainers.

Please include as much of the following as you can:

- A description of the issue and its impact.
- Steps to reproduce, or a proof of concept.
- Affected version(s) (`./yolorouter --version`) and configuration (database driver, deployment shape).
- Any suggested mitigation, if you have one.

We will acknowledge your report, keep you updated on our progress, and credit
you in the release notes once a fix ships (unless you prefer to remain
anonymous).

## Scope

Yolorouter stores sensitive material — upstream provider keys (encrypted at
rest with AES-256), hashed admin credentials, and hashed API keys. Reports
that concern the confidentiality of these secrets, authentication and session
handling, access-control bypasses (model allowlist, budgets, key state), or
outbound request safety (SSRF) are especially valuable.

## Supported versions

Yolorouter is pre-1.0 and evolving quickly. Security fixes target the latest
release; please upgrade before reporting to confirm the issue still reproduces.
