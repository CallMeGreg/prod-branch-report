# prod-branch-report (Python)

A Python port of the [`prod-branch-report`](../README.md) Go tool. It analyzes
repositories across a GitHub organization to identify which branches are likely
being used as "production" branches, and writes a CSV report with one signal
per column so you (the human) can apply judgment across the data.

Like the Go version, **all signal collection is fully deterministic** — no AI is
involved. The same inputs always produce the same outputs. See the top-level
[README](../README.md) for the "why this is hard" background and the
[`ANALYSIS_PROMPT.md`](../ANALYSIS_PROMPT.md) file for the optional LLM analysis
step.

## Signals collected

| Column | What it tells you | API Source |
|--------|-------------------|------------|
| Default Branch | The repo's configured default | GraphQL: `defaultBranchRef` |
| Protected Branches | Branches with protection rules (patterns) | GraphQL: `branchProtectionRules` |
| Ruleset Target Branches | Branches targeted by active repo-level rulesets | REST: `/repos/{o}/{r}/rulesets` + `/rulesets/{id}` |
| Deployment Branches (prod) | Branches deployed to a "production" environment | GraphQL: `deployments(environments: ["production"])` |
| Release Target Branches | Branches that releases were cut from | REST: `/releases` → `target_commitish` |
| Tagged Branches (by count) | Branches with the most release tags | REST: releases grouped by `target_commitish` |
| Top PR Merge Target | Branch receiving the most merged PRs | REST: `/pulls?base={branch}` |
| Workflow Push Branches | Branches in `on.push.branches` in Actions workflows | REST: contents API + YAML parsing |
| Most Active Branch (6mo) | Branch with the highest commit count in the last 6 months | GraphQL: `history(since: ...)` |
| Deepest Branch (total commits) | Branch with the most total commits (proxy for longest-lived) | GraphQL: `history(first: 0)` → `totalCount` |

## Requirements

- Python 3.9+
- A GitHub token with `repo` scope (or a GitHub App with equivalent permissions)
- Falls back to `gh auth token` if `GITHUB_TOKEN` is not set

## Setup

From the `python/` directory:

```bash
# 1. (Recommended) create and activate a virtual environment
python3 -m venv .venv
source .venv/bin/activate          # Windows: .venv\Scripts\activate

# 2. Install dependencies
pip install -r requirements.txt
```

Dependencies:

- [`requests`](https://pypi.org/project/requests/) — HTTP client for the GitHub REST and GraphQL APIs
- [`rich`](https://pypi.org/project/rich/) — terminal header, spinner, progress bar, and summary table

## Authentication

Provide a token in either of two ways:

```bash
# Option A: environment variable
export GITHUB_TOKEN=ghp_...

# Option B: rely on the GitHub CLI (used automatically if GITHUB_TOKEN is unset)
gh auth login
```

## Usage

```bash
# Run against an org, output to CSV
python prod_branch_report.py <org-slug> report.csv

# Lightweight mode — 5 key signals, faster
python prod_branch_report.py <org-slug> report.csv --light

# Output to stdout (progress/log output goes to stderr, so this stays clean)
python prod_branch_report.py <org-slug> > report.csv
```

You can also make the script executable and run it directly:

```bash
chmod +x prod_branch_report.py
./prod_branch_report.py <org-slug> report.csv
```

### Modes

| Mode | Signals | Focus |
|------|---------|-------|
| Full (default) | 10 | All available signals including deployments, workflows, commit velocity |
| `--light` | 5 | Default branch, PR targets, rulesets, branch protection, tags |

**Lightweight mode** focuses on the signals most reliably indicative of
production branches: which branches receive PRs, which are protected by
rules/rulesets, and which have release tags. It skips deployments, workflow
parsing, commit velocity, and branch depth.

## Output

The tool writes a CSV report (to the given file, or to stdout). Decorative
output — the header, discovery spinner, progress bar, rate-limit warnings, and
the coverage summary table — is written to **stderr**, so redirecting stdout to
a file captures only the CSV:

```bash
python prod_branch_report.py my-org > report.csv   # report.csv is clean CSV
```

## Rate limit handling

The tool handles both primary and secondary GitHub API rate limits:

- **Primary limits**: checks `X-RateLimit-Remaining` and `X-RateLimit-Reset`
  headers. When exhausted, waits until the reset window with a visible message.
- **Secondary limits**: detects `Retry-After` headers on 403/429 responses and
  waits the specified duration.
- Retries up to 3 times per request on rate-limit hits.
- Displays clear warnings with wait times when rate limits are encountered.

## Limitations

These mirror the Go implementation:

- Only checks the first 100 releases per repo (most recent)
- Compares the top 5 candidate branches for expensive per-branch queries
  (commit velocity, PR counts, branch depth)
- Workflow YAML parsing is basic (line-based, not a full YAML parser)
- Tag-to-branch mapping uses release `target_commitish` rather than git ancestry
  (faster, but misses tags not associated with releases)
- Rulesets are checked at the repo level only (org-level rulesets require
  `admin:org` scope and are not included)
- Rate limits may require multiple runs for large orgs (500+ repos)

## Relationship to the Go version

This script is a behavior-faithful port of `../main.go`: the same signals, the
same CSV columns (in the same order), the same full/light modes, and the same
rate-limit strategy. The only intentional difference is that log/progress output
is sent to stderr (instead of stdout) so piping the CSV works cleanly.
