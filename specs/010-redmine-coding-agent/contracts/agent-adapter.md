# Contract — Coding Agent Adapter (FR-028)

**Feature**: `010-redmine-coding-agent` | **Scope**: internal Go boundary between orchestration and any coding agent

This boundary exists so that "use a different agent" is a configuration change (`agent.adapter`, `agent.command`) and nothing else. Orchestration, validation, publishing, and reporting must not know which agent ran — they know only what this contract returns.

---

## 1. Interface

```go
// Package agent isolates coding-agent implementations from the orchestrator.
package agent

// Adapter performs exactly one non-interactive invocation in one workspace.
type Adapter interface {
    // Name is the stable identifier used in configuration and stored on the run (e.g. "opencode").
    Name() string

    // Probe reports whether the adapter's executable is present and usable.
    // It must not require network access and must not mutate the workspace.
    Probe(ctx context.Context, cfg Config) (ProbeResult, error)

    // Run executes one invocation to completion, or until ctx is cancelled or
    // the deadline expires. It must always return a Result describing what
    // happened, even when it also returns an error.
    Run(ctx context.Context, req Request) (Result, error)
}
```

`Result` is the only channel through which agent claims reach the rest of the system, and every claim in it is **untrusted**.

## 2. Request

```go
type Request struct {
    RunID        string        // UUIDv7, for log correlation and transcript naming
    Attempt      int           // 1 = implementation, >=2 = repair (FR-036)
    IssueID      int
    WorkspaceDir string        // MUST be the process working directory
    SandboxDir   string        // run-scoped HOME/XDG/TMP; adapter must redirect the child here
    Prompt       Prompt        // already built and fenced by agent/prompt.go

    Model        string        // "provider/model", from config
    AgentName    string        // agent-internal profile name, from config
    Timeout      time.Duration // hard cap (FR-026)
    EnvAllowlist []string      // the ONLY inherited environment variables
    // Path to the worker-generated agent policy file (permission denies, R9).
    PolicyFile   string
}

type Prompt struct {
    Text       string // full text sent to the agent
    SHA256     string // recorded before the call (FR-027)
    Transcript string // absolute path inside the run directory
}
```

## 3. Result

```go
type Result struct {
    Status        Status // Succeeded | Failed | TimedOut | Cancelled | Blocked
    ExitCode      int
    TimedOut      bool
    StartedAt     time.Time
    EndedAt       time.Time
    Transcript    string // path to raw stdout/stderr capture; durable reference (FR-027)

    // ReportedStatus is what the agent *claims*. It may set Blocked (FR-030)
    // and may populate Summary; it MUST NEVER be used to decide that a check passed.
    ReportedStatus string
    Summary        Summary // parsed "## RESULT" block, zero value when absent
    Activity       []ActivityEvent // best-effort parsed progress for the UI

    // PolicyViolations are concrete, observed escape attempts (R9 layer 3).
    PolicyViolations []string
}
```

## 4. Mandatory behaviour

