# prod-branch-report

A tool that analyzes repositories across a GitHub organization to identify which branches are likely being used as "production" branches.

## Implementations

This project ships **two behavior-faithful implementations** — pick whichever fits your environment:

| Option | Where | Run it | Requires |
|--------|-------|--------|----------|
| **Go** (original) | repository root ([`main.go`](main.go)) | `go run main.go <org-slug> report.csv` | Go 1.24+ |
| **Python** (port) | [`python/`](python/) directory | `python prod_branch_report.py <org-slug> report.csv` | Python 3.9+ |

Both collect the same signals, produce the same CSV columns (in the same order), and support the same full/`--light` modes. The rest of this README documents the Go tool; the Python version has its own setup and usage guide in [`python/README.md`](python/README.md).

## Why this is hard

Identifying the production branch in a repository is more art than science. There is no single API field that definitively says "this is the production branch." Teams use wildly different branching strategies:

- Some use `main` or `master` as their production branch
- Some deploy from `release/*` or `deploy` branches
- Some have long-lived `production` or `stable` branches
- Some deploy from tags with no clear branch association
- Some have multiple production branches (e.g., per-region or per-version)

The default branch is often — but not always — the production branch. Branch protection rules and rulesets indicate importance but don't confirm production use. Deployments are the strongest signal, but not all repos use GitHub's deployment API.

This tool doesn't try to give you a definitive answer. Instead, it produces a **report with multiple signals as columns**, letting you (the human) apply judgment across the data. Look for convergence: when multiple signals point to the same branch, confidence is high. When signals diverge, it's worth investigating.

## Signals collected (deterministic)

All signal collection is fully deterministic — no AI involved. The same inputs always produce the same outputs.

| Column | What it tells you | API Source |
|--------|-------------------|------------|
| Default Branch | The repo's configured default | GraphQL: `defaultBranchRef` |
| Protected Branches | Branches with protection rules (patterns) | GraphQL: `branchProtectionRules` |
| Ruleset Target Branches | Branches targeted by active repo-level rulesets | REST: `/repos/{o}/{r}/rulesets` + `/repos/{o}/{r}/rulesets/{id}` |
| Deployment Branches (prod) | Branches that have been deployed to a "production" environment | GraphQL: `deployments(environments: ["production"])` |
| Release Target Branches | Branches that releases were cut from | REST: `/repos/{o}/{r}/releases` → `target_commitish` |
| Tagged Branches (by count) | Branches with the most release tags | REST: releases grouped by `target_commitish` |
| Top PR Merge Target | Branch receiving the most merged PRs | REST: `/repos/{o}/{r}/pulls?base={branch}` |
| Workflow Push Branches | Branches listed in `on.push.branches` in GitHub Actions workflows | REST: contents API + YAML parsing |
| Most Active Branch (6mo) | Branch with the highest commit count in the last 6 months | GraphQL: `history(since: ...)` |
| Deepest Branch (total commits) | Branch with the most total commits (proxy for longest-lived) | GraphQL: `history(first: 0)` → `totalCount` |

## LLM Analysis (separate step)

Instead of built-in AI, this tool provides a prompt file (`ANALYSIS_PROMPT.md`) that you can use with any LLM to analyze the CSV output. The LLM will produce a separate report with:

- Whether each repo likely has 1 or multiple production branches
- Which branch(es) are the best candidates
- Confidence level and reasoning

This keeps the tool itself fully deterministic while letting you use your preferred LLM for the interpretive step.

## Usage

```bash
# Set a GitHub token (or use gh CLI auth)
export GITHUB_TOKEN=ghp_...

# Run against an org, output to CSV
go run main.go <org-slug> report.csv

# Lightweight mode — 5 key signals, faster
go run main.go <org-slug> report.csv --light

# Output to stdout
go run main.go <org-slug>
```

### Modes

| Mode | Signals | Focus |
|------|---------|-------|
| Full (default) | 10 | All available signals including deployments, workflows, commit velocity |
| `--light` | 5 | Default branch, PR targets, rulesets, branch protection, tags |

**Lightweight mode** focuses on the signals most reliably indicative of production branches: which branches receive PRs, which are protected by rules/rulesets, and which have release tags. It skips deployments, workflow parsing, commit velocity, and branch depth.

## Requirements

- Go 1.24+
- A GitHub token with `repo` scope (or a GitHub App with equivalent permissions)
- Falls back to `gh auth token` if `GITHUB_TOKEN` is not set

## Rate Limit Handling

The tool handles both primary and secondary GitHub API rate limits:

- **Primary limits**: Checks `X-RateLimit-Remaining` and `X-RateLimit-Reset` headers. When exhausted, waits until the reset window with a visible countdown.
- **Secondary limits**: Detects `Retry-After` headers on 403/429 responses and waits the specified duration.
- Retries up to 3 times per request on rate limit hits.
- Displays clear warnings with wait times when rate limits are encountered.

## Limitations

- Only checks the first 100 releases per repo (most recent)
- Compares top 5 candidate branches for expensive per-branch queries (commit velocity, PR counts, branch depth)
- Workflow YAML parsing is basic (line-based, not a full YAML parser)
- Tag-to-branch mapping uses release `target_commitish` rather than git ancestry (faster, but misses tags not associated with releases)
- Rulesets are checked at the repo level only (org-level rulesets require `admin:org` scope and are not included)
- Rate limits may require multiple runs for large orgs (500+ repos)

