package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/pterm/pterm"
)

// ---------- Configuration ----------

const (
	graphqlURL       = "https://api.github.com/graphql"
	restBaseURL      = "https://api.github.com"
	perPageREST      = 100
	commitLookbackMo = 6
)

// ---------- Types ----------

type RepoResult struct {
	Name                    string
	DefaultBranch           string
	ProtectedBranches       []string // branch patterns with protection rules
	RulesetTargetBranches   []string // branches targeted by active rulesets
	DeploymentBranches      []string // branches deployed to production env
	ReleaseTargetBranches   []string // branches from release target_commitish
	TaggedBranches          []string // branches associated with most tags (via releases)
	TopPRMergeTarget        string   // branch receiving the most merged PRs
	WorkflowPushBranches    []string // branches in on.push.branches triggers
	MostActiveCommitBranch  string   // branch with highest commit count in lookback
	OldestBranch            string   // branch with the most total commits (deepest history)
	// AI hypothesis (populated only with --analyze flag)
	AIHypothesis *BranchHypothesis
}

// BranchHypothesis is the AI-generated analysis of production branch patterns.
type BranchHypothesis struct {
	MultipleProductionBranches bool     `json:"multiple_production_branches"`
	CandidateBranches          []string `json:"candidate_branches"`
	Confidence                 string   `json:"confidence"` // high, medium, low
	Reasoning                  string   `json:"reasoning"`
}

// ---------- GraphQL helpers ----------

type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func getToken() string {
	token := os.Getenv("GITHUB_TOKEN")
	if token != "" {
		return token
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		pterm.Fatal.Println("No GITHUB_TOKEN set and `gh auth token` failed. Provide a token.")
	}
	return strings.TrimSpace(string(out))
}

var httpClient = &http.Client{Timeout: 30 * time.Second}
var authToken string
var rateLimitMu sync.Mutex

// handleRateLimit checks response headers for rate limiting and waits if needed.
// Returns true if the request should be retried.
func handleRateLimit(resp *http.Response, source string) bool {
	if resp == nil {
		return false
	}

	// Secondary rate limit: 403 or 429 with Retry-After header
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		retryAfter := resp.Header.Get("Retry-After")
		if retryAfter != "" {
			seconds, err := strconv.Atoi(retryAfter)
			if err != nil {
				seconds = 60
			}
			waitForRateLimit(time.Duration(seconds)*time.Second, "secondary", source)
			return true
		}
		// Check if it's a rate limit message in body
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "rate limit") || strings.Contains(string(body), "abuse") {
			waitForRateLimit(60*time.Second, "secondary", source)
			return true
		}
	}

	// Primary rate limit: X-RateLimit-Remaining is 0
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	if remaining == "0" {
		resetStr := resp.Header.Get("X-RateLimit-Reset")
		if resetStr != "" {
			resetUnix, err := strconv.ParseInt(resetStr, 10, 64)
			if err == nil {
				resetTime := time.Unix(resetUnix, 0)
				waitDuration := time.Until(resetTime) + 1*time.Second
				if waitDuration > 0 {
					resource := resp.Header.Get("X-RateLimit-Resource")
					if resource == "" {
						resource = "core"
					}
					waitForRateLimit(waitDuration, "primary ("+resource+")", source)
					return true
				}
			}
		}
	}

	return false
}

func waitForRateLimit(duration time.Duration, limitType, source string) {
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()

	resumeAt := time.Now().Add(duration)
	pterm.Warning.Printfln("⏳ %s rate limit hit (%s). Waiting %s (resuming at %s)",
		limitType, source, duration.Round(time.Second), resumeAt.Format("15:04:05"))

	// Show a countdown spinner
	spinner, _ := pterm.DefaultSpinner.
		WithRemoveWhenDone(true).
		Start(fmt.Sprintf("Rate limited — resuming in %s...", duration.Round(time.Second)))

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timer := time.NewTimer(duration)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			spinner.Success("Rate limit wait complete. Resuming...")
			return
		case <-ticker.C:
			remaining := time.Until(resumeAt).Round(time.Second)
			spinner.UpdateText(fmt.Sprintf("Rate limited — resuming in %s...", remaining))
		}
	}
}

