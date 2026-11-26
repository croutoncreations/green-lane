// Package greenlanetest holds a Redline-shaped policy for tests only.
package greenlanetest

import "github.com/croutoncreations/green-lane/internal/greenlane"

// PolicyYAML has eight pinned CI jobs and Codex/CodeRabbit reviewers.
const PolicyYAML = `version: 1
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

// Config parses PolicyYAML and panics on error.
func Config() greenlane.Config {
	cfg, _, err := greenlane.ParsePolicy([]byte(PolicyYAML))
	if err != nil {
		panic(err)
	}
	return cfg
}
