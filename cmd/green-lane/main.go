// Command green-lane evaluates pull requests against a repository's Green Lane
// policy and, in live mode, merges at most one eligible PR per run.
//
// It runs only trusted code (this repository, pinned by SHA by the caller) and
// never checks out or executes PR code. The policy is read through the API from
// the target repository's default-branch tip, never from a PR.
//
//	green-lane shadow   evaluate every open PR; write green-lane-report.json; never merge
//	green-lane live     shadow, then merge at most one eligible PR (requires opt-in)
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/croutoncreations/green-lane/internal/greenlane"
)

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "eval" && os.Getenv("GITHUB_ACTIONS") != "true" {
		os.Exit(localEval(os.Args[2:]))
	}
	if len(os.Args) != 2 || (os.Args[1] != "shadow" && os.Args[1] != "live") {
		fmt.Fprintln(os.Stderr, "usage: green-lane shadow|live")
		os.Exit(2)
	}
	report, err := run(context.Background(), os.Args[1], os.Getenv)
	if report != nil {
		writeReport(report)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}

// Report is the run artifact. It never contains tokens.
type Report struct {
	Mode            string                       `json:"mode"`
	Repository      string                       `json:"repository"`
	MainTip         string                       `json:"main_tip"`
	PolicySHA       string                       `json:"policy_blob_sha"`
	State           string                       `json:"state"`
	Reason          string                       `json:"reason"`
	TransientError  string                       `json:"transient_error,omitempty"`
	Evaluations     []greenlane.EvaluationResult `json:"evaluations"`
	Skipped         []Skipped                    `json:"skipped"`
	Errors          []string                     `json:"errors,omitempty"`
	Attempt         *Attempt                     `json:"attempt,omitempty"`
	MergedCommit    string                       `json:"merged_commit,omitempty"`
	WriterSnapshots [][]Writer                   `json:"writer_snapshots,omitempty"`
}

type Skipped struct {
	PR     int    `json:"pr"`
	Reason string `json:"reason"`
	Head   string `json:"head,omitempty"`
}

type Attempt struct {
	PR      int    `json:"pr"`
	Head    string `json:"head"`
	MainTip string `json:"main_tip"`
}

type Writer struct {
	Login      string `json:"login"`
	Permission string `json:"permission"`
}

func writeReport(r *Report) {
	data, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile("green-lane-report.json", data, 0o644)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			writeSummary(f, r)
			f.Close()
		}
	}
}

func writeSummary(f *os.File, r *Report) {
	fmt.Fprintf(f, "# Green Lane (%s)\n\n**%s**: %s\n\nMain tip `%s`\n\n", r.Mode, r.State, r.Reason, r.MainTip)
	fmt.Fprintf(f, "| PR | Verdict | Blocking rules |\n|---|---|---|\n")
	for _, e := range r.Evaluations {
		var failed []string
		for _, rule := range e.Rules {
			if !rule.Pass {
				failed = append(failed, rule.Name+": "+strings.ReplaceAll(rule.Message, "|", "/"))
			}
		}
		fmt.Fprintf(f, "| #%d | %s | %s |\n", e.PRNumber, e.Verdict, strings.Join(failed, "<br>"))
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(f, "| #%d | skipped | %s |\n", s.PR, s.Reason)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(f, "\n- error: %s\n", e)
	}
}

type env func(string) string

// apiBaseOverride is set only by tests.
var apiBaseOverride string

func run(ctx context.Context, mode string, getenv env) (*Report, error) {
	repoFull := getenv("GITHUB_REPOSITORY")
	owner, repo, ok := strings.Cut(repoFull, "/")
	token := getenv("GITHUB_TOKEN")
	if !ok || owner == "" || repo == "" || token == "" {
		return nil, errors.New("GITHUB_REPOSITORY and GITHUB_TOKEN are required")
	}
	report := &Report{Mode: mode, Repository: repoFull, State: "deferred", Evaluations: []greenlane.EvaluationResult{}, Skipped: []Skipped{}}
	base := "https://api.github.com"
	if apiBaseOverride != "" {
		base = apiBaseOverride
	}
	gh := &GitHubClient{token: token, owner: owner, repo: repo, apiBase: base, client: &http.Client{Timeout: 30 * time.Second}}

	err := evaluateAndMaybeMerge(ctx, mode, getenv, gh, report)
	var apiErr *APIError
	// A transient GitHub failure before any merge call defers the run. After
	// the merge call nothing is transient: GitHub may already have merged.
	if err != nil && report.State != "merge_requested" && errors.As(err, &apiErr) && apiErr.Transient {
		report.State = "deferred_transient"
		report.TransientError = err.Error()
		report.Reason = "Transient GitHub error before any merge call; nothing merged. The next run starts from scratch."
		fmt.Fprintf(os.Stderr, "::warning::Green Lane deferred: %v\n", err)
		return report, nil
	}
	return report, err
}

