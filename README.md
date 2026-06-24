# prod-branch-report

A Go tool that analyzes repositories across a GitHub organization to identify which branches are likely being used as "production" branches.

## Why this is hard

Identifying the production branch in a repository is more art than science. There is no single API field that definitively says "this is the production branch." Teams use wildly different branching strategies:

- Some use `main` or `master` as their production branch
- Some deploy from `release/*` or `deploy` branches
- Some have long-lived `production` or `stable` branches
- Some deploy from tags with no clear branch association
- Some have multiple production branches (e.g., per-region or per-version)

The default branch is often — but not always — the production branch. Branch protection rules and rulesets indicate importance but don't confirm production use. Deployments are the strongest signal, but not all repos use GitHub's deployment API.

This tool doesn't try to give you a definitive answer. Instead, it produces a **report with multiple signals as columns**, letting you (the human) apply judgment across the data. Look for convergence: when multiple signals point to the same branch, confidence is high. When signals diverge, it's worth investigating.

## Signals collected

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

## Usage

```bash
# Set a GitHub token (or use gh CLI auth)
export GITHUB_TOKEN=ghp_...

# Run against an org, output to CSV
go run main.go <org-slug> report.csv

# Output to stdout
go run main.go <org-slug>
```

## Requirements

- Go 1.21+
- A GitHub token with `repo` scope (or a GitHub App with equivalent permissions)
- Falls back to `gh auth token` if `GITHUB_TOKEN` is not set

## Limitations

- Only checks the first 100 releases per repo (most recent)
- Compares top 5 candidate branches for expensive per-branch queries (commit velocity, PR counts, branch depth)
- Workflow YAML parsing is basic (line-based, not a full YAML parser)
- Tag-to-branch mapping uses release `target_commitish` rather than git ancestry (faster, but misses tags not associated with releases)
- Rulesets are checked at the repo level only (org-level rulesets require `admin:org` scope and are not included)
- Rate limits may require multiple runs for large orgs (500+ repos)
