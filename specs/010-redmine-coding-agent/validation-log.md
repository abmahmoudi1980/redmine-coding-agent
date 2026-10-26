# Validation log — release gate (T143)

**Date**: 2026-10-26
**Feature**: `010-redmine-coding-agent`
**Guide**: [quickstart.md](./quickstart.md)

This is the recorded run of the quickstart validation scenarios and the final
quality gate. Commands were run from the repository root unless stated
otherwise.

## Environment

| Item | Value |
|---|---|
| OS | Linux (x86_64) |
| Go | go1.27.3 linux/amd64 |
| Node / npm | v26.12.0 / 12.2.1 |
| git | available; used by the real mirror/worktree tests |
| Redmine / GitLab / agent | **fakes** (`tests/integration`); no live services on this host |

## Scenario A — preview, confirm, run (US1 + US2)

```bash
go test ./tests/integration -count=1 -run 'TestPreview|TestPipelineHappyPath|TestPublishPushesBranchAndOpensMergeRequest|TestPublishReusesAnExistingMergeRequest|TestPublishingDisabled|TestMergeRequestDisabled|TestReportingDisabled'
```

**Result: PASS**

| Observation | Evidence |
|---|---|
| Preview is side-effect free, invalid input never calls Redmine, ambiguity lists candidates | `TestPreviewCreatesNoRunWorkspaceOrBranch`, `TestPreviewRejectsInvalidIssueNumberBeforeCallingRedmine`, `TestPreviewAmbiguousRepositoryListsCandidatesAndHonoursChoice`, `TestPreviewReportsIssueNotFound` |
| A run commits only after the worker's own check passes | `TestPipelineHappyPathCommitsAfterChecksPass` |
| Push, MR description sections, comment links, base branch untouched, no merge call | `TestPublishPushesBranchAndOpensMergeRequest` |
| MR reuse instead of duplication | `TestPublishReusesAnExistingMergeRequest` |
| Publishing/MR policy produces `skipped`, not failure | `TestPublishingDisabledCompletesWithSkippedPublish`, `TestMergeRequestDisabledStillPushesTheBranch`, `TestReportingDisabledStoresTheReportAsSkipped` |

## Scenario B — independent verification (US2 + SC-005)

```bash
go test ./tests/integration -count=1 -run 'TestPipelineFailingRequiredCheck|TestPipelineNoChanges|TestPipelineBlocked|TestPipelineEscape|TestPipelineRequiredCheckThatNeverRuns|TestNoSecretValueSurvives'
```

**Result: PASS**

- A failing required check blocks the commit and ends `validation_failed`
  (`TestPipelineFailingRequiredCheckBlocksTheCommit`).
- An agent that claims success changes nothing; its claim stays a claim.
- Empty diff ends `no_changes` with zero commits
  (`TestPipelineNoChangesCreatesNoCommit`).
- A required check that never ran counts as failure (SC-005).
- Escaping the workspace ends `policy_violation`
  (`TestPipelineEscapeAttemptEndsPolicyViolation`).
- Zero fixture-secret matches across DB, logs, transcripts, prompts, reports,
  and HTTP responses (SC-013, `TestNoSecretValueSurvivesAFullPipeline`).

## Scenario C — cancel, restart recovery, retry, double start (US4 + US5)

```bash
go test ./tests/integration -count=1 -run 'TestCancelKills|TestRestartInterrupts|TestSecondStartForAnActiveIssue|TestCreateRunRejectsASecondActiveRun|TestCreateRunRequiresConfirmation|TestRetryOfAnActiveRun'
```

**Result: PASS**

- Cancellation kills the agent and its grandchildren and pushes nothing
  (`TestCancelKillsTheAgentTreeAndPublishesNothing`, 1.1 s — inside the 30 s
  SC-009 budget).
- Hard restart leaves the run `interrupted` with its stage; retry is a new
  linked attempt with a suffixed branch
  (`TestRestartInterruptsTheRunAndRetryIsANewLinkedAttempt`).
- A second start returns the blocking run and creates exactly one workspace
  (`TestSecondStartForAnActiveIssueIsRejectedAndCreatesNothing`,
  `TestCreateRunRejectsASecondActiveRun`).
- `cancel` and `retry` refuse active/terminal runs at the API boundary
  (`TestCancelOfATerminalRunIsRejected`, `TestRetryOfAnActiveRunIsRejected`).

## CLI contract (Polish)

```bash
go test ./tests/integration -count=1 -run 'TestCLI'
go test ./cmd/redmine-agent -count=1
```

**Result: PASS**