func evaluateAndMaybeMerge(ctx context.Context, mode string, getenv env, gh *GitHubClient, report *Report) error {
	repo, err := gh.repository(ctx)
	if err != nil {
		return err
	}
	tip, err := gh.branchTip(ctx, repo.DefaultBranch)
	if err != nil {
		return err
	}
	report.MainTip = tip
	if want := getenv("GITHUB_SHA"); mode == "live" && want != "" && want != tip {
		report.Reason = "Default branch moved since this run was triggered; the next run uses the new tip"
		return nil
	}
	cfg, policy, blob, err := gh.loadPolicy(ctx, tip)
	// Local dry runs only: try a draft policy before it lands on the default
	// branch. Ignored in GitHub Actions and in live mode.
	if local := getenv("GREEN_LANE_LOCAL_POLICY"); local != "" && mode == "shadow" && getenv("GITHUB_ACTIONS") != "true" {
		data, readErr := os.ReadFile(local)
		if readErr != nil {
			return readErr
		}
		cfg, policy, err = greenlane.ParsePolicy(data)
		blob = "local:" + local
	}
	if err != nil {
		return err
	}
	report.PolicySHA = blob
	if policy.BaseBranch != repo.DefaultBranch {
		return fmt.Errorf("policy base_branch %q is not the default branch %q", policy.BaseBranch, repo.DefaultBranch)
	}
	gh.config = cfg
	evaluator := greenlane.NewEvaluator(cfg)

	prs, err := gh.openPRs(ctx, policy.BaseBranch)
	if err != nil {
		return err
	}
	type candidate struct {
		pr     listedPR
		result greenlane.EvaluationResult
	}
	var eligible []candidate
	for _, listed := range prs {
		switch {
		case listed.Draft:
			report.Skipped = append(report.Skipped, Skipped{PR: listed.Number, Reason: "draft"})
			continue
		case hasLabel(listed.Labels, policy.OptOutLabels):
			report.Skipped = append(report.Skipped, Skipped{PR: listed.Number, Reason: "opt-out label"})
			continue
		case listed.HeadRepo != gh.owner+"/"+gh.repo:
			report.Skipped = append(report.Skipped, Skipped{PR: listed.Number, Reason: "head is in another repository"})
			continue
		}
		// The worker is the only gate when GitHub protection is unavailable,
		// and strict protection rejects stale heads anyway. Either way the head
		// must contain the exact current tip.
		current, err := gh.containsTip(ctx, tip, listed.HeadSHA)
		if err != nil {
			return err
		}
		if !current {
			report.Skipped = append(report.Skipped, Skipped{PR: listed.Number, Reason: "needs_main_refresh", Head: listed.HeadSHA})
			continue
		}
		result, err := gh.evaluate(ctx, evaluator, listed.Number, listed.HeadSHA)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				return fmt.Errorf("PR #%d: %w", listed.Number, err)
			}
			// Non-API evidence problems block only this PR.
			report.Errors = append(report.Errors, fmt.Sprintf("PR #%d: %v", listed.Number, err))
			continue
		}
		report.Evaluations = append(report.Evaluations, result)
		if result.Verdict == greenlane.VerdictWouldMerge && allRulesPass(result) {
			eligible = append(eligible, candidate{listed, result})
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].pr.Number < eligible[j].pr.Number })

	if mode == "shadow" {
		report.State = "shadow"
		report.Reason = fmt.Sprintf("%d of %d open PRs would merge; shadow mode never merges", len(eligible), len(prs))
		return nil
	}
	if getenv("GREEN_LANE_LIVE_ENABLED") != "true" {
		report.Reason = "GREEN_LANE_LIVE_ENABLED is not true; no merge attempted"
		return nil
	}
	if len(eligible) == 0 {
		report.Reason = "No eligible PR at the current tip; no merge attempted"
		return nil
	}
	return mergeOne(ctx, getenv, gh, policy, tip, eligible[0].pr.Number, eligible[0].result, report)
}

