package greenlane

import (
	"time"
)

// Verdict represents the final evaluation result
type Verdict string

const (
	VerdictWouldMerge    Verdict = "would_merge"
	VerdictWouldNotMerge Verdict = "would_not_merge"
)

// RuleResult represents the outcome of evaluating a single rule
type RuleResult struct {
	Name         string `json:"name"`
	Pass         bool   `json:"pass"`
	Inconclusive bool   `json:"inconclusive,omitempty"`
	Message      string `json:"message"`
}

// EvaluationResult contains the full evaluation outcome
type EvaluationResult struct {
	CheckSnapshot     string                 `json:"check_snapshot"`
	CheckSources      map[string]CheckSource `json:"check_sources"`
	ReviewSnapshot    string                 `json:"review_snapshot"`
	Repository        string                 `json:"repository"`
	BaseBranch        string                 `json:"base_branch"`
	Verdict           Verdict                `json:"verdict"`
	Lane              Lane                   `json:"lane"`
	Rules             []RuleResult           `json:"rules"`
	Summary           string                 `json:"summary"`
	EvaluatedAt       time.Time              `json:"evaluated_at"`
	PRNumber          int                    `json:"pr_number"`
	HeadSHA           string                 `json:"head_sha"`
	BaseSHA           string                 `json:"base_sha"`
	UpdatedAt         string                 `json:"pr_updated_at"`
	AdvisoryCandidate bool                   `json:"advisory_candidate"`
	CandidateMessage  string                 `json:"candidate_message"`
}

// PRFile represents a file changed in the PR
type PRFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
	Patch     string // unified diff hunk text; GitHub omits it for large files
	// PreviousFilename is the source path of a rename (GitHub's
	// previous_filename). A rename touches both paths, so rules that look
	// at paths must check both.
	PreviousFilename string
}

// Paths returns every path the change touches: the file and, for a rename,
// its source.
func (f PRFile) Paths() []string {
	if f.PreviousFilename != "" && f.PreviousFilename != f.Filename {
		return []string{f.Filename, f.PreviousFilename}
	}
	return []string{f.Filename}
}

// CheckRun represents a GitHub check run status
type CheckRun struct {
	Name       string
	Status     string
	Conclusion string
	AppID      int64
	HeadSHA    string
	WorkflowID int64
}

// Review represents a GitHub PR review
type Review struct {
	Body        string
	ID          int64
	User        string
	UserID      int64
	UserType    string
	State       string
	CommitID    string
	SubmittedAt time.Time
}

// ReviewComment represents an inline review comment on the diff
type ReviewComment struct {
	ID       int64
	ReviewID int64
	Path     string
	Line     int
	Body     string
}

// PRInfo contains information about the pull request
type ReviewThread struct {
	ID       string `json:"id"`
	Resolved bool   `json:"isResolved"`
	Outdated bool   `json:"isOutdated"`
}

type PRInfo struct {
	AutomatedReviews      []AutomatedReviewComment
	CheckEvidence         []CheckEvidence
	Repository            string
	State                 string
	UpdatedAt             string
	SnapshotConsistent    bool
	SnapshotMessage       string
	ReviewThreadsComplete bool
	ReviewThreads         []ReviewThread
	Number                int
	Title                 string
	Body                  string
	HeadSHA               string
	BaseSHA               string
	BaseBranch            string
	Draft                 bool
	Mergeable             bool
	Author                string
	Files                 []PRFile
	CheckRuns             []CheckRun
	Reviews               []Review
	ReviewComments        []ReviewComment
}

// Config represents the green-lane configuration. cmd/green-lane-check loads
// an optional override with encoding/json, so every field carries a json tag
// matching its yaml tag; without it, snake_case keys such as "docs_deps" are
// silently ignored.
type CheckSource struct {
	AppID      int64 `json:"app_id"`
	WorkflowID int64 `json:"workflow_id"`
}

type Config struct {
	AllowedAuthors []string `json:"allowed_authors"`
	// Reviewers lists the accepted automated-review formats. Empty means none.
	Reviewers           []PolicyReviewer       `json:"reviewers"`
	AllowedBaseBranches []string               `json:"allowed_base_branches"`
	CheckSources        map[string]CheckSource `json:"check_sources"`
	DeniedPaths         []string               `yaml:"denied_paths" json:"denied_paths"`
	// AllowedPaths, when non-empty, limits the lane to PRs whose every path
	// matches one of these patterns. Denied paths still win.
	AllowedPaths       []string `json:"allowed_paths"`
	MaxChangedLines    int      `yaml:"max_changed_lines" json:"max_changed_lines"`
	MaxLockfileChanges int      `yaml:"max_lockfile_changes" json:"max_lockfile_changes"`
	MaxFiles           int      `yaml:"max_files" json:"max_files"`
	RequiredChecks     []string `yaml:"required_checks" json:"required_checks"`
	// DocsDeps holds the tighter caps for the docs/changelog/dependency lane.
	DocsDeps SizeLimits `yaml:"docs_deps" json:"docs_deps"`
}

// SizeLimits caps a lane's diff size.
type SizeLimits struct {
	MaxChangedLines    int `yaml:"max_changed_lines" json:"max_changed_lines"`
	MaxLockfileChanges int `yaml:"max_lockfile_changes" json:"max_lockfile_changes"`
	MaxFiles           int `yaml:"max_files" json:"max_files"`
}
