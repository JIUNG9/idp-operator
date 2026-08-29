# OSS Hygiene — The Production-Ready Rule

**Version**: 1.0
**Applies to**: this repo and every future OSS project under `github.com/JIUNG9`.
**Enforcement**: `.github/workflows/oss-hygiene.yml` runs the checks on every push and pull request.

---

## The rule

> **An OSS project is a production-ready product that any engineer can clone and safely deploy to their own environment. The author is the first real user, not the only user. The software must assume strangers will deploy it against their own real production data.**

This document is both a spec and a checklist. It is referenced by the CI workflow above and by the assistant's project memory so the rule travels with the repo, not with any single conversation.

---

## What "production-ready for anyone" means concretely

| Dimension | Wrong (personal script) | Right (OSS product) |
| --- | --- | --- |
| Detection regex | Hardcodes the author's employer hostname suffix | Matches generic `.internal / .local / .corp / .intranet / .lan`; company-specific suffixes go through `custom_patterns` config |
| Default config | Author's personal email, author's Git remote | Generic placeholders (`idp-bot@localhost`), empty/required remote |
| Docstring examples | `"Contact <your-name>@<your-employer>.com"` | `"Contact alice.dev@acme-corp.com"` |
| Test fixtures | `"db01.prod.<your-employer>.com"` | `"db01.prod.internal"` + one test proving `custom_patterns` can still catch company-specific suffixes |
| AWS account IDs | Real values | Docs-reserved placeholders: `123456789012`, `111122223333`, `987654321098`, or `000000000000` (LocalStack) |
| Safety posture | "Remember to sanitize" | A shipped feature (PII proxy, IAM template, kill switch) running by default |
| Article framing | "Here's what I built as a side project" | "Here's what the project does — and here's a case study of me using it at my employer" |

The principle in one line: **if a rule can only be upheld by a human remembering to do something, it is not a rule — it is a bug waiting for a bad day.**

---

## The CI workflow

`.github/workflows/oss-hygiene.yml` runs three greps on every push and pull request:

### 1. Employer name check

```
PATTERN: placen|naver|coupang|lotte|hyundai
# (These are the terms the check greps FOR. They must stay literal here and in
#  the workflow, or the check stops catching anything.)
SCOPE:   *.py, *.go, *.ts, *.tsx, *.js, *.jsx, *.json, *.yaml, *.yml, *.sh, *.toml
EXCLUDE: node_modules/, .git/, articles/, docs/, memory/, .claude/
```

Zero matches required. Update the pattern list when the author changes employer.

### 2. Personal email check

```
PATTERN: jiung\.gu@|@placen\.co\.kr|@naver\.com|@coupang\.com
SCOPE:   *.py, *.go, *.ts, *.tsx, *.js, *.jsx, *.toml
EXCLUDE: node_modules/, .git/, articles/, docs/, memory/
```

Zero matches required. Personal emails only belong in article bylines and git commit metadata.

### 3. AWS account ID check

```
PATTERN: any 12-digit number NOT in the allowed placeholder set
ALLOWED: 123456789012, 111122223333, 987654321098, 000000000000
SCOPE:   *.py, *.go, *.ts, *.tsx, *.json, *.yaml, *.yml, *.tf, *.tfvars
EXCLUDE: node_modules/, .git/, articles/, docs/, memory/
```

If a legitimate 12-digit id is not an AWS account, add a comment explaining so the scanner can be tuned.

---

## When the author changes jobs

1. Add the new employer name to the regex in `.github/workflows/oss-hygiene.yml` (employer-names step).
2. Add the new employer's email domain to the regex in the same file (email step).
3. Leave the previous employer in the list — past patterns must still be caught.
4. Update the "Pattern library" table in this doc to reflect any new context.

---

## Pattern library — generic replacements

When writing any OSS code, docstring, README, or test, use these placeholders rather than real names.

| Instead of | Use |
| --- | --- |
| Any real company name, present or past | `acme-corp`, `customer-xyz`, `your-org` |
| Any personal or work email | `alice.dev@acme-corp.com`, `sre-oncall@example.com` |
| Any real internal hostname | `db01.prod.internal`, `api.cluster.corp`, `eks-node.local` |
| Real AWS account id | `123456789012` (canonical) / `111122223333` / `987654321098` / `000000000000` (LocalStack) |
| Real customer name | `<CUSTOMER_1>`, `contoso`, `fabrikam` |
| Real hostname suffix you want detected | Move to `custom_patterns` config in the relevant module, document as an example |

---

## When to break the rule (narrow exceptions)

The rule is strict on purpose, but there are a few legitimate exceptions. Each is scoped to a specific directory that CI already excludes.

### `articles/`

Medium articles carry the author's byline and real-world context. The PIPA case study (Article #11) legitimately discusses how the architecture applies to Korean deployments. Company names can appear here to establish credibility, but **only in author-context paragraphs**, not in code snippets (code snippets in articles should still use `acme-corp` placeholders).

### `docs/`

Design docs sometimes reference the author's deployment environment as a concrete example (e.g. "how this is deployed on the cluster it was written for"). This is a case-study framing and is fine, but it must be clear the example is illustrative — the software itself is generic.

### `memory/`

The assistant's session memory tracks the author's work history (current employer, past employers, career goals). This is intentional and private to the author's `~/.claude/` directory. Never commit memory files to any repo.

---

## The retroactive clause

If you ever notice a past commit containing hardcoded company-specific values, open a cleanup commit the moment you notice. Don't wait for a stranger to fork the project and ask "why doesn't this work for me?" The CI workflow is the primary enforcement, but catching issues in older code before they embed deeper is the operator's responsibility.

---

## Applies to every future OSS project

This rule is not specific to idp-operator. It applies to every public repository under `github.com/JIUNG9`, present and future. When starting a new OSS project:

1. Copy `.github/workflows/oss-hygiene.yml` into the new repo
2. Copy this document (or link back to it) into `docs/`
3. Update the employer-list regex if it is stale
4. Configure the project's module names in the `--exclude-dir` list if the default excludes do not fit

The combination of the CI workflow + this doc + the session memory entry (`feedback_oss_production_ready.md`) makes the rule travel with the author, not with any single repo or conversation.

---

## History of enforcement

| Date | Event |
| --- | --- |
| 2026-04-21 | Rule codified in session memory (`feedback_oss_production_ready.md`) |
| 2026-04-22 | Audit of idp-operator found 17 files with company-specific hardcodes; cleaned up in commit `d58baba` |
| 2026-04-22 | `.github/workflows/oss-hygiene.yml` added as CI enforcement |
| 2026-04-22 | This document created |
