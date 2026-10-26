# CLI Contract — `redmine-agent`

**Feature**: `010-redmine-coding-agent` | **Consumers**: operators, cron/Task Scheduler, `systemd`, `sc.exe`, CI

The CLI and the HTTP API are two front doors onto the same orchestration code. Anything the UI can do, an operator can do headlessly; nothing in the CLI bypasses validation, confirmation, or policy.

```
redmine-agent <command> [flags]
```

Global flags, valid on every command:

| Flag | Default | Meaning |
|---|---|---|
| `--config <path>` | `./config.yaml` (or `$REDMINE_AGENT_CONFIG`) | Configuration file. Resolved secrets come only from `env:`/`file:` references inside it. |
| `--json` | `false` | Machine-readable output on stdout. Human output goes to stderr when this is set. |
| `--log-level <level>` | `info` | `debug`, `info`, `warn`, `error`. JSON logs go to stderr and to the run log file. |
| `--no-color` | auto | Disable ANSI styling. |

---

## `serve`

The only long-running mode. Serves the embedded SPA and the API, and owns the run pipeline.

```
redmine-agent serve --config config.yaml
```

| Flag | Default | Meaning |
|---|---|---|
| `--listen <addr>` | from config (`http.listen`, default `127.0.0.1:8080`) | Overrides the bind address. Binding to a non-loopback address is logged as a warning. |

On start it (1) validates configuration, (2) applies migrations, (3) runs the recovery sweep (every non-terminal run becomes `interrupted`, FR-051), (4) starts the scheduler and HTTP listener, (5) logs the effective port, build identity, and `doctor` summary. `SIGINT`/`SIGTERM` (Ctrl+C / `sc stop` / `systemctl stop`) triggers graceful shutdown: stop accepting runs, cancel the active run, persist state, exit `0`.

---

## `run`

Headless equivalent of clicking Start Development and confirming. Still requires explicit confirmation — via `--yes` or an interactive prompt. This is the command a scheduler or a colleague's script uses.

```
redmine-agent run --issue 4821 --yes
redmine-agent run --issue 4821 --repository 3 --yes --wait=false
```

| Flag | Default | Meaning |
|---|---|---|
| `--issue <n>` | required | Redmine issue number. Validated before any network call (FR-002). |
| `--repository <id>` | from mapping | Required only when the project is ambiguous (FR-015). |
| `--yes` | `false` | Skips the interactive confirmation. Without it, the preview is printed and the command waits for `y`. |
| `--wait` | `true` | Stream stage/activity to stderr until the run reaches a terminal state. With `--wait=false`, prints the run id and exits `0`. |
| `--format <fmt>` | `human` | `human` or `json` (implies `--wait` reporting as JSON lines). |

Exit status reflects the terminal state, so a scheduler can branch without parsing output (see the table below).

---

## `runs`

```
redmine-agent runs list [--state failed] [--issue 4821] [--limit 25] [--json]
redmine-agent runs show <run-id> [--json]
redmine-agent runs checks <run-id>
redmine-agent runs logs <run-id> [--tail 500]
redmine-agent runs report <run-id>
```

`runs list` prints a table (id, issue, state, stage, started, duration) newest first. `show` prints the detail view the UI shows: repository, branch, base revision, agent, invocations, checks, commit, merge request, failure code. `--json` emits the same documents the API returns, which makes these commands usable in scripts without a second contract.

---

## `cancel` / `retry`

```
redmine-agent cancel <run-id>
redmine-agent retry <run-id> --yes
```

`cancel` is idempotent and exits `0` once the request is recorded, or after the process tree is gone if it was called with `--wait`. `retry` creates a new linked attempt (FR-052) and refuses while the previous attempt is still active.

---

## `doctor`

Pre-flight verification, the operational answer to FR-058. Never mutates a repository.

```
redmine-agent doctor --config config.yaml [--deep] [--json]
```

Checks, in order, stopping at the first failure unless `--all` is passed:

1. configuration parses, `schema_version` is known, and required sections exist
2. every secret reference resolves (reports **which reference**, never the value)
3. the database file is writable, migrations are current
4. `git` is on `PATH` and `>= 2.30`
5. the workspace root and data directory are writable
6. Redmine is reachable and the credential can read one configured project
7. GitLab is reachable through the required network path, and the credential can see at least one mapped project
8. each mapped repository fetches with the configured credential (deep mode only)
9. the configured agent executable answers `--version`
10. each mapped repository has at least one required check (warning, not a failure)

Output is one line per check with status, latency, and a remediation hint. `--deep` performs the network fetches; without it, only fast local checks plus unauthenticated reachability run.

---

## `version` / `prune`

```
redmine-agent version [--json]        # version, commit, build date, go version, os/arch, schema version
redmine-agent prune --older-than 30d  # deletes run directories and rows older than the window
```

`prune` refuses to delete any workspace with `has_unpushed_changes = 1` and reports what it skipped (FR-053). It never deletes pushed branches or merge requests (FR-054). It is manual: no scheduler is installed by the binary.

---

## Exit codes

| Code | Name | When |
|---|---|---|
| `0` | `ok` | Command succeeded; a run reached `completed` (or was created with `--wait=false`). |
| `1` | `internal` | Unexpected error. The message names the component and run id. |
| `2` | `usage` | Bad flags or arguments; nothing was attempted. |
| `3` | `config` | Configuration missing, invalid, or a secret reference unresolved. |
| `4` | `dependency_unreachable` | Redmine, GitLab, the repository, or the agent could not be reached (FR-016/FR-017). |
| `5` | `validation_failed` | The run finished, but a required check failed or did not run (FR-037). |
| `6` | `run_failed` | The run ended `failed` for another reason (agent error, no changes, push rejected…). |
| `7` | `blocked` | The run ended `blocked`: ambiguous/critical requirements or no repository mapping. |
| `8` | `cancelled` | The run was cancelled. |
| `9` | `interrupted` | The run was found interrupted after a restart. |

Codes `4`–`9` intentionally mirror the run's terminal state so a pipeline can tell "retry later" (4) from "a human must look" (5–7).

---

## Machine output

`--json` writes exactly one document per line on stdout, no prose:

- `run` → the `Run` object from `contracts/openapi.yaml`
- `runs list` → `{"items":[…],"nextCursor":…}`
- `doctor` → `{"status":"ok|degraded","components":[…]}`
- errors → `{"code":"…","message":"…","stage":"…"}` on stdout and a human line on stderr

Streaming with `--wait` and `--format json` emits one JSON object per event, using the same event names and payloads as the SSE stream, so a wrapper script can render progress without polling.

---

## Guarantees (contract-level)

1. No command starts a coding agent without an explicit confirmation (`--yes` or interactive `y`) — FR-008.
2. No command can merge, force-push, or write to a base branch — there is no such flag.
3. Secrets never appear on stdout, stderr, in `--json` output, or in run artifacts — a redaction writer sits on every output path (FR-056).
4. Every command that mutates state prints the `runId` it affected, or the `code` and `stage` when it refused.
5. `--wait` never exits before the terminal state is durably recorded.