func doGraphQL(query string, variables map[string]interface{}) (json.RawMessage, error) {
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		body, _ := json.Marshal(graphQLRequest{Query: query, Variables: variables})
		req, _ := http.NewRequest("POST", graphqlURL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if handleRateLimit(resp, "GraphQL") {
			resp.Body.Close()
			continue
		}

		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var gqlResp graphQLResponse
		if err := json.Unmarshal(data, &gqlResp); err != nil {
			return nil, fmt.Errorf("graphql unmarshal: %w\nraw: %s", err, string(data))
		}
		if len(gqlResp.Errors) > 0 {
			return gqlResp.Data, fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
		}
		return gqlResp.Data, nil
	}
	return nil, fmt.Errorf("graphql: max retries exceeded due to rate limiting")
}

func doREST(method, path string) ([]byte, http.Header, error) {
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		url := restBaseURL + path
		req, _ := http.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, nil, err
		}

		if handleRateLimit(resp, "REST "+method+" "+path) {
			resp.Body.Close()
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			return nil, resp.Header, fmt.Errorf("REST %s %s: %d %s", method, path, resp.StatusCode, string(body))
		}
		return body, resp.Header, nil
	}
	return nil, nil, fmt.Errorf("REST %s %s: max retries exceeded due to rate limiting", method, path)
}

// ---------- Phase 1: Enumerate repos ----------

type repoInfo struct {
	Name              string
	NameWithOwner     string
	DefaultBranchName string
}

func listOrgRepos(org string) ([]repoInfo, error) {
	query := `query($org: String!, $cursor: String) {
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
	}`

	var repos []repoInfo
	var cursor *string

	for {
		vars := map[string]interface{}{"org": org}
		if cursor != nil {
			vars["cursor"] = *cursor
		}
		data, err := doGraphQL(query, vars)
		if err != nil {
			return nil, fmt.Errorf("listOrgRepos: %w", err)
		}

		var result struct {
			Organization struct {
				Repositories struct {
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
					Nodes []struct {
						Name            string
						NameWithOwner   string
						DefaultBranchRef *struct{ Name string }
					}
				}
			}
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}

		for _, n := range result.Organization.Repositories.Nodes {
			def := ""
			if n.DefaultBranchRef != nil {
				def = n.DefaultBranchRef.Name
			}
			repos = append(repos, repoInfo{Name: n.Name, NameWithOwner: n.NameWithOwner, DefaultBranchName: def})
		}

		if !result.Organization.Repositories.PageInfo.HasNextPage {
			break
		}
		c := result.Organization.Repositories.PageInfo.EndCursor
		cursor = &c
	}
	return repos, nil
}

// ---------- Phase 2: Collect signals ----------

// Signal: Branch protection rules
func getProtectedBranches(owner, repo string) ([]string, error) {
	query := `query($owner: String!, $repo: String!) {
		repository(owner: $owner, name: $repo) {
			branchProtectionRules(first: 50) {
				nodes { pattern }
			}
		}
	}`
	data, err := doGraphQL(query, map[string]interface{}{"owner": owner, "repo": repo})
	if err != nil {
		return nil, err
	}
	var result struct {
		Repository struct {
			BranchProtectionRules struct {
				Nodes []struct{ Pattern string }
			}
		}
	}
	json.Unmarshal(data, &result)
	var patterns []string
	for _, n := range result.Repository.BranchProtectionRules.Nodes {
		patterns = append(patterns, n.Pattern)
	}
	return patterns, nil
}

// Signal: Repository rulesets (branch targets)
func getRulesetBranches(owner, repo string) ([]string, error) {
	seen := map[string]bool{}

	type rulesetSummary struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Target      string `json:"target"`
		Enforcement string `json:"enforcement"`
	}

	type rulesetDetail struct {
		ID         int    `json:"id"`
		Target     string `json:"target"`
		Conditions struct {
			RefName struct {
				Include []string `json:"include"`
			} `json:"ref_name"`
		} `json:"conditions"`
	}

	// Repo-level rulesets (list endpoint doesn't include conditions, must fetch each)
	body, _, err := doREST("GET", fmt.Sprintf("/repos/%s/%s/rulesets", owner, repo))
	if err == nil {
		var rulesets []rulesetSummary
		json.Unmarshal(body, &rulesets)
		for _, rs := range rulesets {
			if rs.Enforcement != "active" || rs.Target != "branch" {
				continue
			}
			// Fetch individual ruleset to get conditions
			detailBody, _, err := doREST("GET", fmt.Sprintf("/repos/%s/%s/rulesets/%d", owner, repo, rs.ID))
			if err != nil {
				continue
			}
			var detail rulesetDetail
			json.Unmarshal(detailBody, &detail)
			for _, pattern := range detail.Conditions.RefName.Include {
				b := strings.TrimPrefix(pattern, "refs/heads/")
				if b == "~DEFAULT_BRANCH" || b == "" {
					b = "~DEFAULT_BRANCH"
				}
				seen[b] = true
			}
		}
	}

	var result []string
	for b := range seen {
		result = append(result, b)
	}
	sort.Strings(result)
	return result, nil
}

