# Quickstart & Validation Guide — RedmineCodingAgent

**Feature**: `010-redmine-coding-agent` | **Plan**: [plan.md](./plan.md)

This is a run guide, not an implementation guide. It tells an operator or reviewer how to prove the feature works, end to end, on **both** Linux and Windows. Implementation details live in `tasks.md` and the code.

---

## 1. Prerequisites

| Requirement | Version | Checked by |
|---|---|---|
| Go toolchain (build only) | 1.24+ | `go version` |
| Node + npm (build only) | Node 22+, npm 10+ | `node --version` |
| `git` | 2.30+ | `redmine-agent doctor` |
| Redmine account + API key | REST API enabled, issues readable, comment permission when reporting is on | `doctor` |
| GitLab account + token | `api` scope on the mapped projects; reachable over the required VPN path | `doctor` |
| Coding agent CLI | `opencode` on `PATH`, already authenticated for its model provider | `doctor` |

Only the first three are needed to build. A real run needs the rest; the fake-service harness in §7 needs none of them.

---

## 2. Build

```bash
# frontend first (build-time only; output is embedded)
cd web && npm ci && npm run build && cd ..

# backend, statically linked, for the host OS
CGO_ENABLED=0 go build -trimpath -o bin/redmine-agent ./cmd/redmine-agent

# all four release targets (what CI enforces)
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/redmine-agent
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/redmine-agent
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o NUL        ./cmd/redmine-agent
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -o NUL        ./cmd/redmine-agent
```

PowerShell equivalent for the host build: `$env:CGO_ENABLED=0; go build -trimpath -o bin\redmine-agent.exe .\cmd\redmine-agent`.

**Expected**: four successful builds with no C toolchain installed. If a build demands `gcc`, a CGO-dependent package slipped in — that breaks the portability gate (G1) and must be fixed, not worked around.

---

## 3. Configure

Copy `config.example.yaml` to `config.yaml` and fill the mappings. Minimal shape:

```yaml
schema_version: 1

http:
  listen: "127.0.0.1:8080"

data:
  database: "./data/agent.db"
  workspace_root: "./data/workspaces"
  log_dir: "./data/logs"
  retention_days: 7

redmine:
  base_url: "https://redmine.internal"
  api_key: "env:REDMINE_API_KEY"
  comment_on_finish: true
  update_status: false          # FR-046: off by default

gitlab:
  base_url: "https://gitlab.internal"
  token: "env:GITLAB_TOKEN"
  push_enabled: true
  merge_request_enabled: true

repositories:
  - redmine_project: "billing"
    repository_path: "group/billing-api"
    base_branch: "main"
    is_default: true
    checks:
      - name: "unit-tests"
        command_linux: "go test ./..."
        command_windows: "go test ./..."
        required: true
        timeout_seconds: 900
      - name: "lint"
        command_linux: "golangci-lint run"
        command_windows: "golangci-lint run"
        required: false

agent:
  adapter: "opencode"
  model: "anthropic/claude-sonnet-4"
  profile: "restricted"          # the policy written into the sandbox
  timeout_minutes: 45
  max_repairs: 1

concurrency:
  max_active_runs: 1             # FR-011
```

Secrets are **never** literal values in this file: `env:NAME` reads an environment variable, `file:/path` reads a mounted secret. On Windows use `file:` with an ACL-restricted file rather than user-wide environment variables where possible.

**Expected**: `redmine-agent doctor --config config.yaml` exits `0`, printing one `ok` line per check. A deliberately wrong token must exit `4` and name the reference (`redmine.api_key`), never the value.

---

## 4. Scenario A — preview, confirm, and a real run (US1 + US2)

1. `redmine-agent serve --config config.yaml`, open `http://127.0.0.1:8080`.
2. Enter a reachable issue number; submit.
3. **Expect** within 10 s: subject, project, status, priority, description, extracted acceptance criteria (or the warning that none were found), target repository, and base branch. No branch, workspace, or commit exists yet — verify with `git -C <repo> branch` and by listing `data/workspaces` (empty).
4. Press Confirm.
5. **Expect**, in order, on the run view: `queued → preparing → implementing → validating`; stage lines for `connectivity.*`, `workspace.created`, `agent.invoked:1`, each check; elapsed time ticking; activity lines summarizing the agent's work without raw logs.
6. **Expect** on completion: a branch `agent/issue-<id>`, a commit whose message references the issue, the changed-file list, every configured check with command + exit code + duration + `passed|failed|not_executed`, and a merge request targeting the configured base branch whose description contains the issue reference, summary, notable files, validation results, limitations, and follow-up section.
7. **Expect** a comment on the Redmine issue containing the same links.

**Negative variants**

| Variant | Expected |
|---|---|
| Non-existent issue number | `404 issue_not_found`, no run row created, no workspace directory. |
| Blank / non-numeric input | Immediate validation error, no network call (`doctor` still healthy). |
| Project mapped to two repositories, no default | `409 repository_ambiguous` with both candidates listed; nothing created. |
| Project with no mapping | `409`/`blocked no_repository_mapping`; nothing guessed. |
| Click Decline on the preview | Back to input; no run, no workspace, no branch. |

## 5. Scenario B — independent verification (US2 + SC-005)

Use the fake agent's `lies` mode (§7) or, with a real agent, make a required check fail (point one check at `exit 1`).

