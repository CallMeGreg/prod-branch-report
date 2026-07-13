#!/usr/bin/env python3
"""prod-branch-report — Python edition.

Analyzes repositories across a GitHub organization to identify which branches
are likely being used as "production" branches.

This is a faithful port of the Go implementation (``main.go``). All signal
collection is fully deterministic — no AI is involved. The tool produces a CSV
report with multiple signals as columns, letting a human apply judgment across
the data (look for convergence across signals).

Decorative/log output (header, progress, warnings, summary) is written to
stderr; the CSV report is written to the requested file or to stdout, so
``python prod_branch_report.py <org> > report.csv`` produces a clean CSV.
"""

from __future__ import annotations

import argparse
import base64
import csv
import os
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Callable, Optional, TypeVar

import requests
from rich.console import Console
from rich.panel import Panel
from rich.progress import (
    BarColumn,
    MofNCompleteColumn,
    Progress,
    TextColumn,
    TimeElapsedColumn,
)
from rich.table import Table

# ---------- Configuration ----------

GRAPHQL_URL = "https://api.github.com/graphql"
REST_BASE_URL = "https://api.github.com"
PER_PAGE_REST = 100
COMMIT_LOOKBACK_MONTHS = 6
HTTP_TIMEOUT = 30  # seconds
MAX_RETRIES = 3

# Decorative output goes to stderr so the CSV (stdout) stays clean when piped.
console = Console(stderr=True)


# ---------- Types ----------


@dataclass
class RepoResult:
    name: str = ""
    default_branch: str = ""
    protected_branches: list[str] = field(default_factory=list)  # protection rule patterns
    ruleset_target_branches: list[str] = field(default_factory=list)  # branches targeted by rulesets
    deployment_branches: list[str] = field(default_factory=list)  # branches deployed to production env
    release_target_branches: list[str] = field(default_factory=list)  # branches from release target_commitish
    tagged_branches: list[str] = field(default_factory=list)  # branches with the most tags (via releases)
    top_pr_merge_target: str = ""  # branch receiving the most merged PRs
    workflow_push_branches: list[str] = field(default_factory=list)  # branches in on.push.branches triggers
    most_active_commit_branch: str = ""  # branch with highest commit count in lookback
    oldest_branch: str = ""  # branch with the most total commits (deepest history)


# ---------- Console helpers (mirror the pterm prefixes used by the Go tool) ----------


def info(msg: str) -> None:
    console.print(f"[cyan] INFO [/cyan] {msg}")


def success(msg: str) -> None:
    console.print(f"[green] SUCCESS [/green] {msg}")


def warning(msg: str) -> None:
    console.print(f"[yellow] WARNING [/yellow] {msg}")


def error(msg: str) -> None:
    console.print(f"[red] ERROR [/red] {msg}")


def fatal(msg: str) -> None:
    error(msg)
    sys.exit(1)


# ---------- Authentication ----------


def get_token() -> str:
    """Return a GitHub token from ``GITHUB_TOKEN`` or ``gh auth token``."""
    token = os.environ.get("GITHUB_TOKEN", "")
    if token:
        return token
    try:
        out = subprocess.check_output(["gh", "auth", "token"], stderr=subprocess.DEVNULL)
        return out.decode().strip()
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        fatal("No GITHUB_TOKEN set and `gh auth token` failed. Provide a token.")
        raise  # unreachable; fatal() exits


# ---------- HTTP client + rate-limit handling ----------

_session = requests.Session()
_auth_token = ""
_rate_limit_lock = threading.Lock()


class GraphQLError(Exception):
    """Raised when a GraphQL response contains an ``errors`` array."""


