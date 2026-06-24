package main

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
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
	// Fall back to gh CLI token
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		log.Fatal("No GITHUB_TOKEN set and `gh auth token` failed. Provide a token.")
	}
	return strings.TrimSpace(string(out))
}

var httpClient = &http.Client{Timeout: 30 * time.Second}
var authToken string

func doGraphQL(query string, variables map[string]interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	req, _ := http.NewRequest("POST", graphqlURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var gqlResp graphQLResponse
	if err := json.Unmarshal(data, &gqlResp); err != nil {
		return nil, fmt.Errorf("graphql unmarshal: %w\nraw: %s", err, string(data))
	}
	if len(gqlResp.Errors) > 0 {
		return gqlResp.Data, fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
	}
	return gqlResp.Data, nil
}

func doREST(method, path string) ([]byte, http.Header, error) {
	url := restBaseURL + path
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.Header, fmt.Errorf("REST %s %s: %d %s", method, path, resp.StatusCode, string(body))
	}
	return body, resp.Header, nil
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

// ---------- Main ----------

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <org-slug> [output.csv]\n", os.Args[0])
		os.Exit(1)
	}

	org := os.Args[1]
	outputFile := ""
	if len(os.Args) >= 3 {
		outputFile = os.Args[2]
	}

	authToken = getToken()

	log.Printf("Enumerating repositories in org: %s", org)
	repos, err := listOrgRepos(org)
	if err != nil {
		log.Fatalf("Failed to list repos: %v", err)
	}
	log.Printf("Found %d repositories", len(repos))

	var results []RepoResult

	for i, repo := range repos {
		log.Printf("[%d/%d] Analyzing %s...", i+1, len(repos), repo.NameWithOwner)
		parts := strings.SplitN(repo.NameWithOwner, "/", 2)
		owner, repoName := parts[0], parts[1]

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

		// Small delay to avoid secondary rate limits
		time.Sleep(100 * time.Millisecond)
	}

	// Write CSV output
	var writer *csv.Writer
	if outputFile != "" {
		f, err := os.Create(outputFile)
		if err != nil {
			log.Fatalf("Failed to create output file: %v", err)
		}
		defer f.Close()
		writer = csv.NewWriter(f)
	} else {
		writer = csv.NewWriter(os.Stdout)
	}
	defer writer.Flush()

	// Header
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
		writer.Write(row)
	}

	if outputFile != "" {
		log.Printf("Report written to %s", outputFile)
	}
}
