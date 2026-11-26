package greenlane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// AutomatedReviewComment is GitHub-owned issue-comment evidence, not PR text.
// Both the publishing App and immutable bot user ID must match.
type AutomatedReviewComment struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"user_id"`
	User         string `json:"user"`
	UserType     string `json:"user_type"`
	AppID        int64  `json:"app_id"`
	Body         string `json:"body"`
	UpdatedAt    string `json:"updated_at"`
	EditorID     int64  `json:"editor_id"`
	EditorUser   string `json:"editor_user"`
	EditorType   string `json:"editor_type"`
	LastEditedAt string `json:"last_edited_at"`
}

// GitHub Actions' bot identity. Any workflow in the repository can post as
// it, so its comments are only trusted together with a pinned review check.
const (
	actionsAppID  int64 = 15368
	actionsUserID int64 = 41898282
)

func TrustedAutomatedSummary(c AutomatedReviewComment) bool {
	return providerOf(c) != ""
}

// providerOf identifies a summary comment by its immutable App and bot IDs
// and its protocol header. Login names alone are never enough.
func providerOf(c AutomatedReviewComment) ReviewerFormat {
	if c.ID <= 0 || c.UserType != "Bot" {
		return ""
	}
	switch {
	case c.AppID == 347564 && c.UserID == 136622811 && c.User == "coderabbitai[bot]":
		if strings.HasPrefix(c.Body, "<!-- coderabbit-ai-generated:v1 -->") && strings.Contains(c.Body, "\n<!-- This is an auto-generated comment: summarize by coderabbit.ai -->") {
			return ReviewerCodeRabbit
		}
	case c.AppID == 1144995 && c.UserID == 199175422 && c.User == "chatgpt-codex-connector[bot]":
		if strings.HasPrefix(c.Body, "<!-- codex-pull-request-review-summary -->") {
			return ReviewerCodex
		}
	case c.AppID == actionsAppID && c.UserID == actionsUserID && c.User == "github-actions[bot]":
		return ReviewerBrigadeClaude
	}
	return ""
}

var fullHead = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Only standalone machine markers outside Markdown code/quotes are protocol.
// Multiple markers are ambiguous, so they fail closed.
func summaryPayload(body, prefix string) []byte {
	var payload []byte
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
			continue
		}
		if payload != nil {
			return nil
		}
		payload = []byte(strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->"))
	}
	return payload
}

// completedAutomatedReview reports whether any reviewer the repository
// accepts finished a clean review of the exact current head. Only the latest
// summary per provider counts.
func (e *Evaluator) completedAutomatedReview(pr PRInfo) bool {
	if !fullHead.MatchString(pr.HeadSHA) {
		return false
	}
	for _, r := range e.config.Reviewers {
		if r.Format == ReviewerCheck && e.checkPassedOnHead(pr, r.Check) {
			return true
		}
	}
	return e.completedSummary(pr)
}

// completedSummary considers only provider summary comments. Check-type
// reviewers are excluded, so clearing one provider's earlier review prose
// always needs a later summary from that same provider.
func (e *Evaluator) completedSummary(pr PRInfo) bool {
	accepted := e.config.Reviewers
	if !fullHead.MatchString(pr.HeadSHA) {
		return false
	}
	latest := map[string]AutomatedReviewComment{}
	invalid := map[string]bool{}
	for _, c := range pr.AutomatedReviews {
		key := summaryKey(c, accepted)
		if key == "" {
			continue
		}
		stamp, err := time.Parse(time.RFC3339, c.UpdatedAt)
		if err != nil || stamp.After(time.Now().UTC().Add(time.Minute)) {
			invalid[key] = true
			continue
		}
		old, ok := latest[key]
		oldStamp, _ := time.Parse(time.RFC3339, old.UpdatedAt)
		if !ok || stamp.After(oldStamp) || (stamp.Equal(oldStamp) && c.ID > old.ID) {
			latest[key] = c
		}
	}
	for key, c := range latest {
		if invalid[key] || strings.EqualFold(c.User, pr.Author) || !authenticSummaryEditor(c) {
			continue
		}
		switch providerOf(c) {
		case ReviewerCodeRabbit:
			if strings.Contains(c.Body, "<!-- This is an auto-generated comment: review in progress by coderabbit.ai -->") || strings.Contains(c.Body, "<!-- This is an auto-generated comment: rate limited by coderabbit.ai -->") || strings.Contains(c.Body, "<!-- This is an auto-generated comment: skip review by coderabbit.ai -->") {
				continue
			}
			var state struct {
				Source  string `json:"sourceCommitId"`
				Covered string `json:"coveredCommitId"`
				Kind    string `json:"kind"`
			}
			if summaryJSON(summaryPayload(c.Body, "<!-- final_review_risk_coverage:"), &state) == nil && state.Source == pr.HeadSHA && state.Covered == pr.HeadSHA && state.Kind == "reviewed" {
				return true
			}
		case ReviewerCodex:
			var state struct {
				Code     json.RawMessage `json:"code_review"`
				Security json.RawMessage `json:"security_review"`
			}
			if summaryJSON(summaryPayload(c.Body, "<!-- codex-review-state:v1 "), &state) == nil && completedCodexComponent(state.Code, pr.HeadSHA) &&
				(len(state.Security) == 0 || bytes.Equal(bytes.TrimSpace(state.Security), []byte("null")) || completedCodexComponent(state.Security, pr.HeadSHA)) {
				return true
			}
		case ReviewerBrigadeClaude:
			for _, r := range accepted {
				if r.Format == ReviewerBrigadeClaude && key == string(ReviewerBrigadeClaude)+r.Marker &&
					reviewedHeadClear(c.Body, r.Marker, pr.HeadSHA) && e.checkPassedOnHead(pr, r.Check) {
					return true
				}
			}
		}
	}
	return false
}

// summaryKey groups comments by reviewer so only the newest one per reviewer
// is considered. It returns "" for comments the repository does not accept.
func summaryKey(c AutomatedReviewComment, accepted []PolicyReviewer) string {
	provider := providerOf(c)
	for _, r := range accepted {
		if r.Format != provider {
			continue
		}
		if provider != ReviewerBrigadeClaude {
			return string(provider)
		}
		if strings.HasPrefix(c.Body, r.Marker+"\n") {
			return string(provider) + r.Marker
		}
	}
	return ""
}

var (
	reviewedHeadLine = regexp.MustCompile(`^<!-- reviewed-head: ([0-9a-f]{40}) -->$`)
	verdictLine      = regexp.MustCompile(`^\*\*Verdict: ([^*]+?)\.?\*\*`)
)

// reviewedHeadClear accepts a reviewed-head summary only when the comment
// starts with the marker, has exactly one reviewed-head line naming the
// current head and exactly one verdict line, and that verdict is "no blocking
// issues". "Issues found", "blocking issues" or any other verdict blocks.
// Lines inside code fences or block quotes are ignored as protocol.
func reviewedHeadClear(body, marker, head string) bool {
	if !strings.HasPrefix(body, marker+"\n") {
		return false
	}
	heads, verdicts := []string{}, []string{}
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := reviewedHeadLine.FindStringSubmatch(line); m != nil {
			heads = append(heads, m[1])
		}
		if m := verdictLine.FindStringSubmatch(line); m != nil {
			verdicts = append(verdicts, strings.ToLower(strings.TrimSpace(m[1])))
		}
	}
	return len(heads) == 1 && heads[0] == head && len(verdicts) == 1 && verdicts[0] == "no blocking issues"
}

// checkPassedOnHead reports whether the named check, from its pinned App and
// workflow, succeeded on the exact current head.
func (e *Evaluator) checkPassedOnHead(pr PRInfo, name string) bool {
	source, ok := e.config.CheckSources[name]
	if name == "" || !ok || source.AppID <= 0 || source.WorkflowID <= 0 {
		return false
	}
	for _, c := range pr.CheckRuns {
		if c.Name == name && c.HeadSHA == pr.HeadSHA && c.AppID == source.AppID && c.WorkflowID == source.WorkflowID &&
			c.Status == "completed" && c.Conclusion == "success" {
			return true
		}
	}
	return false
}

// Both review types use the same exact-head completion protocol. A populated
// security review must finish and clear findings even without inline threads.
func completedCodexComponent(raw json.RawMessage, head string) bool {
	var review struct {
		Commit     string `json:"commit_sha"`
		Status     string `json:"status"`
		Findings   *bool  `json:"has_any_findings"`
		Unresolved *bool  `json:"has_unresolved_findings"`
	}
	return summaryJSON(raw, &review) == nil && review.Commit == head && review.Status == "completed" && review.Findings != nil && review.Unresolved != nil && !*review.Unresolved
}

// Original author/App attribution survives edits by repository writers.
func authenticSummaryEditor(c AutomatedReviewComment) bool {
	if c.LastEditedAt == "" {
		return c.EditorID == 0 && c.EditorUser == "" && c.EditorType == ""
	}
	edited, err := time.Parse(time.RFC3339, c.LastEditedAt)
	updated, updateErr := time.Parse(time.RFC3339, c.UpdatedAt)
	return err == nil && updateErr == nil && !edited.After(updated) && c.EditorID == c.UserID && c.EditorType == "Bot" && c.EditorUser == strings.TrimSuffix(c.User, "[bot]")
}

// encoding/json accepts duplicate keys; completion protocols must be unambiguous.
func summaryJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delim == '{' {
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				// Provider protocol keys are ASCII. Reject Unicode aliases too:
				// encoding/json's field matching includes Unicode case folding.
				for _, r := range name {
					if r > 127 {
						return fmt.Errorf("non-ASCII summary key")
					}
				}
				name = strings.ToLower(name)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate summary key")
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		} else if delim == '[' {
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("unexpected summary delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	return strictJSON(data, target)
}

func strictJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}