def _handle_rate_limit(resp: Optional[requests.Response], source: str) -> bool:
    """Inspect response headers for rate limiting and wait if needed.

    Returns True if the request should be retried.
    """
    if resp is None:
        return False

    # Secondary rate limit: 403 or 429 with Retry-After header.
    if resp.status_code in (403, 429):
        retry_after = resp.headers.get("Retry-After", "")
        if retry_after:
            try:
                seconds = int(retry_after)
            except ValueError:
                seconds = 60
            _wait_for_rate_limit(seconds, "secondary", source)
            return True
        # Check if it's a rate-limit message in the body.
        body = resp.text
        if "rate limit" in body or "abuse" in body:
            _wait_for_rate_limit(60, "secondary", source)
            return True

    # Primary rate limit: X-RateLimit-Remaining is 0.
    if resp.headers.get("X-RateLimit-Remaining") == "0":
        reset_str = resp.headers.get("X-RateLimit-Reset", "")
        if reset_str:
            try:
                reset_unix = int(reset_str)
            except ValueError:
                return False
            wait_seconds = reset_unix - time.time() + 1
            if wait_seconds > 0:
                resource = resp.headers.get("X-RateLimit-Resource") or "core"
                _wait_for_rate_limit(wait_seconds, f"primary ({resource})", source)
                return True

    return False


def _wait_for_rate_limit(duration_seconds: float, limit_type: str, source: str) -> None:
    """Block until a rate-limit window resets, emitting a visible countdown."""
    with _rate_limit_lock:
        duration_seconds = max(0, int(round(duration_seconds)))
        resume_at = time.time() + duration_seconds
        resume_str = time.strftime("%H:%M:%S", time.localtime(resume_at))
        warning(
            f"\u23f3 {limit_type} rate limit hit ({source}). "
            f"Waiting {duration_seconds}s (resuming at {resume_str})"
        )
        # Sleep in small chunks so long waits stay interruptible and can print
        # periodic reassurance without conflicting with an active progress bar.
        end = resume_at
        next_update = time.time() + 30
        while True:
            remaining = end - time.time()
            if remaining <= 0:
                break
            time.sleep(min(5, remaining))
            if time.time() >= next_update and (end - time.time()) > 0:
                console.print(
                    f"[yellow]  … rate limited — resuming in {int(end - time.time())}s[/yellow]"
                )
                next_update = time.time() + 30
    success("Rate limit wait complete. Resuming...")


def do_graphql(query: str, variables: Optional[dict[str, Any]] = None) -> Any:
    """Execute a GraphQL query, retrying on rate limits. Returns the ``data`` object."""
    for _ in range(MAX_RETRIES):
        payload: dict[str, Any] = {"query": query}
        if variables:
            payload["variables"] = variables
        resp = _session.post(
            GRAPHQL_URL,
            json=payload,
            headers={
                "Authorization": f"Bearer {_auth_token}",
                "Content-Type": "application/json",
            },
            timeout=HTTP_TIMEOUT,
        )
        if _handle_rate_limit(resp, "GraphQL"):
            continue
        try:
            gql = resp.json()
        except ValueError as exc:
            raise RuntimeError(f"graphql unmarshal: {exc}\nraw: {resp.text}") from exc
        errors = gql.get("errors")
        if errors:
            raise GraphQLError(errors[0].get("message", "unknown graphql error"))
        return gql.get("data")
    raise RuntimeError("graphql: max retries exceeded due to rate limiting")