| # | Requirement | Why |
|---|---|---|
| A1 | The child process runs with `cwd = WorkspaceDir`; the adapter must fail fast if `WorkspaceDir` is not an existing directory inside the configured workspace root. | FR-022 |
| A2 | `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, `TMPDIR`/`TEMP`/`TMP` point inside `SandboxDir`. | FR-029 (R9 L1) |
| A3 | The environment is exactly `EnvAllowlist` plus the redirects above. No inherited `GITLAB_TOKEN`, `REDMINE_API_KEY`, or arbitrary operator variables. | FR-056 |
| A4 | The process is started through `internal/proc`, so cancellation kills the whole tree within 30 s. | FR-012, SC-009 (R8) |
| A5 | `ctx` cancellation produces `Status = Cancelled`, not `Failed`. | FR-013 |
| A6 | Timeout produces `Status = TimedOut` with `TimedOut = true` and the partial transcript preserved. | FR-026, FR-027 |
| A7 | Exit code, duration, and a durable transcript reference are always returned, even on error. | FR-027 |
| A8 | The adapter never writes `validation_results`, never claims a check passed, and never reports success without an observed exit code. | FR-025, SC-005, G3 |
| A9 | Every byte written to the transcript, activity feed, and error strings passes through the redaction writer. | FR-056 |
| A10 | The adapter must not mutate the workspace outside the agent's own allowed behaviour: it may not `git add`, `commit`, `push`, or create branches. Git operations belong to `internal/git`. | FR-038, FR-039 |

## 5. Transcript format

Raw, append-only, one file per invocation (`<run_dir>/transcript-attempt-<n>.jsonl`), plus the unparsed streams:

```text
<run_dir>/transcript-attempt-1.jsonl   # stdout, verbatim, already redacted
<run_dir>/transcript-attempt-1.err     # stderr, verbatim, already redacted
<run_dir>/prompt-attempt-1.txt         # exactly what was sent; sha256 recorded in the DB
```

`Activity` is derived from the transcript by a **tolerant** parser: unknown event shapes are skipped silently and never abort a run. The raw file is the source of truth; `Activity` is a courtesy for the UI.

## 6. Required summary block

The prompt ends with a fixed block, which the adapter parses into `Summary`:

```text
## RESULT
status: done | blocked
files: <comma-separated paths, or "(none)">
checks_run: <what the agent itself ran, phrased as an observation>
blockers: <questions for the operator; required when status: blocked>
```

Parsing rules:

- `status: blocked` + non-empty `blockers` → `Result.Status = Blocked` (FR-030) and the run ends `blocked`, not `failed`.
- Missing or malformed block → `ReportedStatus = ""`; the run continues to validation and the absence is recorded as a `warning` event. It is never treated as success or as a pass.
- `checks_run` is stored verbatim as a **claim**. `validation_results` is written exclusively by `internal/runner/validate.go`.

## 7. Containment policy file

The worker writes a policy file into the sandbox before each invocation and passes `PolicyFile`. It denies, at minimum: shell execution outside the workspace, network fetches, writes to absolute paths outside the workspace, and reads of the data directory. The file lives in a read-only sandbox subdirectory the agent cannot rewrite. Residual risk and deployment hardening are documented in `research.md` §R9 and `quickstart.md`.

## 8. v1 implementation: `opencode`

```go
// Pseudo-command, composed in internal/agent/opencode.go
exec.CommandContext(ctx, command, "run",
    "--standalone",
    "--format", "json",
    "--model", cfg.Model,
    "--agent", cfg.AgentName,
    "--title", fmt.Sprintf("Issue %d: %s", issueID, subject),
    prompt.Text,
)
// cmd.Dir = workspace; cmd.Env = allowlist + sandbox redirects; started via internal/proc
```

Notes grounded in the installed CLI (`opencode run --help`, verified 2026-10-26):

- There is **no** `--dir` flag. The working directory is the only way to scope the agent, so `cmd.Dir` is mandatory (A1).
- `--standalone` gives each run a private server instead of sharing the operator's background daemon — isolation, and a clean tree to terminate.
- `--auto` is **not** used: auto-approving permissions contradicts FR-029.
- `--session` is reserved for repair attempts so an attempt can see its own prior context without inheriting an unrelated conversation.
- `--format json` shapes are not publicly specified; hence §5's tolerant parsing and the golden-file test that pins the observed shape.

## 9. Adding an adapter (the point of FR-028)

1. Implement `Adapter` in `internal/agent/<name>.go`.
2. Register it in the adapter registry by `Name()`.
3. Add an example block to `config.example.yaml` under `agent:`.
4. Add a fake-agent style test that proves A1–A10 for that adapter (the containment tests are reusable).

No change is permitted in `internal/runner`, `internal/validate`, `internal/git`, `internal/gitlab`, or the API when adding an adapter. If one is needed, this contract is wrong and must be amended first.
