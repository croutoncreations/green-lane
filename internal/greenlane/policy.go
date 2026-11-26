package greenlane

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// PolicyPath is where a repository keeps its Green Lane policy. It is read
// from the default branch only, never from a PR, and is itself a denied path.
const PolicyPath = ".github/green-lane.yml"

// MandatoryDeniedPaths apply to every repository. A policy file can add to
// this list but cannot remove anything from it.
func MandatoryDeniedPaths() []string {
	return []string{
		// CI and automation
		".github/workflows/**",
		".github/actions/**",
		".github/scripts/**",
		".github/ci/**",
		".github/dependabot.yml",
		// Green Lane itself: policy and the tooling that scores PRs
		PolicyPath,
		".github/green-lane/**",
		// Ownership
		"CODEOWNERS",
		".github/CODEOWNERS",
		"docs/CODEOWNERS",
		// Secrets, credentials and keys
		"**/.env*",
		"**/*secret*",
		"**/*credential*",
		"**/*.pem",
		"**/*.key",
		"**/*.p12",
		"**/*.pfx",
		"**/*.keystore",
		"**/*.jks",
		"**/*.mobileprovision",
		// Release and signing
		".goreleaser.yaml",
		".goreleaser.yml",
		"**/*.entitlements",
		// Database migrations
		"**/migrations/**",
		"**/migration/**",
		"**/*.sql",
		// Lockfiles stay manual until trusted regeneration exists
		"**/go.sum",
		"**/package-lock.json",
		"**/pnpm-lock.yaml",
		"**/yarn.lock",
		"**/Package.resolved",
		"**/gradle.lockfile",
		"**/*.lock",
	}
}

// ReviewerFormat names a supported automated-review protocol.
type ReviewerFormat string

const (
	ReviewerCodex      ReviewerFormat = "codex"
	ReviewerCodeRabbit ReviewerFormat = "coderabbit"
	// ReviewerBrigadeClaude is a github-actions[bot] comment carrying a marker
	// such as "<!-- brigade-claude-review -->", a "**Verdict: …**" line and
	// "<!-- reviewed-head: <sha> -->". Any workflow can post as
	// github-actions[bot], so it also needs its pinned review check to have
	// passed on the same head.
	ReviewerBrigadeClaude ReviewerFormat = "claude-reviewed-head"
	// ReviewerCheck is satisfied by a pinned check run that passes only when
	// a review completed cleanly on the exact head (toneclone's claude-review).
	ReviewerCheck ReviewerFormat = "check"
)

// Policy is the per-repository file. Unknown keys are rejected so a typo
// cannot silently drop a rule.
type Policy struct {
	Version          int              `yaml:"version"`
	BaseBranch       string           `yaml:"base_branch"`
	AllowedAuthors   []string         `yaml:"allowed_authors"`
	RequiredChecks   []RequiredCheck  `yaml:"required_checks"`
	Size             PolicySize       `yaml:"size"`
	DeniedPaths      []string         `yaml:"denied_paths"`
	Reviewers        []PolicyReviewer `yaml:"reviewers"`
	OptOutLabels     []string         `yaml:"opt_out_labels"`
	BranchProtection string           `yaml:"branch_protection"`
	AllowedWriters   []string         `yaml:"allowed_writers"`
}

// RequiredCheck pins a check-run name to the GitHub App and workflow that
// must have produced it, so a same-named check from elsewhere never counts.
type RequiredCheck struct {
	Name       string `yaml:"name"`
	AppID      int64  `yaml:"app_id"`
	WorkflowID int64  `yaml:"workflow_id"`
}

type PolicySize struct {
	Code SizeLimits `yaml:"code"`
	Docs SizeLimits `yaml:"docs"`
}

type PolicyReviewer struct {
	Format ReviewerFormat `yaml:"format"`
	// Marker is the HTML comment that identifies the comment for
	// claude-reviewed-head. Defaults to "<!-- brigade-claude-review -->".
	Marker string `yaml:"marker,omitempty"`
	// Check names a required check (pinned by app and workflow) that must pass
	// on the exact head. Required for claude-reviewed-head and check.
	Check string `yaml:"check,omitempty"`
}