// mergeOne repeats every mutable check immediately before a single
// exact-head squash merge. It is never retried, and never falls through to a
// second PR, because a failed or timed-out call may still have merged.
func mergeOne(ctx context.Context, getenv env, gh *GitHubClient, policy greenlane.Policy, tip string, number int, result greenlane.EvaluationResult, report *Report) error {
	workflow := getenv("GREEN_LANE_WORKFLOW_FILE")
	if workflow == "" {
		return errors.New("GREEN_LANE_WORKFLOW_FILE is required in live mode (kill switch)")
	}
	if policy.BranchProtection == "github" {
		ok, err := gh.protectionMatches(ctx, policy)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("policy says branch_protection: github, but the default branch is not protected with every required check; no merge")
		}
	}
	if len(policy.AllowedWriters) > 0 {
		if err := auditWriters(ctx, getenv, gh, policy.AllowedWriters, report); err != nil {
			return err
		}
	}
	// Fresh, independent evaluation just before the merge call.
	fresh, err := gh.evaluate(ctx, greenlane.NewEvaluator(gh.config), number, result.HeadSHA)
	if err != nil {
		return err
	}
	if fresh.Verdict != greenlane.VerdictWouldMerge || !allRulesPass(fresh) || fresh.HeadSHA != result.HeadSHA ||
		fresh.ReviewSnapshot != result.ReviewSnapshot || fresh.CheckSnapshot != result.CheckSnapshot {
		report.Reason = fmt.Sprintf("PR #%d changed during evaluation; fresh evidence required", number)
		return nil
	}
	now, err := gh.branchTip(ctx, policy.BaseBranch)
	if err != nil {
		return err
	}
	if now != tip {
		report.Reason = "Default branch moved during evaluation; the next run re-evaluates"
		return nil
	}
	current, err := gh.containsTip(ctx, tip, result.HeadSHA)
	if err != nil {
		return err
	}
	if !current {
		report.Reason = fmt.Sprintf("PR #%d no longer contains the tip", number)
		return nil
	}
	active, err := gh.workflowActive(ctx, workflow)
	if err != nil {
		return err
	}
	if !active {
		report.Reason = "Kill switch: the Green Lane workflow was disabled during evaluation"
		return nil
	}
	if len(policy.AllowedWriters) > 0 {
		if err := auditWriters(ctx, getenv, gh, policy.AllowedWriters, report); err != nil {
			return err
		}
	}
	report.State = "merge_requested"
	report.Attempt = &Attempt{PR: number, Head: result.HeadSHA, MainTip: tip}
	writeReport(report)
	commit, err := gh.squashMerge(ctx, number, result.HeadSHA)
	if err != nil {
		return fmt.Errorf("merge call for PR #%d failed; it is NOT retried and may have succeeded: %w", number, err)
	}
	report.State = "merged"
	report.MergedCommit = commit
	report.Reason = fmt.Sprintf("Merged PR #%d at %s; one merge per run", number, commit)
	if ci := getenv("GREEN_LANE_POST_MERGE_DISPATCH"); ci != "" {
		// GITHUB_TOKEN merges do not trigger push workflows; dispatch explicitly.
		if err := gh.dispatch(ctx, ci, policy.BaseBranch); err != nil {
			return fmt.Errorf("merged PR #%d at %s, but post-merge dispatch of %s failed: %w", number, commit, ci, err)
		}
	}
	return nil
}

// auditWriters fails closed unless every collaborator with write, maintain or
// admin access is listed. It uses a separate read-only token because the
// workflow token cannot list collaborators.
func auditWriters(ctx context.Context, getenv env, gh *GitHubClient, allowed []string, report *Report) error {
	token := getenv("GREEN_LANE_ACCESS_AUDIT_TOKEN")
	if token == "" {
		return errors.New("policy sets allowed_writers, but GREEN_LANE_ACCESS_AUDIT_TOKEN is missing; no merge")
	}
	audit := *gh
	audit.token = token
	writers, err := audit.writers(ctx)
	if err != nil {
		return fmt.Errorf("writer audit failed; no merge: %w", err)
	}
	report.WriterSnapshots = append(report.WriterSnapshots, writers)
	ok := map[string]bool{}
	for _, login := range allowed {
		ok[strings.ToLower(login)] = true
	}
	for _, w := range writers {
		if (w.Permission == "admin" || w.Permission == "maintain" || w.Permission == "write") && !ok[strings.ToLower(w.Login)] {
			return fmt.Errorf("unexpected writer %s (%s); no merge", w.Login, w.Permission)
		}
	}
	return nil
}

func allRulesPass(r greenlane.EvaluationResult) bool {
	if len(r.Rules) == 0 {
		return false
	}
	for _, rule := range r.Rules {
		if !rule.Pass || rule.Inconclusive {
			return false
		}
	}
	return true
}

