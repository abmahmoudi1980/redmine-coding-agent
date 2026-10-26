# Security model

**Scope**: self-hosted `redmine-agent` v1.0. This document records the security
review pass (T142): what is redacted, where containment stops, and how to verify
both. It is deliberately specific about the residual risk rather than claiming a
sandbox the worker cannot enforce.

---

## 1. Secret handling

Secrets are never literal configuration values. Every credential is a reference:

| Reference | Resolution |
|---|---|
| `env:NAME` | read the process environment at use time |
| `file:/path` | read a mounted file at use time |

`config.SecretRef` has no `String()` that returns the value, so an accidental
`%v` prints the reference name only. Resolved values are registered with
`logging.Redactor`, which stores the raw value plus its URL-, path-, and
JSON-escaped variants and replaces every variant with `***`.

### Redaction coverage (write paths audited)

| Path | Mechanism | Test |
|---|---|---|
| Process log (stderr, JSON) | `logging.Logger` wraps the sink in `Redactor.Writer` | `internal/logging` |
| Per-run log `<log_dir>/<run_id>.log` | same writer, separate sink | `internal/logging` |
| Run events (`run_events.message/data`) | `store` redacts on write | `internal/store` |
| Validation output excerpts | `validate.Runner` scrubs before persisting | `internal/validate` |
| Agent prompt | built from non-secret config + issue snapshot only | `internal/agent/prompt_test.go` |
| Agent environment | allowlist; credential-like names dropped; `HOME`/`TMPDIR` redirected | `internal/agent/sandbox.go` |
| HTTP responses | `redactingResponseWriter` buffers JSON/text bodies before flush | `internal/api` |
| SSE events | events are redacted at the source (store write) | `internal/api/events_test.go` |
| Reports | rendered from already-redacted run facts; persisted through the redacting store | `internal/runner/report_test.go` |
| CLI stdout/stderr | `newCLIOutput` wraps both in `Redactor.Writer`, line-buffered | `cmd/redmine-agent` |
| Subprocess argv | git credentials are passed through an environment header, never argv | `internal/git` |
| DB dump / transcripts / prompts on disk | written from the redacted sources above | `tests/integration/secret_scan_test.go` |

`TestSecretScanFindsNoFixtureValue` runs a full pipeline and greps the database
dump, every log file, transcript, prompt, report, and captured HTTP response for
the fixture credentials. Any match fails the build (SC-013).

### Deliberate non-goals

- **Not encryption at rest.** SQLite, logs, and workspaces inherit the host's
  filesystem permissions. Use full-disk encryption or an encrypted volume.
- **No user authentication in the HTTP API.** v1 binds to `127.0.0.1` by
  default; exposing it on a LAN address is a logged, deliberate choice and must
  be paired with a reverse proxy that authenticates operators.

---

## 2. Agent containment

Containment is layered (research §R9), not a kernel sandbox:

1. **Scope.** The agent runs with `cmd.Dir` set to the run's isolated worktree.
   `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, `TMPDIR`/`TEMP`
   point inside `<run_dir>/sandbox/`, so the agent cannot read the operator's
   credentials or personal configuration.
2. **Policy file.** `WritePolicy` writes `policy.json` with mode `0400` inside a
   policy directory chmod `0500`, before each invocation. It denies shell
   execution outside the workspace, network fetches, absolute-path writes, and
   reads/writes under the data directory. The agent cannot edit it: it has no
   write bit on the file or the directory.
3. **Post-hoc detection.** After the invocation the worker inspects the
   transcript for permission-denied events and fails the run with
   `policy_violation` when an escape was attempted, even if the attempt failed
   (proved by the fake agent's `escape-path` mode).
4. **Process containment.** Every child runs in its own process group (Linux) or
   Job Object with `KILL_ON_JOB_CLOSE` (Windows); cancellation kills the whole
   tree, including grandchildren (SC-009).

### Residual risk (stated, not hidden)

On a host without containers, a **deliberately malicious** agent that runs with
the worker's own account rights has the worker's own reach. The policy file and
prompt restrictions are the agent's own permission layer; a hostile binary can
ignore them. Mitigations, in order of strength:

1. Run the worker in the provided container (`deploy/docker/compose.yaml`),
   non-root, read-only root filesystem, all capabilities dropped.
2. On Linux without containers, use the systemd unit in
   `deploy/systemd/` (dedicated account, `ProtectSystem=strict`,
   `ProtectHome=true`, `NoNewPrivileges=true`).
3. On Windows, use a dedicated service account via
   `deploy/windows/install-service.ps1` whose ACLs reach only the data root; no
   production shares, no other users' profiles.

The worker never merges a merge request, never writes to a base branch, and has
no force-push code path. Those guarantees are structural (no such method or
flag exists), not policy enforcement.

---

## 3. Network and dependency posture

- Outbound traffic is limited to the configured Redmine, GitLab, the mapped
  repositories, and the agent's own model provider. There is no telemetry.
- The agent process gets only the allowlisted environment; credential-like
  variables are dropped by name (`TOKEN`, `API_KEY`, `SECRET`, `PASSWORD`,
  `CREDENTIAL`).
- `doctor` reports the *reference name* for an unresolved secret, never the
  value, and exits `4` rather than starting a run with a broken dependency.
- The SQLite database is opened with `foreign_keys=on`, WAL, and a bounded
  `busy_timeout`; one worker per database is the documented deployment shape.

---

## 4. How to re-run the security review

```bash
# whole suite, including the secret scan and containment tests
CGO_ENABLED=0 go test ./cmd/... ./internal/... ./tests/... -count=1

# only the secret scan (slow: runs a full pipeline)
go test ./tests/integration -run TestSecretScan -v

# only containment
go test ./internal/proc ./tests/integration -run 'Cancel|Policy|Escape' -v

# dependency and static analysis gates
go vet ./cmd/... ./internal/... ./tests/...
golangci-lint run ./cmd/... ./internal/... ./tests/...
```

When adding a new sink (a file, a header, a response field), add it to the table
in §1 and extend the secret-scan fixture list. A sink is not "probably safe"
because it is nearby; it is safe when the scan proves it.