**Expect**: the run ends `validation_failed`; `runs show <id>` lists the failing check with its real exit code; **no** branch is pushed (`pushedAt` is null); no merge request exists; the Redmine issue receives a report saying it was not validated. An agent that *claims* all checks passed changes nothing — its claim is stored as `reported_status`, never as a result.

## 6. Scenario C — cancellation, restart recovery, retry (US4 + US5)

**Cancel**

1. Start a run that takes minutes (a check with `sleep 600`, or the fake agent's `hang` mode).
2. Press Cancel.
3. **Expect** within 30 s (SC-009): no child processes remain (`ps`/`Get-Process` for the agent and its children), logs still readable, state `cancelled`, nothing pushed, the report stating whether uncommitted changes remain in the workspace.

**Restart recovery**

1. Start a run and hard-kill the worker (`kill -9` / `Stop-Process -Force`) mid-implementation.
2. Restart `serve`.
3. **Expect**: the previous run still appears in history, state `interrupted`, with `interrupted_stage` showing the last stage reached; **no** push, merge request, or comment is attempted; the workspace and logs are still on disk.

**Retry**

1. Retry the interrupted run.
2. **Expect**: a new run with `previousRunId` set, a distinct branch (`agent/issue-<id>-r2` when the earlier branch exists remotely), visible lineage in the history list, and no force-push over the earlier attempt.

**Double start**

1. While a run is active, submit the same issue again.
2. **Expect**: `409 active_run_exists` naming the blocking `runId` and `stage`; only one workspace directory and one agent process exist for that issue (SC-008).

## 7. Fake-service harness (no network, used by CI)

`tests/integration` provides everything needed to exercise the pipeline deterministically:

- `httptest` fakes for Redmine (`issue`, `journals`, `attachments`, `notes`) and GitLab (`project`, `branches`, `merge_requests`)
- a throwaway bare origin repository per test, so a real mirror + worktree is exercised
- a fake agent binary with selectable modes: `ok`, `no-changes`, `lies`, `blocked`, `crash`, `hang`, `escape-path`

Run them on either OS:

```bash
go test ./... -count=1
go test -race ./internal/... ./tests/integration/...   # Linux/CI leg
```

The fake agent's `escape-path` and `hang` modes are the automated proofs of R9 layer 3 and SC-009 respectively; they are the reason containment is testable at all.

## 8. Cross-platform parity checklist

Run this on Windows and on Linux before a release. Both must produce the same outcome; only the OS-specific command strings differ.

| Check | Linux | Windows |
|---|---|---|
| Binary builds with `CGO_ENABLED=0` | ✓ | ✓ |
| `doctor` passes against the same Redmine/GitLab | ✓ | ✓ |
| Scenario A completes (check commands differ only in shell wrapper: `sh -c` vs `cmd.exe /c`) | ✓ | ✓ |
| Cancel kills the agent's grandchildren within 30 s | process group | Job Object |
| SQLite file opened by a second process while running (expect `busy_timeout` behaviour, no corruption) | ✓ | ✓ |
| Paths with spaces and non-ASCII characters in `workspace_root` | ✓ | ✓ |

## 9. Success-criteria verification map

| SC | How to observe it |
|---|---|
| SC-001 | Scenario A step 3: preview rendered within 10 s; Decline returns cleanly |
| SC-002 | §4 negative variants: every bad input produces a specific error and zero side effects |
| SC-003 | Scenario B: failing required check ⇒ no push, no MR |
| SC-004 | After every scenario, confirm the base branch's remote tip is unchanged and no MR is merged |
| SC-005 | `runs checks <id>`: every configured check has command, exit code, and a classification; no `passed` row without exit 0 (also a DB `CHECK`) |
| SC-006 | Fake agent `no-changes` mode: run ends `no_changes`, zero commits created |
| SC-007 | Scenario C restart: history intact with stage and state |
| SC-008 | Scenario C double start: `409` with the blocking run id |
| SC-009 | Scenario C cancel: process tree gone within 30 s, nothing pushed |
| SC-010 | Scenario A step 6: inspect the MR description sections |
| SC-011 | `runs show <failed-id>`: failing stage, reason, and next action readable without opening logs |
| SC-012 | Add a mapping + check to `config.yaml`, restart, run again — the new check appears with no rebuild |
| SC-013 | `grep` the logs, transcripts, reports, and DB for the token values: zero matches |
| SC-014 | Scenario A end to end with no manual repository, branch, or command entry |

## 10. Operational notes and known limits

- **Loopback by default.** The listener binds `127.0.0.1`. Exposing it on a LAN address is a deliberate, logged choice; there is no per-user login in v1.
- **One process per database.** The recovery sweep marks non-terminal runs `interrupted`, so two workers sharing one SQLite file would interrupt each other's live runs. Run one `serve` per database. Horizontal scale needs a different store and is out of scope.
- **Agent containment is layered, not absolute.** Denies + sandboxed `HOME` + post-hoc detection cover a confused or prompt-injected agent. On Linux, prefer running the worker in a container; on Windows, use a dedicated service account whose filesystem ACLs do not reach production shares. See `research.md` §R9.
- **`opencode --format json` is not a published schema.** If a future release changes event shapes, the activity feed degrades; the transcript, exit code, and validation results still drive every decision.
- **Retention is manual in v1.** `redmine-agent prune --older-than` exists; nothing schedules it.
- **VPN is an environment concern.** The worker verifies GitLab reachability and fails clearly; it does not bring a tunnel up.