func hasLabel(labels []string, optOut []string) bool {
	for _, l := range labels {
		for _, o := range optOut {
			if strings.EqualFold(l, o) {
				return true
			}
		}
	}
	return false
}

// evaluate collects evidence twice and refuses an inconsistent snapshot.
func (c *GitHubClient) evaluate(ctx context.Context, e *greenlane.Evaluator, number int, expectedHead string) (greenlane.EvaluationResult, error) {
	first, err := c.GetPR(ctx, number)
	if err != nil {
		return greenlane.EvaluationResult{}, err
	}
	second, err := c.GetPR(ctx, number)
	if err != nil {
		return greenlane.EvaluationResult{}, err
	}
	if !sameEvidence(first, second) {
		second.SnapshotConsistent = false
		second.SnapshotMessage = "evidence changed between reads"
	}
	if second.HeadSHA != expectedHead {
		second.SnapshotConsistent = false
		second.SnapshotMessage = "head differs from the listed head"
	}
	return e.Evaluate(second), nil
}

type repoInfo struct {
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

func (c *GitHubClient) repository(ctx context.Context) (repoInfo, error) {
	var r repoInfo
	data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s", c.apiBase, c.owner, c.repo))
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	if err == nil && r.DefaultBranch == "" {
		err = errors.New("repository has no default branch")
	}
	return r, err
}

func (c *GitHubClient) branchTip(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", c.apiBase, c.owner, c.repo, branch))
	if err == nil {
		err = json.Unmarshal(data, &ref)
	}
	if err == nil && len(ref.Object.SHA) != 40 {
		err = errors.New("invalid branch tip")
	}
	return ref.Object.SHA, err
}

// loadPolicy reads the policy file at the exact tip commit, not from any PR.
func (c *GitHubClient) loadPolicy(ctx context.Context, tip string) (greenlane.Config, greenlane.Policy, string, error) {
	var file struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", c.apiBase, c.owner, c.repo, greenlane.PolicyPath, tip))
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return greenlane.Config{}, greenlane.Policy{}, "", fmt.Errorf("no %s on the default branch; Green Lane is not configured", greenlane.PolicyPath)
		}
		return greenlane.Config{}, greenlane.Policy{}, "", err
	}
	if err := json.Unmarshal(data, &file); err != nil || file.Encoding != "base64" {
		return greenlane.Config{}, greenlane.Policy{}, "", fmt.Errorf("unreadable %s", greenlane.PolicyPath)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return greenlane.Config{}, greenlane.Policy{}, "", err
	}
	cfg, policy, err := greenlane.ParsePolicy(raw)
	return cfg, policy, file.SHA, err
}

type listedPR struct {
	Number   int
	Draft    bool
	HeadSHA  string
	HeadRepo string
	Labels   []string
}

