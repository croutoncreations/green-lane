package greenlane

// DefaultConfig is a Redline-shaped test policy: eight pinned CI jobs and
// Codex/CodeRabbit as accepted reviewers. Production config always comes from
// ParsePolicy.
func DefaultConfig() Config {
	cfg, _, err := ParsePolicy([]byte(testPolicyYAML))
	if err != nil {
		panic(err)
	}
	return cfg
}

func testEvaluator() *Evaluator { return NewEvaluator(DefaultConfig()) }

const testPolicyYAML = `version: 1
base_branch: main
branch_protection: github
allowed_authors: [jfox85]
required_checks:
  - {name: actionlint, app_id: 15368, workflow_id: 321038296}
  - {name: go, app_id: 15368, workflow_id: 321038296}
  - {name: homebrew, app_id: 15368, workflow_id: 321038296}
  - {name: dashboard, app_id: 15368, workflow_id: 321038296}
  - {name: linux-smoke, app_id: 15368, workflow_id: 321038296}
  - {name: windows-smoke, app_id: 15368, workflow_id: 321038296}
  - {name: macos, app_id: 15368, workflow_id: 321038296}
  - {name: macos-intel, app_id: 15368, workflow_id: 321038296}
size:
  code: {max_files: 6, max_changed_lines: 150, max_lockfile_changes: 300}
  docs: {max_files: 5, max_changed_lines: 100, max_lockfile_changes: 150}
denied_paths:
  - internal/apiauth/**
  - internal/store/sqlite.go
  - playwright.config.*
  - .github/release-notes-*
  - .github/prompts/**
  - internal/nativeusage/credentials*
reviewers:
  - format: codex
  - format: coderabbit
opt_out_labels: [green-lane-hold]
`
