package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/croutoncreations/green-lane/internal/greenlane/greenlanetest"
)

const (
	tip  = "1111111111111111111111111111111111111111"
	head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var checkNames = []string{"actionlint", "go", "homebrew", "dashboard", "linux-smoke", "windows-smoke", "macos", "macos-intel"}

// fakeGitHub serves one repository with eligible PRs. Each knob makes one
// thing go wrong.
type fakeGitHub struct {
	mu            sync.Mutex
	prs           []int
	behind        map[int]bool
	merges        []string
	dispatches    int
	workflowState string
	tipAfterEval  string
	mergeStatus   int
	failPath      string
	failStatus    int
	writers       map[string]string
	protected     bool
	policy        string
	tipReads      int
	evaluated     map[int]int
}

func newFake() *fakeGitHub {
	return &fakeGitHub{prs: []int{1, 2}, behind: map[int]bool{}, workflowState: "active", protected: true,
		writers: map[string]string{"jfox85": "admin"}, policy: greenlanetest.PolicyYAML, evaluated: map[int]int{}}
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		send := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		p := r.URL.Path
		if f.failPath != "" && strings.Contains(p, f.failPath) {
			w.WriteHeader(f.failStatus)
			return
		}
		var n int
		switch {
		case p == "/repos/owner/repo":
			send(map[string]any{"default_branch": "main", "private": true})
		case p == "/repos/owner/repo/git/ref/heads/main":
			f.tipReads++
			sha := tip
			if f.tipAfterEval != "" && f.tipReads > 1 {
				sha = f.tipAfterEval
			}
			send(map[string]any{"object": map[string]any{"sha": sha}})
		case p == "/repos/owner/repo/contents/.github/green-lane.yml":
			if r.URL.Query().Get("ref") != tip {
				t.Errorf("policy read at %q, want the tip", r.URL.Query().Get("ref"))
			}
			send(map[string]any{"sha": "blob", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(f.policy))})
		case p == "/repos/owner/repo/pulls" && r.Method == "GET":
			var rows []any
			for _, n := range f.prs {
				sha := head
				if f.behind[n] {
					sha = strings.Repeat("b", 40)
				}
				rows = append(rows, map[string]any{"number": n, "draft": false, "head": map[string]any{"sha": sha, "repo": map[string]any{"full_name": "owner/repo"}}, "labels": []any{}})
			}
			send(rows)
		case strings.HasPrefix(p, "/repos/owner/repo/compare/"):
			behind := 0
			if strings.HasSuffix(p, strings.Repeat("b", 40)) {
				behind = 1
			}
			send(map[string]any{"status": "ahead", "behind_by": behind, "merge_base_commit": map[string]any{"sha": tip}})
		case p == "/repos/owner/repo/branches/main":
			checks := []any{}
			for _, c := range checkNames {
				checks = append(checks, map[string]any{"context": c, "app_id": 15368})
			}
			send(map[string]any{"protected": f.protected, "protection": map[string]any{"enabled": f.protected, "required_status_checks": map[string]any{"enforcement_level": "everyone", "checks": checks}}})
		case p == "/repos/owner/repo/actions/workflows/green-lane.yml":
			send(map[string]any{"state": f.workflowState})
		case p == "/repos/owner/repo/actions/workflows/ci.yml/dispatches":
			f.dispatches++
			w.WriteHeader(204)
		case p == "/repos/owner/repo/collaborators":
			var rows []any
			for login := range f.writers {
				rows = append(rows, map[string]any{"login": login})
			}
			send(rows)
		case strings.HasPrefix(p, "/repos/owner/repo/collaborators/") && strings.HasSuffix(p, "/permission"):
			login := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/owner/repo/collaborators/"), "/permission")
			send(map[string]any{"permission": f.writers[login], "user": map[string]any{"login": login}})
		case sscan(p, "/repos/owner/repo/pulls/%d/merge", &n):
			body, _ := io.ReadAll(r.Body)
			f.merges = append(f.merges, fmt.Sprintf("%d:%s", n, body))
			if f.mergeStatus != 0 {
				w.WriteHeader(f.mergeStatus)
				return
			}
			send(map[string]any{"merged": true, "sha": strings.Repeat("f", 40)})
		case sscan(p, "/repos/owner/repo/pulls/%d/files", &n):
			send([]any{map[string]any{"filename": "internal/cli/cli.go", "status": "modified", "additions": 3}})
		case sscan(p, "/repos/owner/repo/pulls/%d/reviews", &n), sscan(p, "/repos/owner/repo/pulls/%d/comments", &n):
			send([]any{})
		case sscan(p, "/repos/owner/repo/issues/%d/comments", &n):
			send([]any{map[string]any{"id": 500 + n, "node_id": fmt.Sprintf("IC_%d", n), "body": codexBody, "updated_at": "2026-10-21T00:00:00Z",
				"user": map[string]any{"id": 199175422, "login": "chatgpt-codex-connector[bot]", "type": "Bot"}, "performed_via_github_app": map[string]any{"id": 1144995}}})
		case sscan(p, "/repos/owner/repo/pulls/%d", &n):
			f.evaluated[n]++
			send(map[string]any{"number": n, "title": "t", "state": "open", "updated_at": "2026-10-21T00:00:00Z", "mergeable": true, "changed_files": 1,
				"head": map[string]any{"sha": head}, "base": map[string]any{"sha": tip, "ref": "main"}, "user": map[string]any{"login": "jfox85"}})
		case strings.HasSuffix(p, "/check-runs"):
			var runs []any
			for i, c := range checkNames {
				id := 100 + i
				runs = append(runs, map[string]any{"id": id, "name": c, "status": "completed", "conclusion": "success", "head_sha": head,
					"details_url": fmt.Sprintf("https://github.com/owner/repo/actions/runs/99/job/%d", id), "app": map[string]any{"id": 15368}})
			}
			send(map[string]any{"total_count": len(runs), "check_runs": runs})
		case sscan(p, "/repos/owner/repo/actions/jobs/%d", &n):
			send(map[string]any{"run_id": 99, "check_run_url": fmt.Sprintf("http://%s/repos/owner/repo/check-runs/%d", r.Host, n), "head_sha": head,
				"name": checkNames[n-100], "status": "completed", "conclusion": "success"})
		case p == "/repos/owner/repo/actions/runs/99":
			send(map[string]any{"workflow_id": 321038296, "head_sha": head})
		case p == "/graphql":
			var req struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if strings.Contains(req.Query, "nodes(ids:") {
				ids := req.Variables["ids"].([]any)
				var nodes []any
				for _, id := range ids {
					var num int
					fmt.Sscanf(id.(string), "IC_%d", &num)
					nodes = append(nodes, map[string]any{"id": id, "databaseId": 500 + num, "body": codexBody, "updatedAt": "2026-10-21T00:00:00Z"})
				}
				send(map[string]any{"data": map[string]any{"nodes": nodes}})
				return
			}
			send(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"headRefOid": head, "baseRefOid": tip,
				"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
}

// sscan matches path against format (with one %d) exactly, so
// "/pulls/%d" does not match "/pulls/1/merge" and vice versa.
func sscan(path, format string, n *int) bool {
	if _, err := fmt.Sscanf(path, format, n); err != nil {
		return false
	}
	return fmt.Sprintf(format, *n) == path
}

func runFake(t *testing.T, f *fakeGitHub, mode string, vars map[string]string) (*Report, error) {
	t.Helper()
	server := f.serve(t)
	t.Cleanup(server.Close)
	base := map[string]string{"GITHUB_REPOSITORY": "owner/repo", "GITHUB_TOKEN": "t", "GITHUB_SHA": tip,
		"GREEN_LANE_LIVE_ENABLED": "true", "GREEN_LANE_WORKFLOW_FILE": "green-lane.yml", "GREEN_LANE_ACCESS_AUDIT_TOKEN": "audit"}
	for k, v := range vars {
		base[k] = v
	}
	apiBaseOverride = server.URL
	retryBackoff = nil
	t.Cleanup(func() { apiBaseOverride = "" })
	t.Chdir(t.TempDir())
	return run(context.Background(), mode, func(k string) string { return base[k] })
}

func withWriters(policy string) string {
	return policy + "allowed_writers: [jfox85]\n"
}

func TestLiveMergesExactlyOneExactHeadPR(t *testing.T) {
	f := newFake()
	r, err := runFake(t, f, "live", map[string]string{"GREEN_LANE_POST_MERGE_DISPATCH": "ci.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "merged" || len(f.merges) != 1 || !strings.HasPrefix(f.merges[0], "1:") || !strings.Contains(f.merges[0], `"sha":"`+head+`"`) || !strings.Contains(f.merges[0], `"squash"`) {
		t.Fatalf("state=%s merges=%v", r.State, f.merges)
	}
	if f.dispatches != 1 {
		t.Fatal("post-merge CI not dispatched")
	}
}

func TestBehindPRIsSkippedAndNextEligibleMerges(t *testing.T) {
	f := newFake()
	f.behind[1] = true
	r, err := runFake(t, f, "live", nil)
	if err != nil || len(f.merges) != 1 || !strings.HasPrefix(f.merges[0], "2:") || f.evaluated[1] != 0 {
		t.Fatalf("err=%v merges=%v evaluated=%v", err, f.merges, f.evaluated)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Reason != "needs_main_refresh" {
		t.Fatalf("skipped=%+v", r.Skipped)
	}
}

func TestShadowNeverMerges(t *testing.T) {
	f := newFake()
	r, err := runFake(t, f, "shadow", nil)
	if err != nil || len(f.merges) != 0 || r.State != "shadow" || len(r.Evaluations) != 2 {
		t.Fatalf("err=%v merges=%v report=%+v", err, f.merges, r)
	}
}

func TestLiveNoMerge(t *testing.T) {
	for name, edit := range map[string]func(*fakeGitHub, map[string]string){
		"opt-in missing":        func(f *fakeGitHub, v map[string]string) { v["GREEN_LANE_LIVE_ENABLED"] = "" },
		"opt-in false":          func(f *fakeGitHub, v map[string]string) { v["GREEN_LANE_LIVE_ENABLED"] = "false" },
		"kill switch":           func(f *fakeGitHub, v map[string]string) { f.workflowState = "disabled_manually" },
		"tip moved before run":  func(f *fakeGitHub, v map[string]string) { v["GITHUB_SHA"] = strings.Repeat("2", 40) },
		"tip moved during eval": func(f *fakeGitHub, v map[string]string) { f.tipAfterEval = strings.Repeat("3", 40) },
		"all behind":            func(f *fakeGitHub, v map[string]string) { f.behind[1] = true; f.behind[2] = true },
		"no open PRs":           func(f *fakeGitHub, v map[string]string) { f.prs = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			vars := map[string]string{}
			edit(f, vars)
			r, err := runFake(t, f, "live", vars)
			if err != nil || len(f.merges) != 0 || r.State == "merged" {
				t.Fatalf("err=%v merges=%v state=%s", err, f.merges, r.State)
			}
		})
	}
}

func TestLiveFailsClosed(t *testing.T) {
	for name, edit := range map[string]func(*fakeGitHub, map[string]string){
		"unprotected branch": func(f *fakeGitHub, v map[string]string) { f.protected = false },
		"extra writer": func(f *fakeGitHub, v map[string]string) {
			f.policy = withWriters(f.policy)
			f.writers["intruder"] = "write"
		},
		"audit token missing": func(f *fakeGitHub, v map[string]string) {
			f.policy = withWriters(f.policy)
			v["GREEN_LANE_ACCESS_AUDIT_TOKEN"] = ""
		},
		"audit 401": func(f *fakeGitHub, v map[string]string) {
			f.policy = withWriters(f.policy)
			f.failPath = "/collaborators"
			f.failStatus = 401
		},
		"policy missing":     func(f *fakeGitHub, v map[string]string) { f.failPath = "/contents/"; f.failStatus = 404 },
		"policy invalid":     func(f *fakeGitHub, v map[string]string) { f.policy = "version: 1\nauto_merge: everything\n" },
		"no kill switch":     func(f *fakeGitHub, v map[string]string) { v["GREEN_LANE_WORKFLOW_FILE"] = "" },
		"merge rejected":     func(f *fakeGitHub, v map[string]string) { f.mergeStatus = 409 },
		"merge 502 is fatal": func(f *fakeGitHub, v map[string]string) { f.mergeStatus = 502 },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			vars := map[string]string{}
			edit(f, vars)
			_, err := runFake(t, f, "live", vars)
			if err == nil {
				t.Fatal("expected failure")
			}
			if len(f.merges) > 1 {
				t.Fatalf("merge retried or advanced to another PR: %v", f.merges)
			}
		})
	}
}

func TestMergeCallIsNeverRetriedOrAdvanced(t *testing.T) {
	f := newFake()
	f.mergeStatus = 504
	r, err := runFake(t, f, "live", nil)
	if err == nil || len(f.merges) != 1 || r.State != "merge_requested" || !strings.Contains(err.Error(), "NOT retried") {
		t.Fatalf("err=%v merges=%v state=%s", err, f.merges, r.State)
	}
}

func TestTransientErrorBeforeMergeDefers(t *testing.T) {
	for _, path := range []string{"/compare/", "/check-runs", "/graphql", "/pulls/1/files", "/branches/main"} {
		t.Run(path, func(t *testing.T) {
			f := newFake()
			f.failPath, f.failStatus = path, 503
			r, err := runFake(t, f, "live", nil)
			if err != nil || r.State != "deferred_transient" || len(f.merges) != 0 || r.TransientError == "" {
				t.Fatalf("err=%v state=%s merges=%v", err, r.State, f.merges)
			}
		})
	}
}

func TestWriterAuditPassesForAllowedWriters(t *testing.T) {
	f := newFake()
	f.policy = withWriters(f.policy)
	f.writers["reader"] = "read"
	r, err := runFake(t, f, "live", nil)
	if err != nil || r.State != "merged" || len(r.WriterSnapshots) != 2 {
		t.Fatalf("err=%v state=%s snapshots=%d", err, r.State, len(r.WriterSnapshots))
	}
}