def do_rest(method: str, path: str) -> tuple[bytes, requests.structures.CaseInsensitiveDict]:
    """Execute a REST call, retrying on rate limits. Returns ``(body, headers)``."""
    for _ in range(MAX_RETRIES):
        url = REST_BASE_URL + path
        resp = _session.request(
            method,
            url,
            headers={
                "Authorization": f"Bearer {_auth_token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
            timeout=HTTP_TIMEOUT,
        )
        if _handle_rate_limit(resp, f"REST {method} {path}"):
            continue
        if resp.status_code >= 400:
            raise RuntimeError(f"REST {method} {path}: {resp.status_code} {resp.text}")
        return resp.content, resp.headers
    raise RuntimeError(f"REST {method} {path}: max retries exceeded due to rate limiting")


def _rest_json(method: str, path: str) -> Any:
    body, _ = do_rest(method, path)
    if not body:
        return None
    return _json_loads(body)


def _json_loads(body: bytes) -> Any:
    import json

    return json.loads(body)


# ---------- Phase 1: Enumerate repos ----------


@dataclass
class RepoInfo:
    name: str
    name_with_owner: str
    default_branch_name: str


def list_org_repos(org: str) -> list[RepoInfo]:
    query = """query($org: String!, $cursor: String) {
        organization(login: $org) {
            repositories(first: 100, after: $cursor, isArchived: false) {
                pageInfo { hasNextPage endCursor }
                nodes {
                    name
                    nameWithOwner
                    defaultBranchRef { name }
                }
            }
        }
    }"""

    repos: list[RepoInfo] = []
    cursor: Optional[str] = None

    while True:
        variables: dict[str, Any] = {"org": org}
        if cursor is not None:
            variables["cursor"] = cursor
        data = do_graphql(query, variables)

        org_data = (data or {}).get("organization")
        if not org_data:
            raise RuntimeError(f"listOrgRepos: organization '{org}' not found or inaccessible")
        repositories = org_data.get("repositories", {})
        for node in repositories.get("nodes", []) or []:
            default_ref = node.get("defaultBranchRef") or {}
            repos.append(
                RepoInfo(
                    name=node.get("name", ""),
                    name_with_owner=node.get("nameWithOwner", ""),
                    default_branch_name=default_ref.get("name", "") if default_ref else "",
                )
            )

        page_info = repositories.get("pageInfo", {})
        if not page_info.get("hasNextPage"):
            break
        cursor = page_info.get("endCursor")

    return repos


# ---------- Phase 2: Collect signals ----------


def get_protected_branches(owner: str, repo: str) -> list[str]:
    """Signal: branch protection rule patterns (GraphQL)."""
    query = """query($owner: String!, $repo: String!) {
        repository(owner: $owner, name: $repo) {
            branchProtectionRules(first: 50) {
                nodes { pattern }
            }
        }
    }"""
    data = do_graphql(query, {"owner": owner, "repo": repo})
    repository = (data or {}).get("repository") or {}
    nodes = (repository.get("branchProtectionRules") or {}).get("nodes") or []
    return [n.get("pattern", "") for n in nodes if n.get("pattern")]


def get_ruleset_branches(owner: str, repo: str) -> list[str]:
    """Signal: branches targeted by active repo-level rulesets (REST)."""
    seen: set[str] = set()

    # The list endpoint doesn't include conditions, so each ruleset is fetched.
    try:
        rulesets = _rest_json("GET", f"/repos/{owner}/{repo}/rulesets") or []
    except RuntimeError:
        rulesets = []

    for rs in rulesets:
        if rs.get("enforcement") != "active" or rs.get("target") != "branch":
            continue
        try:
            detail = _rest_json("GET", f"/repos/{owner}/{repo}/rulesets/{rs.get('id')}")
        except RuntimeError:
            continue
        conditions = (detail or {}).get("conditions") or {}
        ref_name = conditions.get("ref_name") or {}
        for pattern in ref_name.get("include") or []:
            b = _trim_prefix(pattern, "refs/heads/")
            if b in ("~DEFAULT_BRANCH", ""):
                b = "~DEFAULT_BRANCH"
            seen.add(b)

    return sorted(seen)


def get_deployment_branches(owner: str, repo: str) -> list[str]:
    """Signal: branches deployed to a "production" environment (GraphQL)."""
    query = """query($owner: String!, $repo: String!, $cursor: String) {
        repository(owner: $owner, name: $repo) {
            deployments(first: 100, after: $cursor, environments: ["production"]) {
                pageInfo { hasNextPage endCursor }
                nodes {
                    ref { name }
                }
            }
        }
    }"""
    counts: dict[str, int] = {}
    cursor: Optional[str] = None

    while True:
        variables: dict[str, Any] = {"owner": owner, "repo": repo}
        if cursor is not None:
            variables["cursor"] = cursor
        data = do_graphql(query, variables)
        repository = (data or {}).get("repository") or {}
        deployments = repository.get("deployments") or {}
        for node in deployments.get("nodes") or []:
            ref = node.get("ref") or {}
            name = ref.get("name") if ref else ""
            if name:
                counts[name] = counts.get(name, 0) + 1
        page_info = deployments.get("pageInfo", {})
        if not page_info.get("hasNextPage"):
            break
        cursor = page_info.get("endCursor")

    return sorted_keys_by_value(counts)


def get_release_branches(owner: str, repo: str) -> list[str]:
    """Signal: branches releases were cut from via ``target_commitish`` (REST)."""
    releases = _rest_json("GET", f"/repos/{owner}/{repo}/releases?per_page={PER_PAGE_REST}") or []
    counts: dict[str, int] = {}
    for r in releases:
        if r.get("draft"):
            continue
        target = r.get("target_commitish")
        if target:
            counts[target] = counts.get(target, 0) + 1
    return sorted_keys_by_value(counts)


def get_tag_count_by_branch(owner: str, repo: str) -> dict[str, int]:
    """Signal: number of release tags per branch (REST)."""
    releases = _rest_json("GET", f"/repos/{owner}/{repo}/releases?per_page={PER_PAGE_REST}") or []
    counts: dict[str, int] = {}
    for r in releases:
        if not r.get("draft") and r.get("target_commitish") and r.get("tag_name"):
            target = r["target_commitish"]
            counts[target] = counts.get(target, 0) + 1
    return counts


def get_top_pr_merge_target(owner: str, repo: str, candidate_branches: list[str]) -> str:
    """Signal: branch receiving the most closed PRs among the top candidates (REST)."""
    if not candidate_branches:
        return ""

    results: list[tuple[str, int]] = []  # (branch, count)
    for branch in candidate_branches[:5]:
        path = f"/repos/{owner}/{repo}/pulls?state=closed&base={branch}&per_page=1"
        try:
            body, headers = do_rest("GET", path)
        except RuntimeError:
            continue
        count = estimate_count_from_response(body, headers.get("Link", ""))
        results.append((branch, count))

    if not results:
        return ""
    results.sort(key=lambda bc: bc[1], reverse=True)
    top_branch, top_count = results[0]
    if top_count > 0:
        return f"{top_branch} ({top_count} PRs)"
    return ""


def get_workflow_push_branches(owner: str, repo: str) -> list[str]:
    """Signal: branches listed in ``on.push.branches`` in Actions workflows (REST)."""
    files = _rest_json("GET", f"/repos/{owner}/{repo}/contents/.github/workflows") or []

    seen: set[str] = set()
    for f in files:
        name = f.get("name", "")
        if not name.endswith(".yml") and not name.endswith(".yaml"):
            continue
        for b in parse_workflow_push_branches(owner, repo, name):
            seen.add(b)
    return sorted(seen)


def parse_workflow_push_branches(owner: str, repo: str, filename: str) -> list[str]:
    try:
        content = _rest_json(
            "GET", f"/repos/{owner}/{repo}/contents/.github/workflows/{filename}"
        )
    except RuntimeError:
        return []
    content = content or {}
    if content.get("encoding") != "base64":
        return []
    try:
        decoded = decode_base64(content.get("content", ""))
    except Exception:
        return []
    # Simple line-based YAML parsing for on.push.branches to avoid a YAML dep,
    # matching the Go implementation exactly.
    return extract_push_branches(decoded)


def extract_push_branches(yaml_content: str) -> list[str]:
    branches: list[str] = []
    in_push = False
    in_branches = False
    push_indent = 0
    branches_indent = 0

    for line in yaml_content.split("\n"):
        trimmed = line.strip()
        if trimmed == "" or trimmed.startswith("#"):
            continue

        indent = len(line) - len(line.lstrip(" "))

        if trimmed == "push:":
            in_push = True
            push_indent = indent
            in_branches = False
            continue

        if in_push and indent <= push_indent and trimmed != "" and not trimmed.startswith("#"):
            if not trimmed.startswith("branches"):
                in_push = False
                in_branches = False
                continue

        if in_push and (trimmed == "branches:" or trimmed.startswith("branches:")):
            in_branches = True
            branches_indent = indent
            # Handle inline form: branches: [main, develop]
            if "[" in trimmed:
                inner = trimmed[trimmed.index("[") + 1:]
                if inner.endswith("]"):
                    inner = inner[:-1]
                for b in inner.split(","):
                    b = b.strip().strip("'\"")
                    if b:
                        branches.append(b)
                in_branches = False
            continue

        if in_branches:
            if indent <= branches_indent:
                in_branches = False
                in_push = False
                continue
            if trimmed.startswith("- "):
                b = trimmed[2:].strip().strip("'\"")
                if b:
                    branches.append(b)

    return branches


def get_most_active_branch(owner: str, repo: str, branches: list[str]) -> str:
    """Signal: branch with the highest commit count in the lookback window (GraphQL)."""
    if not branches:
        return ""

    since = (
        datetime.now(timezone.utc) - timedelta(days=COMMIT_LOOKBACK_MONTHS * 30)
    ).strftime("%Y-%m-%dT%H:%M:%SZ")

    query = """query($owner: String!, $repo: String!, $branch: String!, $since: GitTimestamp!) {
        repository(owner: $owner, name: $repo) {
            ref(qualifiedName: $branch) {
                target {
                    ... on Commit {
                        history(since: $since) {
                            totalCount
                        }
                    }
                }
            }
        }
    }"""

    results: list[tuple[str, int]] = []
    for branch in branches[:5]:
        try:
            data = do_graphql(
                query,
                {
                    "owner": owner,
                    "repo": repo,
                    "branch": "refs/heads/" + branch,
                    "since": since,
                },
            )
        except (RuntimeError, GraphQLError):
            continue
        ref = ((data or {}).get("repository") or {}).get("ref")
        if ref:
            total = (((ref.get("target") or {}).get("history") or {}).get("totalCount")) or 0
            results.append((branch, total))

    if not results:
        return ""
    results.sort(key=lambda bc: bc[1], reverse=True)
    return f"{results[0][0]} ({results[0][1]} commits)"


def get_oldest_branch(owner: str, repo: str, branches: list[str]) -> str:
    """Signal: branch with the most total commits — proxy for longest-lived (GraphQL)."""
    if not branches:
        return ""

    query = """query($owner: String!, $repo: String!, $branch: String!) {
        repository(owner: $owner, name: $repo) {
            ref(qualifiedName: $branch) {
                target {
                    ... on Commit {
                        history(first: 0) {
                            totalCount
                        }
                    }
                }
            }
        }
    }"""

    results: list[tuple[str, int]] = []
    for branch in branches[:5]:
        try:
            data = do_graphql(
                query,
                {"owner": owner, "repo": repo, "branch": "refs/heads/" + branch},
            )
        except (RuntimeError, GraphQLError):
            continue
        ref = ((data or {}).get("repository") or {}).get("ref")
        if ref:
            total = (((ref.get("target") or {}).get("history") or {}).get("totalCount")) or 0
            results.append((branch, total))

    if not results:
        return ""
    results.sort(key=lambda bc: bc[1], reverse=True)
    return f"{results[0][0]} ({results[0][1]} total commits)"


def list_candidate_branches(owner: str, repo: str, default_branch: str) -> list[str]:
    """Return a prioritized list of candidate branches for expensive per-branch queries."""
    query = """query($owner: String!, $repo: String!) {
        repository(owner: $owner, name: $repo) {
            refs(refPrefix: "refs/heads/", first: 50, orderBy: {field: TAG_COMMIT_DATE, direction: DESC}) {
                nodes { name }
            }
        }
    }"""
    data = do_graphql(query, {"owner": owner, "repo": repo})
    repository = (data or {}).get("repository") or {}
    nodes = (repository.get("refs") or {}).get("nodes") or []

    priority = {
        default_branch,
        "main",
        "master",
        "production",
        "release",
        "deploy",
        "stable",
        "trunk",
    }

    prioritized: list[str] = []
    other: list[str] = []
    seen: set[str] = set()
    for node in nodes:
        name = node.get("name", "")
        if name in priority and name not in seen:
            prioritized.append(name)
            seen.add(name)
        elif name not in seen:
            other.append(name)
            seen.add(name)

    candidates = list(prioritized)
    if other:
        candidates.extend(other[:3])  # add a few others for diversity

    if not candidates and default_branch:
        candidates = [default_branch]

    return candidates


# ---------- Utilities ----------


def decode_base64(s: str) -> str:
    s = s.replace("\n", "")
    return base64.standard_b64decode(s).decode("utf-8", errors="replace")


def sorted_keys_by_value(m: dict[str, int]) -> list[str]:
    """Return keys sorted by value, descending."""
    return [k for k, _ in sorted(m.items(), key=lambda kv: kv[1], reverse=True)]


def _last_page_from_link(link: str) -> Optional[int]:
    """Return the ``rel="last"`` page number from a REST ``Link`` header, if present."""
    if not link:
        return None
    for part in link.split(","):
        if 'rel="last"' in part:
            start = part.rfind("page=")
            if start == -1:
                continue
            num_str = part[start + 5:]
            end = _index_any(num_str, ">&")
            if end != -1:
                num_str = num_str[:end]
            try:
                return int(num_str)
            except ValueError:
                return None
    return None


def estimate_count_from_response(body: bytes, link: str) -> int:
    """Estimate the total item count for a ``per_page=1`` listing.

    With ``per_page=1`` each page holds one item, so the ``rel="last"`` page
    number in the ``Link`` header equals the total count. GitHub omits the
    ``Link`` header entirely when the results fit on a single page (0 or 1
    items), so when it is absent we fall back to the number of items actually
    returned in the body. This correctly reports 0 for an empty result set
    instead of a spurious 1.
    """
    page = _last_page_from_link(link)
    if page is not None:
        return page
    try:
        items = _json_loads(body) if body else []
    except ValueError:
        return 0
    return len(items) if isinstance(items, list) else 0


def unique(items: list[str]) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for s in items:
        if s and s not in seen:
            seen.add(s)
            out.append(s)
    return out


def _trim_prefix(s: str, prefix: str) -> str:
    return s[len(prefix):] if s.startswith(prefix) else s


def _index_any(s: str, chars: str) -> int:
    for i, ch in enumerate(s):
        if ch in chars:
            return i
    return -1


T = TypeVar("T")


def safe(fn: Callable[[], T], default: T) -> T:
    """Run ``fn`` and return its result, or ``default`` on any exception.

    Mirrors the Go code's pattern of ignoring per-signal errors so one failing
    signal never aborts the analysis of a repository.
    """
    try:
        return fn()
    except Exception:
        return default


# ---------- Lightweight mode ----------


def get_lightweight_signals(
    owner: str, repo: str, default_branch: str
) -> tuple[list[str], list[str], list[str], str]:
    """Fetch the most important signals quickly.

    Returns ``(protected_branches, ruleset_branches, tagged_branches, top_pr_target)``.
    """
    protected_branches: list[str] = []
    ruleset_branches: list[str] = []
    tagged_branches: list[str] = []
    top_pr_target = ""

    # 1. Branch protection rules via GraphQL.
    query = """query($owner: String!, $repo: String!) {
        repository(owner: $owner, name: $repo) {
            branchProtectionRules(first: 50) {
                nodes { pattern }
            }
        }
    }"""
    try:
        data = do_graphql(query, {"owner": owner, "repo": repo})
        repository = (data or {}).get("repository") or {}
        nodes = (repository.get("branchProtectionRules") or {}).get("nodes") or []
        protected_branches = [n.get("pattern", "") for n in nodes if n.get("pattern")]
    except (RuntimeError, GraphQLError):
        pass

    # 2. Repo rulesets.
    ruleset_branches = safe(lambda: get_ruleset_branches(owner, repo), [])

    # 3. Tags from releases (which branches have tags).
    try:
        releases = (
            _rest_json("GET", f"/repos/{owner}/{repo}/releases?per_page={PER_PAGE_REST}") or []
        )
        tag_counts: dict[str, int] = {}
        for r in releases:
            if not r.get("draft") and r.get("target_commitish") and r.get("tag_name"):
                target = r["target_commitish"]
                tag_counts[target] = tag_counts.get(target, 0) + 1
        tagged_branches = sorted_keys_by_value(tag_counts)
    except RuntimeError:
        pass

    # 4. Top PR merge target — check default + well-known branches (few REST calls).
    candidates = unique(
        [default_branch, "main", "master", "production", "release", "deploy"]
    )
    top_pr_target = safe(lambda: get_top_pr_merge_target(owner, repo, candidates), "")

    return protected_branches, ruleset_branches, tagged_branches, top_pr_target


# ---------- CSV output ----------

CSV_HEADER = [
    "Repository",
    "Default Branch",
    "Protected Branches",
    "Ruleset Target Branches",
    "Tagged Branches (by count)",
    "Top PR Merge Target",
    "Deployment Branches (prod)",
    "Release Target Branches",
    "Workflow Push Branches",
    "Most Active Branch (6mo)",
    "Deepest Branch (total commits)",
]


def result_to_row(r: RepoResult) -> list[str]:
    return [
        r.name,
        r.default_branch,
        "; ".join(r.protected_branches),
        "; ".join(r.ruleset_target_branches),
        "; ".join(r.tagged_branches),
        r.top_pr_merge_target,
        "; ".join(r.deployment_branches),
        "; ".join(r.release_target_branches),
        "; ".join(r.workflow_push_branches),
        r.most_active_commit_branch,
        r.oldest_branch,
    ]


# ---------- Main ----------


def analyze_repo(repo: RepoInfo, light: bool) -> RepoResult:
    owner, _, repo_name = repo.name_with_owner.partition("/")

    r = RepoResult(name=repo.name_with_owner, default_branch=repo.default_branch_name)

    if light:
        (
            r.protected_branches,
            r.ruleset_target_branches,
            r.tagged_branches,
            r.top_pr_merge_target,
        ) = safe(
            lambda: get_lightweight_signals(owner, repo_name, repo.default_branch_name),
            ([], [], [], ""),
        )
    else:
        candidates = safe(
            lambda: list_candidate_branches(owner, repo_name, repo.default_branch_name), []
        )

        r.protected_branches = safe(lambda: get_protected_branches(owner, repo_name), [])
        r.ruleset_target_branches = safe(lambda: get_ruleset_branches(owner, repo_name), [])
        r.deployment_branches = safe(lambda: get_deployment_branches(owner, repo_name), [])
        r.release_target_branches = safe(lambda: get_release_branches(owner, repo_name), [])

        tag_counts = safe(lambda: get_tag_count_by_branch(owner, repo_name), {})
        r.tagged_branches = sorted_keys_by_value(tag_counts)

        r.top_pr_merge_target = safe(
            lambda: get_top_pr_merge_target(owner, repo_name, candidates), ""
        )
        r.workflow_push_branches = safe(lambda: get_workflow_push_branches(owner, repo_name), [])
        r.most_active_commit_branch = safe(
            lambda: get_most_active_branch(owner, repo_name, candidates), ""
        )
        r.oldest_branch = safe(lambda: get_oldest_branch(owner, repo_name, candidates), "")

    return r


def print_coverage_summary(results: list[RepoResult], light: bool) -> None:
    # (name, light, predicate)
    signals: list[tuple[str, bool, Callable[[RepoResult], bool]]] = [
        ("Default Branch", True, lambda r: r.default_branch != ""),
        ("Protected Branches", True, lambda r: len(r.protected_branches) > 0),
        ("Ruleset Targets", True, lambda r: len(r.ruleset_target_branches) > 0),
        ("Deployments (prod)", False, lambda r: len(r.deployment_branches) > 0),
        ("Release Targets", False, lambda r: len(r.release_target_branches) > 0),
        ("Tagged Branches", True, lambda r: len(r.tagged_branches) > 0),
        ("PR Merge Target", True, lambda r: r.top_pr_merge_target != ""),
        ("Workflow Push", False, lambda r: len(r.workflow_push_branches) > 0),
        ("Commit Activity", False, lambda r: r.most_active_commit_branch != ""),
        ("Branch Depth", False, lambda r: r.oldest_branch != ""),
    ]

    table = Table(show_header=True, header_style="bold")
    table.add_column("Signal")
    table.add_column("Repos with data", justify="right")
    table.add_column("Coverage", justify="right")

    total = len(results)
    for name, is_light, predicate in signals:
        if light and not is_light:
            continue
        count = sum(1 for r in results if predicate(r))
        pct = (count * 100 // total) if total > 0 else 0
        table.add_row(name, str(count), f"{pct}%")

    console.print()
    console.print(table)


def run(org: str, output_file: str, light: bool) -> int:
    global _auth_token
    _auth_token = get_token()

    # Header.
    console.print()
    console.print(Panel.fit(" Production Branch Report ", style="bold black on cyan"))
    console.print()
    info(f"Organization: [bold]{org}[/bold]")
    if light:
        info(
            "Mode: lightweight (5 signals: default branch, PR targets, rulesets, "
            "protection rules, tags)"
        )
    else:
        info("Mode: full (10 signals, ~10+ API calls/repo)")

    # Phase 1: Enumerate repos.
    try:
        with console.status("Discovering repositories...", spinner="dots"):
            repos = list_org_repos(org)
    except Exception as exc:  # noqa: BLE001 — surface any discovery failure and exit
        error(f"Failed to list repos: {exc}")
        return 1
    success(f"Found {len(repos)} repositories")
    console.print()

    # Phase 2: Analyze repos with a progress bar.
    results: list[RepoResult] = []
    with Progress(
        TextColumn("[progress.description]{task.description}"),
        BarColumn(complete_style="cyan", finished_style="cyan"),
        MofNCompleteColumn(),
        TimeElapsedColumn(),
        console=console,
    ) as progress:
        task = progress.add_task("Analyzing repositories", total=len(repos))
        for repo in repos:
            progress.update(task, description=f"Analyzing {repo.name}")
            results.append(analyze_repo(repo, light))
            progress.advance(task)

    # Phase 3: Write CSV output.
    if output_file:
        try:
            out_handle = open(output_file, "w", newline="", encoding="utf-8")
        except OSError as exc:
            fatal(f"Failed to create output file: {exc}")
            return 1
        close_when_done = True
    else:
        out_handle = sys.stdout
        close_when_done = False

    try:
        writer = csv.writer(out_handle)
        writer.writerow(CSV_HEADER)
        for r in results:
            writer.writerow(result_to_row(r))
    finally:
        if close_when_done:
            out_handle.close()

    # Summary.
    console.print()
    if output_file:
        success(f"Report written to {output_file} ({len(results)} repos)")
    else:
        success(f"Report complete ({len(results)} repos)")

    print_coverage_summary(results, light)
    return 0


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="prod_branch_report.py",
        description=(
            "Analyze an org's repositories to surface signals about which "
            "branches are likely production branches."
        ),
    )
    parser.add_argument("org", help="GitHub organization slug to analyze")
    parser.add_argument(
        "output",
        nargs="?",
        default="",
        help="Optional CSV output path (defaults to stdout)",
    )
    parser.add_argument(
        "--light",
        action="store_true",
        help="Lightweight mode — 5 key signals, faster",
    )
    return parser.parse_args(argv)


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    try:
        return run(args.org, args.output, args.light)
    except KeyboardInterrupt:
        console.print()
        warning("Interrupted.")
        return 130


if __name__ == "__main__":
    sys.exit(main())