// Signal: Deployments to production
func getDeploymentBranches(owner, repo string) ([]string, error) {
	query := `query($owner: String!, $repo: String!, $cursor: String) {
		repository(owner: $owner, name: $repo) {
			deployments(first: 100, after: $cursor, environments: ["production"]) {
				pageInfo { hasNextPage endCursor }
				nodes {
					ref { name }
				}
			}
		}
	}`
	counts := map[string]int{}
	var cursor *string

	for {
		vars := map[string]interface{}{"owner": owner, "repo": repo}
		if cursor != nil {
			vars["cursor"] = *cursor
		}
		data, err := doGraphQL(query, vars)
		if err != nil {
			return nil, err
		}
		var result struct {
			Repository struct {
				Deployments struct {
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
					Nodes []struct {
						Ref *struct{ Name string }
					}
				}
			}
		}
		json.Unmarshal(data, &result)

		for _, n := range result.Repository.Deployments.Nodes {
			if n.Ref != nil && n.Ref.Name != "" {
				counts[n.Ref.Name]++
			}
		}
		if !result.Repository.Deployments.PageInfo.HasNextPage {
			break
		}
		c := result.Repository.Deployments.PageInfo.EndCursor
		cursor = &c
	}

	return sortedKeysByValue(counts), nil
}

// Signal: Release target_commitish
func getReleaseBranches(owner, repo string) ([]string, error) {
	body, _, err := doREST("GET", fmt.Sprintf("/repos/%s/%s/releases?per_page=%d", owner, repo, perPageREST))
	if err != nil {
		return nil, err
	}
	var releases []struct {
		TargetCommitish string `json:"target_commitish"`
		Draft           bool   `json:"draft"`
		Prerelease      bool   `json:"prerelease"`
		TagName         string `json:"tag_name"`
	}
	json.Unmarshal(body, &releases)

	counts := map[string]int{}
	for _, r := range releases {
		if r.Draft {
			continue
		}
		if r.TargetCommitish != "" {
			counts[r.TargetCommitish]++
		}
	}
	return sortedKeysByValue(counts), nil
}

// Signal: Tags (via releases target_commitish, grouped)
// Already covered by getReleaseBranches — this provides tag count per branch
func getTagCountByBranch(owner, repo string) (map[string]int, error) {
	body, _, err := doREST("GET", fmt.Sprintf("/repos/%s/%s/releases?per_page=%d", owner, repo, perPageREST))
	if err != nil {
		return nil, err
	}
	var releases []struct {
		TargetCommitish string `json:"target_commitish"`
		TagName         string `json:"tag_name"`
		Draft           bool   `json:"draft"`
	}
	json.Unmarshal(body, &releases)

	counts := map[string]int{}
	for _, r := range releases {
		if !r.Draft && r.TargetCommitish != "" && r.TagName != "" {
			counts[r.TargetCommitish]++
		}
	}
	return counts, nil
}

// Signal: PR merge target
func getTopPRMergeTarget(owner, repo string, candidateBranches []string) (string, error) {
	if len(candidateBranches) == 0 {
		return "", nil
	}

	// Check top 5 candidate branches
	limit := 5
	if len(candidateBranches) < limit {
		limit = len(candidateBranches)
	}

	type branchCount struct {
		branch string
		count  int
	}
	var results []branchCount

	for _, branch := range candidateBranches[:limit] {
		// Use per_page=1 and check total count from response
		path := fmt.Sprintf("/repos/%s/%s/pulls?state=closed&base=%s&per_page=1", owner, repo, branch)
		_, headers, err := doREST("GET", path)
		if err != nil {
			continue
		}
		// Parse Link header to estimate total count
		count := estimateCountFromLink(headers.Get("Link"))
		results = append(results, branchCount{branch: branch, count: count})
	}

	if len(results) == 0 {
		return "", nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].count > results[j].count })
	if results[0].count > 0 {
		return fmt.Sprintf("%s (%d PRs)", results[0].branch, results[0].count), nil
	}
	return "", nil
}

