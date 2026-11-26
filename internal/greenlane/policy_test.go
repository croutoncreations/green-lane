package greenlane

import (
	"os"
	"strings"
	"testing"
)

func TestParsePolicyRejectsLooseOrAmbiguousFiles(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"unknown key":   func(s string) string { return s + "auto_approve: true\n" },
		"typo in key":   func(s string) string { return strings.Replace(s, "denied_paths:", "denied_path:", 1) },
		"wrong version": func(s string) string { return strings.Replace(s, "version: 1", "version: 2", 1) },
		"no authors": func(s string) string {
			return strings.Replace(s, "allowed_authors: [jfox85]", "allowed_authors: []", 1)
		},
		"no protection mode": func(s string) string { return strings.Replace(s, "branch_protection: github\n", "", 1) },
		"bad protection mode": func(s string) string {
			return strings.Replace(s, "branch_protection: github", "branch_protection: none", 1)
		},
		"unpinned check": func(s string) string {
			return strings.Replace(s, "{name: go, app_id: 15368, workflow_id: 321038296}", "{name: go}", 1)
		},
		"duplicate check": func(s string) string { return strings.Replace(s, "name: go,", "name: actionlint,", 1) },
		"zero file cap":   func(s string) string { return strings.Replace(s, "code: {max_files: 6", "code: {max_files: 0", 1) },
		"no reviewers": func(s string) string {
			return strings.Replace(s, "  - format: codex\n  - format: coderabbit\n", "  []\n", 1)
		},
		"unknown reviewer": func(s string) string { return strings.Replace(s, "format: codex", "format: copilot", 1) },
		"codex with marker": func(s string) string {
			return strings.Replace(s, "format: codex", "{format: codex, marker: '<!-- x -->'}", 1)
		},
		"check reviewer no name": func(s string) string { return strings.Replace(s, "format: codex", "format: check", 1) },
		"check reviewer unpinned": func(s string) string {
			return strings.Replace(s, "format: codex", "{format: check, check: claude-review}", 1)
		},
		"reviewed-head without check": func(s string) string {
			return strings.Replace(s, "format: codex", "format: claude-reviewed-head", 1)
		},
		"multiline marker": func(s string) string {
			return strings.Replace(s, "format: codex", "{format: claude-reviewed-head, check: go, marker: \"<!-- a\\n -->\"}", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParsePolicy([]byte(edit(testPolicyYAML))); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestPolicyCannotRemoveMandatoryDeniedPaths(t *testing.T) {
	cfg, _, err := ParsePolicy([]byte(strings.Replace(testPolicyYAML, "denied_paths:\n", "denied_paths:\n  - docs/private/**\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEvaluator(cfg)
	for _, path := range append(MandatoryDeniedPaths()[:0:0], ".github/workflows/ci.yml", PolicyPath, "CODEOWNERS", "db/migrations/1.sql", "go.sum", ".env.production", "docs/private/plan.md") {
		if e.checkDeniedPaths(PRInfo{Files: []PRFile{{Filename: path}}}).Pass {
			t.Errorf("%s not denied", path)
		}
	}
}

func TestAcceptedReviewersAreEnforced(t *testing.T) {
	codexOnly, _, err := ParsePolicy([]byte(strings.Replace(testPolicyYAML, "  - format: coderabbit\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	p := passingPR() // carries only a clean CodeRabbit summary
	if NewEvaluator(codexOnly).completedAutomatedReview(p) {
		t.Fatal("CodeRabbit counted in a Codex-only repo")
	}
	if !testEvaluator().completedAutomatedReview(p) {
		t.Fatal("CodeRabbit rejected where accepted")
	}
}

const brigadeHead = "aaf1b6e1b61d5fa9e3d0411d12fca3c26586a742"

func brigadeComment(body string) AutomatedReviewComment {
	return AutomatedReviewComment{ID: 7188653012, UserID: actionsUserID, User: "github-actions[bot]", UserType: "Bot", AppID: actionsAppID,
		UpdatedAt: "2026-11-26T12:41:23Z", LastEditedAt: "2026-11-26T12:41:23Z", EditorID: actionsUserID, EditorUser: "github-actions", EditorType: "Bot", Body: body}
}

// Shape copied from croutoncreations/brigade#192.
const brigadeBody = "<!-- brigade-claude-review -->\n### Claude review (delta) of `aaf1b6e`\n\n**Verdict: no blocking issues.** This delta fixes F20.\n\n### Delta findings\nNone.\n\n<!-- reviewed-head: " + brigadeHead + " -->"

func brigadePolicy(t *testing.T) *Evaluator {
	t.Helper()
	cfg, _, err := ParsePolicy([]byte(`version: 1
branch_protection: worker
allowed_authors: [jfox85]
required_checks:
  - {name: test, app_id: 15368, workflow_id: 11}
  - {name: review, app_id: 15368, workflow_id: 22}
size:
  code: {max_files: 6, max_changed_lines: 150, max_lockfile_changes: 0}
  docs: {max_files: 5, max_changed_lines: 100, max_lockfile_changes: 0}
reviewers:
  - {format: claude-reviewed-head, check: review}
`))
	if err != nil {
		t.Fatal(err)
	}
	return NewEvaluator(cfg)
}

func brigadePR(body string) PRInfo {
	return PRInfo{HeadSHA: brigadeHead, Author: "jfox85", AutomatedReviews: []AutomatedReviewComment{brigadeComment(body)},
		CheckRuns: []CheckRun{{Name: "review", Status: "completed", Conclusion: "success", HeadSHA: brigadeHead, AppID: 15368, WorkflowID: 22}}}
}

func TestReviewedHeadFormat(t *testing.T) {
	e := brigadePolicy(t)
	if !e.completedAutomatedReview(brigadePR(brigadeBody)) {
		t.Fatal("clean Brigade review rejected")
	}
	if !e.completedAutomatedReview(brigadePR(strings.Replace(brigadeBody, "no blocking issues.**", "no blocking issues**", 1))) {
		t.Fatal("verdict without trailing period rejected")
	}
	for name, body := range map[string]string{
		"blocking issues":    strings.Replace(brigadeBody, "Verdict: no blocking issues.", "Verdict: blocking issues.", 1),
		"issues found":       strings.Replace(brigadeBody, "Verdict: no blocking issues.", "Verdict: issues found.", 1),
		"unknown verdict":    strings.Replace(brigadeBody, "Verdict: no blocking issues.", "Verdict: probably fine.", 1),
		"missing verdict":    strings.Replace(brigadeBody, "**Verdict: no blocking issues.**", "", 1),
		"two verdicts":       strings.Replace(brigadeBody, "### Delta", "**Verdict: issues found.**\n### Delta", 1),
		"stale head":         strings.Replace(brigadeBody, brigadeHead, strings.Repeat("b", 40), 1),
		"two heads":          brigadeBody + "\n<!-- reviewed-head: " + brigadeHead + " -->",
		"missing head":       strings.Replace(brigadeBody, "<!-- reviewed-head: "+brigadeHead+" -->", "", 1),
		"marker not first":   "note\n" + brigadeBody,
		"wrong marker":       strings.Replace(brigadeBody, "brigade-claude-review", "other-review", 1),
		"verdict in fence":   strings.Replace(brigadeBody, "**Verdict: no blocking issues.**", "```\n**Verdict: no blocking issues.**\n```", 1),
		"quoted head marker": strings.Replace(brigadeBody, "<!-- reviewed-head:", "> <!-- reviewed-head:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if e.completedAutomatedReview(brigadePR(body)) {
				t.Fatal("accepted")
			}
		})
	}
}

func TestReviewedHeadNeedsPinnedPassingCheck(t *testing.T) {
	e := brigadePolicy(t)
	for name, edit := range map[string]func(*PRInfo){
		"no check":          func(p *PRInfo) { p.CheckRuns = nil },
		"failed check":      func(p *PRInfo) { p.CheckRuns[0].Conclusion = "failure" },
		"check on old head": func(p *PRInfo) { p.CheckRuns[0].HeadSHA = strings.Repeat("c", 40) },
		"other workflow":    func(p *PRInfo) { p.CheckRuns[0].WorkflowID = 99 },
		"other app":         func(p *PRInfo) { p.CheckRuns[0].AppID = 1 },
		"human edited": func(p *PRInfo) {
			p.AutomatedReviews[0].EditorID = 5
			p.AutomatedReviews[0].EditorType = "User"
			p.AutomatedReviews[0].EditorUser = "someone"
		},
		"spoofed login": func(p *PRInfo) { p.AutomatedReviews[0].UserID = 123 },
		"newer dirty review": func(p *PRInfo) {
			c := brigadeComment(strings.Replace(brigadeBody, "no blocking issues.", "issues found.", 1))
			c.ID++
			c.UpdatedAt = "2026-11-26T13:00:00Z"
			c.LastEditedAt = ""
			c.EditorID = 0
			c.EditorUser = ""
			c.EditorType = ""
			p.AutomatedReviews = append(p.AutomatedReviews, c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := brigadePR(brigadeBody)
			edit(&p)
			if e.completedAutomatedReview(p) {
				t.Fatal("accepted")
			}
		})
	}
}

func TestReviewedHeadNotAcceptedUnlessConfigured(t *testing.T) {
	if testEvaluator().completedAutomatedReview(brigadePR(brigadeBody)) {
		t.Fatal("github-actions comment counted in a repo that does not accept it")
	}
}

func TestCheckReviewer(t *testing.T) {
	cfg, _, err := ParsePolicy([]byte(`version: 1
branch_protection: github
allowed_authors: [jfox85]
required_checks:
  - {name: All Tests Pass, app_id: 15368, workflow_id: 173621508}
  - {name: claude-review, app_id: 15368, workflow_id: 182144999}
size:
  code: {max_files: 6, max_changed_lines: 150, max_lockfile_changes: 0}
  docs: {max_files: 5, max_changed_lines: 100, max_lockfile_changes: 0}
reviewers:
  - {format: check, check: claude-review}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEvaluator(cfg)
	head := strings.Repeat("d", 40)
	run := CheckRun{Name: "claude-review", Status: "completed", Conclusion: "success", HeadSHA: head, AppID: 15368, WorkflowID: 182144999}
	if !e.completedAutomatedReview(PRInfo{HeadSHA: head, CheckRuns: []CheckRun{run}}) {
		t.Fatal("passing pinned review check rejected")
	}
	for name, edit := range map[string]func(*CheckRun){
		"failed":         func(c *CheckRun) { c.Conclusion = "failure" },
		"skipped":        func(c *CheckRun) { c.Conclusion = "skipped" },
		"in progress":    func(c *CheckRun) { c.Status = "in_progress" },
		"old head":       func(c *CheckRun) { c.HeadSHA = strings.Repeat("e", 40) },
		"wrong workflow": func(c *CheckRun) { c.WorkflowID = 1 },
	} {
		r := run
		edit(&r)
		if e.completedAutomatedReview(PRInfo{HeadSHA: head, CheckRuns: []CheckRun{r}}) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestExamplePolicyParses(t *testing.T) {
	data, err := os.ReadFile("../../examples/green-lane.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParsePolicy(data); err != nil {
		t.Fatal(err)
	}
}

func TestDeniedPathsIgnoreCase(t *testing.T) {
	cfg, _, err := ParsePolicy([]byte(strings.Replace(testPolicyYAML, "denied_paths:\n", "denied_paths:\n  - \"**/*session*\"\n  - \"**/*prompt*\"\n  - Infra/**\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEvaluator(cfg)
	for _, p := range []string{"frontend/src/hooks/useSessionLock.ts", "CLI_HUMANIZE_PROMPT.md", "infra/main.tf", "INFRA/x.tf", ".GitHub/Workflows/ci.yml", "Go.Sum"} {
		if e.checkDeniedPaths(PRInfo{Files: []PRFile{{Filename: p}}}).Pass {
			t.Errorf("%s allowed", p)
		}
	}
	if !e.checkDeniedPaths(PRInfo{Files: []PRFile{{Filename: "frontend/src/hooks/useWriting.ts"}}}).Pass {
		t.Error("unrelated path denied")
	}
}

func TestAllowedPathsLimitTheLane(t *testing.T) {
	cfg, _, err := ParsePolicy([]byte(strings.Replace(testPolicyYAML, "reviewers:", "allowed_paths:\n  - docs/**\n  - frontend/src/components/**\n  - \"**/*_test.go\"\nreviewers:", 1)))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEvaluator(cfg)
	for _, p := range []string{"docs/a.md", "frontend/src/components/Button.tsx", "Frontend/Src/Components/X.tsx", "backend/pkg/x/x_test.go"} {
		if r := e.checkDeniedPaths(PRInfo{Files: []PRFile{{Filename: p}}}); !r.Pass {
			t.Errorf("%s: %s", p, r.Message)
		}
	}
	for _, p := range []string{"backend/pkg/routes/persona_routes.go", "frontend/src/lib/checkout.ts", "README.md"} {
		if r := e.checkDeniedPaths(PRInfo{Files: []PRFile{{Filename: p}}}); r.Pass || !strings.Contains(r.Message, "Outside allowed_paths") {
			t.Errorf("%s allowed: %+v", p, r)
		}
	}
	// A deny pattern (here the mandatory **/.env*) wins over the allowlist.
	if e.checkDeniedPaths(PRInfo{Files: []PRFile{{Filename: "frontend/src/components/.env.local"}}}).Pass {
		t.Error("deny did not win")
	}
	// Both sides of a rename must be allowed.
	if e.checkDeniedPaths(PRInfo{Files: []PRFile{{Filename: "docs/moved.go", PreviousFilename: "backend/pkg/routes/x.go"}}}).Pass {
		t.Error("rename from outside the allowlist passed")
	}
}