| Assertion | Evidence |
|---|---|
| Real binary; `--json` is exactly one document per line | `TestCLIVersionJSON`, `TestCLIRunExitCodesFollowTerminalState` |
| `0/5/6/7` map to completed / validation_failed / failed / blocked | `TestCLIRunExitCodesFollowTerminalState` |
| `8` and `9` map to cancelled and interrupted | `TestExitCodeForRunMirrorsTerminalState` |
| `--yes` (or a TTY `y`) is required before any run row or agent process | `TestCLIRunRequiresYesAndStartsNothing` |
| Retention parser accepts `30d`, `2w`, `12h`, bare days; rejects zero/negative | `TestParseRetention` |
| Flags after positionals (`runs show <id> --json`) parse | `TestSplitArgsAcceptsFlagsAfterPositionals` |

## Success-criteria map

| SC | Result | Evidence |
|---|---|---|
| SC-001 preview ≤ 3 s | PASS | p95 0.55 ms over 40 requests (`TestPreviewPerformanceBudget`, `tests/fixtures/perf-baseline.md`) |
| SC-002 bad input, zero side effects | PASS | preview negative variants |
| SC-003 failing check ⇒ no push/MR | PASS | `TestPipelineFailingRequiredCheckBlocksTheCommit` |
| SC-004 base tip unchanged, no merge | PASS | publish tests assert origin tip and `MergeCalls() == 0` |
| SC-005 no `passed` without exit 0 | PASS | DB `CHECK` + `TestPipelineRequiredCheckThatNeverRunsFails` |
| SC-006 `no_changes`, zero commits | PASS | `TestPipelineNoChangesCreatesNoCommit` |
| SC-007 restart keeps history and stage | PASS | `TestRestartInterruptsTheRunAndRetryIsANewLinkedAttempt` |
| SC-008 double start names the blocker | PASS | `TestSecondStartForAnActiveIssueIsRejectedAndCreatesNothing` |
| SC-009 cancel kills tree < 30 s | PASS | `TestCancelKillsTheAgentTreeAndPublishesNothing` (1.1 s) |
| SC-010 MR sections in order | PASS | `TestPublishPushesBranchAndOpensMergeRequest` |
| SC-011 failed run diagnosable without logs | PASS | run detail/document carries `failureCode`, `stage`, `remediation`; CLI `runs show` renders them |
| SC-012 config change, no rebuild | PASS | `TestConfigurationChangeAppliesOnRestartWithoutRebuilding` |
| SC-013 zero secret matches | PASS | `TestNoSecretValueSurvivesAFullPipeline` |
| SC-014 no manual branch/command entry | PASS | Scenario A end to end |

## Quality gate (T144)

| Gate | Command | Result |
|---|---|---|
| Vet | `go vet ./cmd/... ./internal/... ./tests/...` | PASS |
| Unit + integration | `go test ./cmd/... ./internal/... ./tests/... -count=1` | PASS, ~74 s |
| Race (Linux) | `go test -race ./cmd/... ./internal/... ./tests/... -count=1` | PASS, ~86 s |
| Frontend types | `npm --prefix web run check` | PASS, 0 errors / 0 warnings |
| Frontend unit | `npm --prefix web test` | PASS (no unit files; `--passWithNoTests`) |
| Browser suite | `npm --prefix web run e2e` | PASS, 9/9 Playwright tests |
| Cross-compile | Linux amd64/arm64 + Windows amd64/arm64, `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"` | PASS; 12–13 MB each |
| Windows static check | `GOOS=windows GOARCH=amd64 go vet ./cmd/... ./internal/... ./tests/...` | PASS |
| Lint | `golangci-lint run ./cmd/... ./internal/... ./tests/...` | PASS (0 issues) with golangci-lint 2.15.0 built by the local Go 1.27.3. The host's preinstalled 2.13.2 binary is unusable with this Go export data (`version 5 > 4`); install a matching linter before `make lint`. |

## Windows runtime leg — how to close it

This host is Linux, so the Windows *runtime* scenarios (Job Object cancellation,
`cmd.exe /c` checks, ACL-restricted service account) were not executed here.
They are covered by the `windows-latest` leg of `.github/workflows/ci.yml`,
which runs `go test ./...` and the four-target cross-compile. Before a release:

```powershell
$env:CGO_ENABLED=0
go test ./cmd/... ./internal/... ./tests/... -count=1
go test -race ./cmd/... ./internal/... ./tests/... -count=1   # where supported
.\bin\redmine-agent.exe doctor --config config.yaml --all
# then repeat Scenarios A–C from quickstart.md §4–§6
```

## Verdict

- Linux: **PASS** on every gate.
- Windows: compile and vet **PASS**; runtime leg delegated to CI and recorded as
  **pending** rather than claimed green.