// Signal: Workflow push branches
func getWorkflowPushBranches(owner, repo string) ([]string, error) {
	body, _, err := doREST("GET", fmt.Sprintf("/repos/%s/%s/contents/.github/workflows", owner, repo))
	if err != nil {
		return nil, err // no workflows dir
	}
	var files []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
	}
	json.Unmarshal(body, &files)

	seen := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".yml") && !strings.HasSuffix(f.Name, ".yaml") {
			continue
		}
		branches := parseWorkflowPushBranches(owner, repo, f.Name)
		for _, b := range branches {
			seen[b] = true
		}
	}
	var result []string
	for b := range seen {
		result = append(result, b)
	}
	sort.Strings(result)
	return result, nil
}

func parseWorkflowPushBranches(owner, repo, filename string) []string {
	path := fmt.Sprintf("/repos/%s/%s/contents/.github/workflows/%s", owner, repo, filename)
	body, _, err := doREST("GET", path)
	if err != nil {
		return nil
	}
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	json.Unmarshal(body, &content)

	if content.Encoding != "base64" {
		return nil
	}

	decoded, err := decodeBase64(content.Content)
	if err != nil {
		return nil
	}

	// Simple YAML parsing for on.push.branches — look for the pattern
	// We do a simple line-based parse since importing a YAML lib adds deps
	return extractPushBranches(decoded)
}

func extractPushBranches(yamlContent string) []string {
	lines := strings.Split(yamlContent, "\n")
	var branches []string
	inPush := false
	inBranches := false
	pushIndent := 0
	branchesIndent := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))

		if trimmed == "push:" {
			inPush = true
			pushIndent = indent
			inBranches = false
			continue
		}

		if inPush && indent <= pushIndent && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if !strings.HasPrefix(trimmed, "branches") {
				inPush = false
				inBranches = false
				continue
			}
		}

		if inPush && (trimmed == "branches:" || strings.HasPrefix(trimmed, "branches:")) {
			inBranches = true
			branchesIndent = indent
			// Check inline: branches: [main, develop]
			if strings.Contains(trimmed, "[") {
				inner := trimmed[strings.Index(trimmed, "[")+1:]
				inner = strings.TrimSuffix(inner, "]")
				for _, b := range strings.Split(inner, ",") {
					b = strings.TrimSpace(b)
					b = strings.Trim(b, "'\"")
					if b != "" {
						branches = append(branches, b)
					}
				}
				inBranches = false
			}
			continue
		}

		if inBranches {
			if indent <= branchesIndent {
				inBranches = false
				inPush = false
				continue
			}
			if strings.HasPrefix(trimmed, "- ") {
				b := strings.TrimPrefix(trimmed, "- ")
				b = strings.TrimSpace(b)
				b = strings.Trim(b, "'\"")
				if b != "" {
					branches = append(branches, b)
				}
			}
		}
	}
	return branches
}

// Signal: Commit velocity
func getMostActiveBranch(owner, repo string, branches []string) (string, error) {
	if len(branches) == 0 {
		return "", nil
	}

	since := time.Now().AddDate(0, -commitLookbackMo, 0).Format(time.RFC3339)
	limit := 5
	if len(branches) < limit {
		limit = len(branches)
	}

	type branchCount struct {
		branch string
		count  int
	}
	var results []branchCount

	for _, branch := range branches[:limit] {
		query := `query($owner: String!, $repo: String!, $branch: String!, $since: GitTimestamp!) {
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
		}`
		vars := map[string]interface{}{
			"owner":  owner,
			"repo":   repo,
			"branch": "refs/heads/" + branch,
			"since":  since,
		}
		data, err := doGraphQL(query, vars)
		if err != nil {
			continue
		}
		var result struct {
			Repository struct {
				Ref *struct {
					Target struct {
						History struct {
							TotalCount int
						}
					}
				}
			}
		}
		json.Unmarshal(data, &result)
		if result.Repository.Ref != nil {
			results = append(results, branchCount{branch: branch, count: result.Repository.Ref.Target.History.TotalCount})
		}
	}

	if len(results) == 0 {
		return "", nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].count > results[j].count })
	return fmt.Sprintf("%s (%d commits)", results[0].branch, results[0].count), nil
}

