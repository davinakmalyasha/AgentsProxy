# Security Policy

## Supported Versions

Only the current `main` branch is supported. Security fixes land on `main`
and are deployed through the gated deploy workflow (`db-migrate` →
staged deploy → smoke → promote). There are no maintained release branches.

## Reporting a Vulnerability

**Do not open a public issue for a suspected vulnerability.**

Use one of these private channels:

- GitHub **Private vulnerability reporting**: repository
  `Security` tab → `Report a vulnerability` (preferred — creates a private
  advisory with a CVE request flow).
- If that is unavailable, contact the maintainer privately through the
  contact listed in `README.md` / `RUNBOOK.md`.

Include:

- affected endpoint, commit SHA, and how to reproduce;
- what an attacker can read, write, or bypass (RLS boundary, tenant
  isolation, auth, rate limit, SSRF, injection);
- whether production data or credentials are at risk.

You will get an acknowledgement within 72 hours. Fixes ship through the
normal gate; coordinated disclosure timing can be arranged for severe
issues.

## Scope and Rules of Engagement

- **In scope:** this repository's code and its default configuration,
  tenant-isolation and auth boundaries, RLS policies, session handling,
  rate limiting, and CI supply-chain configuration.
- **Out of scope / do not do:**
  - load testing, brute force, or credential stuffing against the
    production deployment;
  - accessing, modifying, or exfiltrating real tenant or customer data;
  - social engineering, phishing, or physical attacks;
  - testing third-party services (Supabase, Vercel, Upstash, Resend) —
    report those to the respective vendor.

Rate limits and abuse guards are intentional; tripping them during testing
is expected. Stop and report if you reach real user data.

## What to Expect After a Report

1. Triage and severity assessment (CVSS-style, plus tenant-isolation impact).
2. Fix on a private branch, full test suite, then merge through the gate.
3. Production deploy with verification (`/healthz`, smoke); quarantine or
   key rotation if credentials were exposed.
4. Public advisory only after the fix is live, unless you request otherwise.

## Hardening Already in Place

- All secrets live in GitHub Actions secrets / Vercel environment variables,
  never in the repo. `.env`, `.env.staging`, and `.env.test` are gitignored
  and have never been committed (only `*.example` files are tracked).
- Fork pull requests do not receive Actions secrets; production deploys run
  only from `main` through the gated workflow.
- `gitleaks` and `govulncheck` run on every PR; dependencies are SHA-pinned
  with Dependabot coverage.