func (c *GitHubClient) openPRs(ctx context.Context, base string) ([]listedPR, error) {
	var all []listedPR
	for page := 1; page <= 30; page++ {
		data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&base=%s&sort=created&direction=asc&per_page=100&page=%d", c.apiBase, c.owner, c.repo, base, page))
		if err != nil {
			return nil, err
		}
		var rows []struct {
			Number int  `json:"number"`
			Draft  bool `json:"draft"`
			Head   struct {
				SHA  string `json:"sha"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.Number <= 0 {
				return nil, errors.New("invalid PR number in listing")
			}
			p := listedPR{Number: r.Number, Draft: r.Draft, HeadSHA: r.Head.SHA}
			if r.Head.Repo != nil {
				p.HeadRepo = r.Head.Repo.FullName
			}
			for _, l := range r.Labels {
				p.Labels = append(p.Labels, l.Name)
			}
			all = append(all, p)
		}
		if len(rows) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("open-PR pagination did not finish")
}

// containsTip is true only when head has the exact tip as an ancestor.
func (c *GitHubClient) containsTip(ctx context.Context, tip, head string) (bool, error) {
	var cmp struct {
		Status    string `json:"status"`
		BehindBy  int    `json:"behind_by"`
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", c.apiBase, c.owner, c.repo, tip, head))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, &cmp); err != nil {
		return false, err
	}
	return cmp.BehindBy == 0 && cmp.MergeBase.SHA == tip && (cmp.Status == "ahead" || cmp.Status == "identical"), nil
}

// protectionMatches reads the contents-readable branch summary: protected,
// enforced for everyone, and every required check bound to its pinned App.
func (c *GitHubClient) protectionMatches(ctx context.Context, policy greenlane.Policy) (bool, error) {
	var branch struct {
		Protected  bool `json:"protected"`
		Protection struct {
			Enabled bool `json:"enabled"`
			Checks  struct {
				Enforcement string `json:"enforcement_level"`
				Checks      []struct {
					Context string `json:"context"`
					AppID   int64  `json:"app_id"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/branches/%s", c.apiBase, c.owner, c.repo, policy.BaseBranch))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, &branch); err != nil {
		return false, err
	}
	if !branch.Protected || !branch.Protection.Enabled || branch.Protection.Checks.Enforcement != "everyone" {
		return false, nil
	}
	for _, want := range policy.RequiredChecks {
		found := false
		for _, got := range branch.Protection.Checks.Checks {
			if got.Context == want.Name && got.AppID == want.AppID {
				found = true
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

func (c *GitHubClient) workflowActive(ctx context.Context, file string) (bool, error) {
	var wf struct {
		State string `json:"state"`
	}
	data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s", c.apiBase, c.owner, c.repo, file))
	if err == nil {
		err = json.Unmarshal(data, &wf)
	}
	return wf.State == "active", err
}

func (c *GitHubClient) writers(ctx context.Context) ([]Writer, error) {
	var out []Writer
	for page := 1; page <= 10; page++ {
		data, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/collaborators?affiliation=all&per_page=100&page=%d", c.apiBase, c.owner, c.repo, page))
		if err != nil {
			return nil, err
		}
		var rows []struct {
			Login string `json:"login"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			raw, err := c.doRequest(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/collaborators/%s/permission", c.apiBase, c.owner, c.repo, r.Login))
			if err != nil {
				return nil, err
			}
			var perm struct {
				Permission string `json:"permission"`
				User       struct {
					Login string `json:"login"`
				} `json:"user"`
			}
			if err := json.Unmarshal(raw, &perm); err != nil {
				return nil, err
			}
			if perm.User.Login != r.Login || perm.Permission == "" {
				return nil, fmt.Errorf("unverifiable permission for %s", r.Login)
			}
			out = append(out, Writer{Login: r.Login, Permission: perm.Permission})
		}
		if len(rows) < 100 {
			if len(out) == 0 {
				return nil, errors.New("collaborator list is empty; the audit token cannot see collaborators")
			}
			return out, nil
		}
	}
	return nil, errors.New("collaborator pagination did not finish")
}

// squashMerge makes exactly one write call: a squash merge guarded by the
// evaluated head SHA. It is never retried.
func (c *GitHubClient) squashMerge(ctx context.Context, number int, head string) (string, error) {
	body, _ := json.Marshal(map[string]string{"sha": head, "merge_method": "squash"})
	data, err := c.requestOnce(ctx, "PUT", fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", c.apiBase, c.owner, c.repo, number), body)
	if err != nil {
		return "", err
	}
	var out struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	if err := json.Unmarshal(data, &out); err != nil || !out.Merged || len(out.SHA) != 40 {
		return "", errors.New("GitHub did not confirm the exact-head squash merge")
	}
	return out.SHA, nil
}

func (c *GitHubClient) dispatch(ctx context.Context, workflow, ref string) error {
	body, _ := json.Marshal(map[string]string{"ref": ref})
	_, err := c.requestOnce(ctx, "POST", fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/dispatches", c.apiBase, c.owner, c.repo, url.PathEscape(workflow)), body)
	return err
}

// localEval prints the verdict for specific PRs (open or merged) using
// GREEN_LANE_LOCAL_POLICY. Local, read-only; refused in GitHub Actions.
func localEval(args []string) int {
	owner, repo, _ := strings.Cut(os.Getenv("GITHUB_REPOSITORY"), "/")
	data, err := os.ReadFile(os.Getenv("GREEN_LANE_LOCAL_POLICY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg, _, err := greenlane.ParsePolicy(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	gh := &GitHubClient{token: os.Getenv("GITHUB_TOKEN"), owner: owner, repo: repo, apiBase: "https://api.github.com", client: &http.Client{Timeout: 30 * time.Second}, config: cfg}
	for _, a := range args {
		var n int
		fmt.Sscanf(a, "%d", &n)
		pr, err := gh.GetPR(context.Background(), n)
		if err != nil {
			fmt.Printf("#%d error: %v\n", n, err)
			continue
		}
		r := greenlane.NewEvaluator(cfg).Evaluate(pr)
		fmt.Printf("#%d %s\n", n, r.Verdict)
		for _, rule := range r.Rules {
			if !rule.Pass {
				fmt.Printf("    %s: %s\n", rule.Name, rule.Message)
			}
		}
	}
	return 0
}