// Signal: Oldest branch (by total commit depth — more commits = longer-lived)
func getOldestBranch(owner, repo string, branches []string) (string, error) {
	if len(branches) == 0 {
		return "", nil
	}

	limit := 5
	if len(branches) < limit {
		limit = len(branches)
	}

	type branchDepth struct {
		branch string
		count  int
	}
	var results []branchDepth

	for _, branch := range branches[:limit] {
		query := `query($owner: String!, $repo: String!, $branch: String!) {
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
		}`
		vars := map[string]interface{}{
			"owner":  owner,
			"repo":   repo,
			"branch": "refs/heads/" + branch,
		}
		data, err := doGraphQL(query, vars)
		if err != nil {
			continue
		}
		var result struct {
			Repository struct {
				Ref *struct {
					Target struct {
						History struct {
							TotalCount int
						}
					}
				}
			}
		}
		json.Unmarshal(data, &result)
		if result.Repository.Ref != nil {
			results = append(results, branchDepth{
				branch: branch,
				count:  result.Repository.Ref.Target.History.TotalCount,
			})
		}
	}

	if len(results) == 0 {
		return "", nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].count > results[j].count })
	return fmt.Sprintf("%s (%d total commits)", results[0].branch, results[0].count), nil
}

// Signal: list candidate branches (top branches by various signals)
func listCandidateBranches(owner, repo, defaultBranch string) ([]string, error) {
	query := `query($owner: String!, $repo: String!) {
		repository(owner: $owner, name: $repo) {
			refs(refPrefix: "refs/heads/", first: 50, orderBy: {field: TAG_COMMIT_DATE, direction: DESC}) {
				nodes { name }
			}
		}
	}`
	data, err := doGraphQL(query, map[string]interface{}{"owner": owner, "repo": repo})
	if err != nil {
		return nil, err
	}
	var result struct {
		Repository struct {
			Refs struct {
				Nodes []struct{ Name string }
			}
		}
	}
	json.Unmarshal(data, &result)

	// Prioritize well-known branch names and default branch
	priority := map[string]bool{
		defaultBranch: true,
		"main":        true,
		"master":      true,
		"production":  true,
		"release":     true,
		"deploy":      true,
		"stable":      true,
		"trunk":       true,
	}

	var prioritized, other []string
	seen := map[string]bool{}
	for _, n := range result.Repository.Refs.Nodes {
		if priority[n.Name] && !seen[n.Name] {
			prioritized = append(prioritized, n.Name)
			seen[n.Name] = true
		} else if !seen[n.Name] {
			other = append(other, n.Name)
			seen[n.Name] = true
		}
	}

	// Ensure default branch is always first
	candidates := prioritized
	if len(other) > 0 {
		// Add a few others for diversity
		limit := 3
		if len(other) < limit {
			limit = len(other)
		}
		candidates = append(candidates, other[:limit]...)
	}

	if len(candidates) == 0 && defaultBranch != "" {
		candidates = []string{defaultBranch}
	}

	return candidates, nil
}

// ---------- Utilities ----------

func decodeBase64(s string) (string, error) {
	s = strings.ReplaceAll(s, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func sortedKeysByValue(m map[string]int) []string {
	type kv struct {
		key string
		val int
	}
	var sorted []kv
	for k, v := range m {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].val > sorted[j].val })
	var keys []string
	for _, s := range sorted {
		keys = append(keys, s.key)
	}
	return keys
}

func estimateCountFromLink(link string) int {
	// Parse Link header: <...?page=42>; rel="last"
	if link == "" {
		return 1
	}
	parts := strings.Split(link, ",")
	for _, part := range parts {
		if strings.Contains(part, `rel="last"`) {
			// Extract page number
			start := strings.LastIndex(part, "page=")
			if start == -1 {
				continue
			}
			numStr := part[start+5:]
			end := strings.IndexAny(numStr, ">&")
			if end != -1 {
				numStr = numStr[:end]
			}
			var n int
			fmt.Sscanf(numStr, "%d", &n)
			return n
		}
	}
	return 1
}

