# Green Lane

Green Lane merges small, reviewed, green pull requests automatically, and nothing else.

It runs in GitHub Actions from code in this repository, pinned by commit SHA. It never checks out or runs PR code, and it reads each repository's policy from that repository's default branch, never from a PR. Comments, labels and bots can't talk it into a merge. Every rule is code.

- **Shadow mode** evaluates every open PR and writes a report. It never merges.
- **Live mode** does the same, then squash-merges **at most one** eligible PR per run, at its exact evaluated head SHA. It never retries a merge call.

## What makes a PR eligible

All of these, on the exact current head:

1. **Author** is in `allowed_authors`.
2. **Open, not a draft, mergeable, targets the default branch**, has no opt-out label, and its head is in the same repository.
3. **Contains the current default-branch tip** (zero commits behind). Otherwise it's skipped as `needs_main_refresh`. Green Lane never updates branches.
4. **Every required check passed**, produced by the pinned GitHub App and workflow ID. A same-named check from elsewhere doesn't count.
5. **An accepted automated reviewer finished a clean review of this head** (see [Reviewers](#reviewers)). Human approvals don't substitute.
6. **No unresolved feedback**:
   - every review thread is resolved, including outdated ones;
   - no outstanding "changes requested", including on older heads;
   - no review prose from anyone but the author. Prose has no "resolved" state, so Green Lane never guesses whether it's harmless; fix it in a new commit and get a fresh review. Claude's two exact "review not started" notices are the only exception.
7. **Small**: within the policy's file and line caps (docs-only PRs use the tighter `docs` caps).
8. **Only allowed paths, no denied paths.** If the policy sets `allowed_paths`, every changed path must match it. Denied paths always win, and both sides of a rename count. Matching ignores case.
   **No denied paths**, including either side of a rename. Some paths are always denied: CI workflows and `.github/scripts`/`.github/ci`, secrets and keys, release and signing, migrations and `*.sql`, `CODEOWNERS`, lockfiles, and `.github/green-lane.yml` itself.
9. **Stable evidence**: everything is read twice, then re-read just before merging. Any change, incomplete pagination or API error blocks the merge.

## Add Green Lane to a repository

1. **Write the policy.** Copy [`examples/green-lane.yml`](examples/green-lane.yml) to `.github/green-lane.yml` and fill it in:
   - **`required_checks`**: the exact check names. Get workflow IDs with `gh api repos/OWNER/REPO/actions/workflows --jq '.workflows[] | "\(.id) \(.path)"'`. The app ID for GitHub Actions is `15368`.
   - **`allowed_authors`**: real logins. Bot logins end in `[bot]`.
   - **`reviewers`**: the reviewers this repository actually runs.
   - **`allowed_paths`** (recommended): list the areas small PRs may touch, such as UI components and docs. Anything else, including files added later, stays manual. That's safer than trying to deny every sensitive file by name.
   - **`branch_protection`**: `github` if the default branch has strict required checks, otherwise `worker`. See [Without branch protection](#without-branch-protection).
2. **Add the caller workflow.** Copy [`examples/caller-workflow.yml`](examples/caller-workflow.yml) to `.github/workflows/green-lane.yml`.
   - Pin `uses:` to a full commit SHA of this repository.
   - Set `CI` in `workflow_run` to your CI workflow's name.
   - Keep `mode: shadow`.
3. **Merge both through a normal PR.** Green Lane never merges changes to itself: both files are always denied paths.
4. **Run shadow for a few days.** Run `gh workflow run green-lane.yml -R OWNER/REPO`, then read the `green-lane-shadow-*` artifact or the job summary. Compare its verdicts with what you would have merged by hand.
5. **Go live**:
   - in one reviewed PR, change `mode: shadow` to `mode: live` and raise `contents`, `pull-requests` and `actions` to `write` in the caller's `permissions` (shadow runs with read scopes only);
   - then run `gh variable set GREEN_LANE_LIVE_ENABLED -R OWNER/REPO --body true`.
   - Optional: set `post_merge_dispatch: ci.yml`. Merges made with the workflow token don't trigger push workflows, so this dispatches CI on the default branch.
6. **Optional writer audit.** Set `allowed_writers` in the policy and pass `access_audit_token`. Green Lane then refuses to merge if anyone else can push. Token setup:
   - create a fine-grained PAT owned by the repository owner;
   - only that repository, **Metadata: Read-only**;
   - store it as the `GREEN_LANE_ACCESS_AUDIT_TOKEN` secret.

### Kill switch

- **Stop immediately:** `gh workflow disable green-lane.yml -R OWNER/REPO`. The worker re-reads this state just before its merge call, so it also stops a run that's already evaluating. Also cancel any active run. A merge GitHub has already accepted can't be recalled.
- **Stop future runs:** `gh variable set GREEN_LANE_LIVE_ENABLED -R OWNER/REPO --body false`. The variable is read once at run start, so it doesn't stop a run already in progress.

## Reviewers

| `format` | Accepts | Blocks |
|---|---|---|
| `codex` | Codex summary (`chatgpt-codex-connector[bot]`, app 1144995). The code review is `completed` on the head with `has_unresolved_findings: false`, and any security review also completed cleanly on the head. | running, failed, stale head, unresolved findings, edits by anyone else |
| `coderabbit` | CodeRabbit summary (app 347564) whose coverage marker names the head as `reviewed` | in progress, rate limited, skipped, partial coverage |
| `claude-reviewed-head` | `github-actions[bot]` comment that starts with `marker`, has exactly one `**Verdict: no blocking issues**` line and exactly one `<!-- reviewed-head: SHA -->` naming the head. Its `check` must also have passed on the head. | "issues found", "blocking issues", any other verdict, two verdicts, stale head, verdict inside a code fence |
| `check` | a pinned check that passes only when a review completed cleanly on the exact head (e.g. toneclone's `claude-review`) | failure, skipped, other head, other workflow |

Only the newest summary per reviewer counts. Any workflow in a repository can post as `github-actions[bot]`. So `claude-reviewed-head` also requires its named check, which is pinned to one workflow ID, to have passed on the same head.

## Without branch protection

On repositories without usable branch protection (for example private repositories on GitHub Free), use `branch_protection: worker`. Green Lane then enforces everything itself before its single merge call:
- the head contains the current tip;
- every required check is green on the exact head;
- every conversation is resolved, checked through the API;
- the merge is made at the exact head SHA.

What's weaker than native protection:

- **Humans and other tools aren't gated.** Anyone with write access can still push to the default branch or merge directly. Green Lane only governs its own merges. For a single-owner organization that's acceptable; with more writers, set `allowed_writers`.
- **There's a small race window.** Between Green Lane's last check and its merge call, someone could push to the default branch. Native strict protection would then reject the merge; without it, the merge still lands on the new tip without re-running CI. The window is seconds, and the next run re-evaluates from scratch.
- **Required checks are advisory to GitHub.** Nothing stops a manual merge with red CI.

## Transient errors

GitHub 5xx errors and dropped connections are retried up to three times for reads. If they persist *before* the merge call, the run ends as `deferred_transient`: nothing is merged, and the next run starts over. From the merge call onward, every error fails loudly and is **never retried**, because GitHub may already have merged.

## Development

```sh
go test -race ./...
actionlint
```