// ParsePolicy validates the policy and returns the evaluator config plus the
// opt-out labels. Anything missing or ambiguous is an error, never a default
// that loosens the lane.
func ParsePolicy(data []byte) (Config, Policy, error) {
	var p Policy
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&p); err != nil {
		return Config{}, p, fmt.Errorf("invalid %s: %w", PolicyPath, err)
	}
	if p.Version != 1 {
		return Config{}, p, fmt.Errorf("%s: version must be 1", PolicyPath)
	}
	if p.BaseBranch == "" {
		p.BaseBranch = "main"
	}
	if len(p.AllowedAuthors) == 0 {
		return Config{}, p, fmt.Errorf("%s: allowed_authors must list at least one login", PolicyPath)
	}
	if len(p.RequiredChecks) == 0 {
		return Config{}, p, fmt.Errorf("%s: required_checks must list at least one check", PolicyPath)
	}
	if len(p.Reviewers) == 0 {
		return Config{}, p, fmt.Errorf("%s: reviewers must list at least one accepted reviewer", PolicyPath)
	}
	switch p.BranchProtection {
	case "github", "worker":
	default:
		return Config{}, p, fmt.Errorf(`%s: branch_protection must be "github" (native strict protection) or "worker" (the worker is the only gate)`, PolicyPath)
	}
	for _, l := range []SizeLimits{p.Size.Code, p.Size.Docs} {
		if l.MaxFiles <= 0 || l.MaxChangedLines <= 0 || l.MaxLockfileChanges < 0 {
			return Config{}, p, fmt.Errorf("%s: size.code and size.docs need positive max_files and max_changed_lines", PolicyPath)
		}
	}
	cfg := Config{
		AllowedBaseBranches: []string{p.BaseBranch},
		AllowedAuthors:      p.AllowedAuthors,
		CheckSources:        map[string]CheckSource{},
		MaxChangedLines:     p.Size.Code.MaxChangedLines,
		MaxLockfileChanges:  p.Size.Code.MaxLockfileChanges,
		MaxFiles:            p.Size.Code.MaxFiles,
		DocsDeps:            p.Size.Docs,
		DeniedPaths:         append(MandatoryDeniedPaths(), p.DeniedPaths...),
	}
	for _, c := range p.RequiredChecks {
		if c.Name == "" || c.AppID <= 0 || c.WorkflowID <= 0 {
			return Config{}, p, fmt.Errorf("%s: each required check needs name, app_id and workflow_id", PolicyPath)
		}
		if _, dup := cfg.CheckSources[c.Name]; dup {
			return Config{}, p, fmt.Errorf("%s: duplicate required check %q", PolicyPath, c.Name)
		}
		cfg.CheckSources[c.Name] = CheckSource{AppID: c.AppID, WorkflowID: c.WorkflowID}
		cfg.RequiredChecks = append(cfg.RequiredChecks, c.Name)
	}
	for _, r := range p.Reviewers {
		switch r.Format {
		case ReviewerCodex, ReviewerCodeRabbit:
			if r.Marker != "" || r.Check != "" {
				return Config{}, p, fmt.Errorf("%s: %s takes no marker or check", PolicyPath, r.Format)
			}
		case ReviewerCheck:
			if r.Marker != "" {
				return Config{}, p, fmt.Errorf("%s: check reviewers take no marker", PolicyPath)
			}
		case ReviewerBrigadeClaude:
			if r.Marker == "" {
				r.Marker = "<!-- brigade-claude-review -->"
			}
			if !strings.HasPrefix(r.Marker, "<!-- ") || !strings.HasSuffix(r.Marker, " -->") || strings.Contains(r.Marker, "\n") {
				return Config{}, p, fmt.Errorf("%s: marker must be a single-line HTML comment", PolicyPath)
			}
		default:
			return Config{}, p, fmt.Errorf("%s: unknown reviewer format %q", PolicyPath, r.Format)
		}
		if r.Format == ReviewerCheck || r.Format == ReviewerBrigadeClaude {
			if _, ok := cfg.CheckSources[r.Check]; !ok {
				return Config{}, p, fmt.Errorf("%s: reviewer %s needs check: naming one of required_checks", PolicyPath, r.Format)
			}
		}
		cfg.Reviewers = append(cfg.Reviewers, r)
	}
	return cfg, p, nil
}