func unique(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] && s != "" {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---------- AI Hypothesis (Copilot SDK) ----------

func formatSignalSummary(r RepoResult) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Repository: %s", r.Name))
	lines = append(lines, fmt.Sprintf("Default branch: %s", r.DefaultBranch))
	if len(r.ProtectedBranches) > 0 {
		lines = append(lines, fmt.Sprintf("Protected branches: %s", strings.Join(r.ProtectedBranches, ", ")))
	}
	if len(r.RulesetTargetBranches) > 0 {
		lines = append(lines, fmt.Sprintf("Ruleset-targeted branches: %s", strings.Join(r.RulesetTargetBranches, ", ")))
	}
	if len(r.DeploymentBranches) > 0 {
		lines = append(lines, fmt.Sprintf("Branches deployed to production: %s", strings.Join(r.DeploymentBranches, ", ")))
	}
	if len(r.ReleaseTargetBranches) > 0 {
		lines = append(lines, fmt.Sprintf("Release target branches: %s", strings.Join(r.ReleaseTargetBranches, ", ")))
	}
	if len(r.TaggedBranches) > 0 {
		lines = append(lines, fmt.Sprintf("Branches with tags (by count): %s", strings.Join(r.TaggedBranches, ", ")))
	}
	if r.TopPRMergeTarget != "" {
		lines = append(lines, fmt.Sprintf("Top PR merge target: %s", r.TopPRMergeTarget))
	}
	if len(r.WorkflowPushBranches) > 0 {
		lines = append(lines, fmt.Sprintf("Workflow push trigger branches: %s", strings.Join(r.WorkflowPushBranches, ", ")))
	}
	if r.MostActiveCommitBranch != "" {
		lines = append(lines, fmt.Sprintf("Most active branch (6mo): %s", r.MostActiveCommitBranch))
	}
	if r.OldestBranch != "" {
		lines = append(lines, fmt.Sprintf("Deepest branch (total commits): %s", r.OldestBranch))
	}
	return strings.Join(lines, "\n")
}

func analyzeWithCopilot(ctx context.Context, results []RepoResult) error {
	client := copilot.NewClient(&copilot.ClientOptions{
		LogLevel: "error",
	})
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("failed to start Copilot client: %w", err)
	}
	defer client.Stop()

	session, err := client.CreateSession(ctx, &copilot.SessionConfig{
		Model: "claude-sonnet-4.5",
		SystemMessage: &copilot.SystemMessageConfig{
			Mode: "replace",
			Content: `You are an expert at analyzing GitHub repository branching strategies.
You will receive deterministic signal data for repositories and must produce a JSON hypothesis for each.

Rules:
- Respond ONLY with a JSON array — no markdown, no explanation, no code fences.
- Each element must have: "repo" (string), "multiple_production_branches" (boolean), "candidate_branches" (string array), "confidence" ("high"/"medium"/"low"), "reasoning" (string, 1-2 sentences).
- "candidate_branches" should list the branch name(s) you believe serve as production branches, ordered by likelihood.
- Set "multiple_production_branches" to true ONLY when the evidence shows the repo actively maintains multiple long-lived branches that each independently serve production traffic or releases (e.g., release/1.x and release/2.x both receiving deployments or tags).
- Patterns like release/* in workflow triggers or rulesets alone do NOT prove multiple production branches — they may be conventions for a single active release branch.
- Confidence: "high" = strong convergence of 3+ signals; "medium" = 2 signals or some ambiguity; "low" = sparse data or conflicting signals.
- If insufficient data, still provide your best guess with "low" confidence.`,
		},
		OnPermissionRequest: copilot.PermissionHandler.ApproveAll,
	})
	if err != nil {
		return fmt.Errorf("failed to create Copilot session: %w", err)
	}
	defer session.Disconnect()

	// Build the prompt with all repo signal summaries
	var promptParts []string
	promptParts = append(promptParts, "Analyze the following repositories and return a JSON array with your hypothesis for each.\n")
	for _, r := range results {
		promptParts = append(promptParts, "---")
		promptParts = append(promptParts, formatSignalSummary(r))
	}
	prompt := strings.Join(promptParts, "\n")

	// Collect the full response
	var responseBuf strings.Builder
	done := make(chan bool)

	session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageData:
			responseBuf.WriteString(d.Content)
		case *copilot.SessionIdleData:
			close(done)
		}
	})

	_, err = session.Send(ctx, copilot.MessageOptions{
		Prompt: prompt,
	})
	if err != nil {
		return fmt.Errorf("failed to send to Copilot: %w", err)
	}

	<-done

	// Parse the JSON response
	responseText := responseBuf.String()
	// Strip markdown code fences if present
	responseText = strings.TrimSpace(responseText)
	if strings.HasPrefix(responseText, "```") {
		lines := strings.Split(responseText, "\n")
		if len(lines) > 2 {
			lines = lines[1 : len(lines)-1]
		}
		responseText = strings.Join(lines, "\n")
	}

	var hypotheses []struct {
		Repo                       string   `json:"repo"`
		MultipleProductionBranches bool     `json:"multiple_production_branches"`
		CandidateBranches          []string `json:"candidate_branches"`
		Confidence                 string   `json:"confidence"`
		Reasoning                  string   `json:"reasoning"`
	}

	if err := json.Unmarshal([]byte(responseText), &hypotheses); err != nil {
		return fmt.Errorf("failed to parse Copilot response as JSON: %w\nResponse: %s", err, responseText[:min(len(responseText), 500)])
	}

	// Map hypotheses back to results
	hypothesisMap := map[string]*BranchHypothesis{}
	for _, h := range hypotheses {
		hypothesisMap[h.Repo] = &BranchHypothesis{
			MultipleProductionBranches: h.MultipleProductionBranches,
			CandidateBranches:          h.CandidateBranches,
			Confidence:                 h.Confidence,
			Reasoning:                  h.Reasoning,
		}
	}

	for i := range results {
		if h, ok := hypothesisMap[results[i].Name]; ok {
			results[i].AIHypothesis = h
		}
	}

	return nil
}

// ---------- Main ----------

func main() {
	if len(os.Args) < 2 {
		pterm.Error.Println("Usage: prod-branch-report <org-slug> [output.csv] [--analyze]")
		os.Exit(1)
	}

	org := os.Args[1]
	outputFile := ""
	analyze := false

	for _, arg := range os.Args[2:] {
		if arg == "--analyze" {
			analyze = true
		} else if outputFile == "" {
			outputFile = arg
		}
	}

	authToken = getToken()

	// Header
	pterm.DefaultHeader.WithBackgroundStyle(pterm.NewStyle(pterm.BgCyan)).
		WithTextStyle(pterm.NewStyle(pterm.FgBlack)).
		Println("Production Branch Report")
	fmt.Println()
	pterm.Info.Printfln("Organization: %s", pterm.Bold.Sprint(org))

	// Phase 1: Enumerate repos
	spinner, _ := pterm.DefaultSpinner.Start("Discovering repositories...")
	repos, err := listOrgRepos(org)
	if err != nil {
		spinner.Fail(fmt.Sprintf("Failed to list repos: %v", err))
		os.Exit(1)
	}
	spinner.Success(fmt.Sprintf("Found %d repositories", len(repos)))
	fmt.Println()

	// Phase 2: Analyze repos with progress bar
	var results []RepoResult

	progressBar, _ := pterm.DefaultProgressbar.
		WithTotal(len(repos)).
		WithTitle("Analyzing repositories").
		WithBarCharacter("█").
		WithLastCharacter("█").
		WithElapsedTimeRoundingFactor(time.Second).
		WithShowElapsedTime(true).
		WithShowCount(true).
		Start()

	for _, repo := range repos {
		parts := strings.SplitN(repo.NameWithOwner, "/", 2)
		owner, repoName := parts[0], parts[1]

		progressBar.UpdateTitle(fmt.Sprintf("Analyzing %s", repo.Name))

		r := RepoResult{
			Name:          repo.NameWithOwner,
			DefaultBranch: repo.DefaultBranchName,
		}

		// Get candidate branches for expensive per-branch queries
		candidates, _ := listCandidateBranches(owner, repoName, repo.DefaultBranchName)

		// Collect signals
		r.ProtectedBranches, _ = getProtectedBranches(owner, repoName)
		r.RulesetTargetBranches, _ = getRulesetBranches(owner, repoName)
		r.DeploymentBranches, _ = getDeploymentBranches(owner, repoName)
		r.ReleaseTargetBranches, _ = getReleaseBranches(owner, repoName)

		tagCounts, _ := getTagCountByBranch(owner, repoName)
		r.TaggedBranches = sortedKeysByValue(tagCounts)

		r.TopPRMergeTarget, _ = getTopPRMergeTarget(owner, repoName, candidates)
		r.WorkflowPushBranches, _ = getWorkflowPushBranches(owner, repoName)
		r.MostActiveCommitBranch, _ = getMostActiveBranch(owner, repoName, candidates)
		r.OldestBranch, _ = getOldestBranch(owner, repoName, candidates)

		results = append(results, r)
		progressBar.Increment()
	}

	fmt.Println()

	// Phase 3: AI hypothesis (optional)
	if analyze {
		pterm.Info.Println("Running AI analysis with GitHub Copilot SDK...")

		// Process in batches to stay within context limits
		batchSize := 20
		aiProgress, _ := pterm.DefaultProgressbar.
			WithTotal(len(results)).
			WithTitle("AI analysis").
			WithBarCharacter("█").
			WithLastCharacter("█").
			WithElapsedTimeRoundingFactor(time.Second).
			WithShowElapsedTime(true).
			WithShowCount(true).
			Start()

		for i := 0; i < len(results); i += batchSize {
			end := i + batchSize
			if end > len(results) {
				end = len(results)
			}
			batch := results[i:end]

			aiProgress.UpdateTitle(fmt.Sprintf("AI analyzing batch %d-%d", i+1, end))

			if err := analyzeWithCopilot(context.Background(), batch); err != nil {
				pterm.Warning.Printfln("AI analysis failed for batch %d-%d: %v", i+1, end, err)
			}

			// Copy hypotheses back into results
			for j, r := range batch {
				results[i+j].AIHypothesis = r.AIHypothesis
			}

			for k := 0; k < len(batch); k++ {
				aiProgress.Increment()
			}
		}
		fmt.Println()
	}

	// Phase 4: Write CSV output
	var writer *csv.Writer
	if outputFile != "" {
		f, err := os.Create(outputFile)
		if err != nil {
			pterm.Fatal.Printfln("Failed to create output file: %v", err)
		}
		defer f.Close()
		writer = csv.NewWriter(f)
	} else {
		writer = csv.NewWriter(os.Stdout)
	}
	defer writer.Flush()

	header := []string{
		"Repository",
		"Default Branch",
		"Protected Branches",
		"Ruleset Target Branches",
		"Deployment Branches (prod)",
		"Release Target Branches",
		"Tagged Branches (by count)",
		"Top PR Merge Target",
		"Workflow Push Branches",
		"Most Active Branch (6mo)",
		"Deepest Branch (total commits)",
	}
	if analyze {
		header = append(header, "AI: Multiple Prod Branches?", "AI: Candidate Branches", "AI: Confidence", "AI: Reasoning")
	}
	writer.Write(header)

	for _, r := range results {
		row := []string{
			r.Name,
			r.DefaultBranch,
			strings.Join(r.ProtectedBranches, "; "),
			strings.Join(r.RulesetTargetBranches, "; "),
			strings.Join(r.DeploymentBranches, "; "),
			strings.Join(r.ReleaseTargetBranches, "; "),
			strings.Join(r.TaggedBranches, "; "),
			r.TopPRMergeTarget,
			strings.Join(r.WorkflowPushBranches, "; "),
			r.MostActiveCommitBranch,
			r.OldestBranch,
		}
		if analyze {
			if r.AIHypothesis != nil {
				multi := "No"
				if r.AIHypothesis.MultipleProductionBranches {
					multi = "Yes"
				}
				row = append(row, multi, strings.Join(r.AIHypothesis.CandidateBranches, "; "), r.AIHypothesis.Confidence, r.AIHypothesis.Reasoning)
			} else {
				row = append(row, "", "", "", "")
			}
		}
		writer.Write(row)
	}

	// Summary
	fmt.Println()
	if outputFile != "" {
		pterm.Success.Printfln("Report written to %s (%d repos)", outputFile, len(results))
	} else {
		pterm.Success.Printfln("Report complete (%d repos)", len(results))
	}

	// Print signal coverage summary
	signalNames := []string{
		"Protected Branches", "Ruleset Targets", "Deployments (prod)",
		"Release Targets", "Tagged Branches", "PR Merge Target",
		"Workflow Push", "Commit Activity", "Branch Depth",
	}
	var coverageData pterm.TableData
	coverageData = append(coverageData, []string{"Signal", "Repos with data", "Coverage"})
	for i, name := range signalNames {
		count := 0
		for _, r := range results {
			switch i {
			case 0:
				if len(r.ProtectedBranches) > 0 { count++ }
			case 1:
				if len(r.RulesetTargetBranches) > 0 { count++ }
			case 2:
				if len(r.DeploymentBranches) > 0 { count++ }
			case 3:
				if len(r.ReleaseTargetBranches) > 0 { count++ }
			case 4:
				if len(r.TaggedBranches) > 0 { count++ }
			case 5:
				if r.TopPRMergeTarget != "" { count++ }
			case 6:
				if len(r.WorkflowPushBranches) > 0 { count++ }
			case 7:
				if r.MostActiveCommitBranch != "" { count++ }
			case 8:
				if r.OldestBranch != "" { count++ }
			}
		}
		pct := 0
		if len(results) > 0 {
			pct = count * 100 / len(results)
		}
		coverageData = append(coverageData, []string{name, fmt.Sprintf("%d", count), fmt.Sprintf("%d%%", pct)})
	}

	fmt.Println()
	pterm.DefaultTable.WithHasHeader().WithData(coverageData).Render()
}
