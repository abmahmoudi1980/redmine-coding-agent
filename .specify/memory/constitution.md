<!--
SYNC IMPACT REPORT
==================
Version change: (unratified scaffold template, no version) → 1.0.0

Reason for the major-line adoption (1.0.0): initial ratification. The template carried no
principles; this is the first governing version, so nothing can be backward compatible with it.

Modified principles (template placeholders → ratified principles):
  PRINCIPLE_1_NAME  → I.   Independent Verification (NON-NEGOTIABLE)
  PRINCIPLE_2_NAME  → II.  Human Authority Over Publishing (NON-NEGOTIABLE)
  PRINCIPLE_3_NAME  → III. Durable, Auditable Run State
  PRINCIPLE_4_NAME  → IV.  Secret Hygiene
  PRINCIPLE_5_NAME  → V.   Least Privilege for Autonomous Agents
  (none in template) → VI.  Portability Across Linux and Windows
  (none in template) → VII. Simplicity and Explicit Configuration

Added sections:
  - Technology Constraints (replaces the SECTION_2 placeholder)
  - Development Workflow & Quality Gates (replaces the SECTION_3 placeholder)
  - Governance (written from scratch; template carried only an example comment)

Removed sections:
  - Template placeholder comments (examples replaced by normative text)

Ratification basis:
  These principles codify gates G1–G7 that `specs/010-redmine-coding-agent/plan.md` had
  recorded as provisional, derived from spec.md FR/SC requirements plus the operator's
  stack directive (Linux + Windows, Go backend, Svelte frontend, SQLite).

Templates requiring updates:
  ✅ .specify/templates/plan-template.md     — generic "Constitution Check" section already
                                               accommodates seven gates; no edit required
  ✅ .specify/templates/spec-template.md     — spec scope/requirement sections unaffected
  ✅ .specify/templates/tasks-template.md    — task categories unaffected
  ✅ .specify/templates/commands/            — directory does not exist; nothing to check

Runtime guidance requiring updates:
  ✅ specs/010-redmine-coding-agent/plan.md   — updated: gates re-labelled with principle mapping
  ✅ specs/010-redmine-coding-agent/tasks.md  — updated: closing note and Tests rationale now cite
                                               Principles I and III
  ✅ AGENTS.md                               — contains no constitution reference; no edit needed

Deferred items: none. Ratification date is the project's first constitution date (2026-10-26),
taken as today because the template carried no earlier adoption date.
-->

# RedmineCodingAgent Constitution

This constitution governs the internal worker that turns one Redmine issue number into a verified,
reviewable merge request. It binds every artifact in this repository: the specification, the plan,
the task list, the code, and the reviews. Where a design decision and this document disagree, this
document wins and the design is amended.

## Core Principles

### I. Independent Verification (NON-NEGOTIABLE)

The worker, never the coding agent, decides whether an implementation is acceptable.

- Every configured check MUST be executed by the worker itself, with the exact command, exit code,
  duration, and a `passed` / `failed` / `not_executed` classification recorded.
- A check MUST NOT be recorded as passed without an exit code of 0. This is enforced in the store,
  not only in the runner: a row claiming a pass with a non-zero or missing exit code MUST be
  rejected by the database.
- Anything the agent says about its own testing is a **claim**. It MUST be stored as
  `reported_status` and MUST NOT influence any validation outcome.
- A required check that did not run MUST be treated as a failure for the purpose of completion.
- A run MUST NOT be reported as validated, and MUST NOT proceed to push or merge-request creation,
  when any required check failed or did not run.

**Rationale**: an automation that certifies its own output is a code generator with a review
theatre attached. Independence is the product's entire value, and it is cheap to guarantee only
if it is guaranteed structurally.

### II. Human Authority Over Publishing (NON-NEGOTIABLE)

A human approves and merges. The tool never does.

- The tool MUST NOT merge a merge request, MUST NOT force-push, and MUST NOT write to a base or
  protected branch. No such operation may exist as a code path — not merely unused, absent.
- Starting work MUST require explicit confirmation of the displayed issue, repository, and base
  branch. There is no `--force`-style bypass in the CLI and no implicit-start endpoint.
- An ambiguous or missing repository mapping MUST block the run and list the candidates. The tool
  MUST NOT infer a repository, and MUST NOT fall back to a repository's default branch.
- Repeated publishing MUST be idempotent: reuse or update the existing merge request for the same
  source and target branch rather than creating a duplicate.

**Rationale**: the boundary between "proposes work" and "commits work" is the only reason a team
can safely leave this running unattended.

### III. Durable, Auditable Run State

Every attempt is reconstructable after a crash, a restart, or a change of operator.

- Every run MUST persist its identifier, issue, timestamps, state, the stage reached before each
  transition, the agent and configuration used, per-check results, commit and merge-request
  references, and failure details.
- The event timeline MUST be append-only and written in the same transaction as the state change it
  describes, so ordering can never diverge from the state machine.
- On startup, every non-terminal run MUST be marked `interrupted` with its last stage, and MUST NOT
  be resumed automatically into a push, merge request, or report.
- Terminal states MUST be final. A retry MUST create a new run, linked to its predecessor, never a
  resurrection of the old one.
- History MUST survive restarts and MUST be readable without opening raw logs.

**Rationale**: network-dependent automation fails, and a tool whose history becomes ambiguous after
its first bad day is one an operator cannot trust again.

### IV. Secret Hygiene

Credentials are supplied, never stored, never rendered, never inferred.

- Credentials MUST be referenced only through `env:NAME` or `file:/path` indirection in
  configuration. A literal secret in the repository, the database, a log, a prompt, a report, or an
  API response is a defect.
- Resolved secret values MUST live in one registry, and every write path — logs, transcripts,
  prompts, reports, HTTP responses, error messages — MUST pass through redaction.
- Persisted configuration snapshots MUST be redacted; only secret *references* may be stored.
- Issue text, comments, and repository instructions MUST be treated as untrusted data. They MUST NOT
  be able to override operating restrictions, the credentials policy, or publishing rules.
- A test MUST scan the database, logs, transcripts, prompts, and reports for fixture secret values
  and find zero matches before a release is considered valid.

**Rationale**: indirection plus one choke point makes "no credentials in the codebase" an
enforceable property instead of an aspiration.

### V. Least Privilege for Autonomous Agents

An autonomous process is an untrusted process that happens to be useful.

- The agent MUST run with its working directory set to the run's workspace, with `HOME`, XDG paths,
  and temporary directories redirected into a run-scoped sandbox, and with an explicit environment
  allowlist rather than an inherited environment.
- The agent MUST receive a read-only policy that denies shell execution outside the workspace,
  network fetches, absolute-path writes, and reads of the tool's data directory.
- The agent MUST NOT receive production credentials, and auto-approving its permission prompts is
  forbidden.
- Children MUST be contained as a tree: process group on Unix, Job Object on Windows. Cancellation
  MUST terminate the agent and its descendants within 30 seconds.
- Observed attempts to escape the boundary MUST fail the run as a policy violation rather than be
  logged as a curiosity.
- Deployment guidance for stronger isolation (container, dedicated service account) MUST be
  documented, and the residual risk MUST be stated plainly rather than described as a sandbox.

**Rationale**: honest containment is a layered set of controls with a written limit. Claiming
kernel-level isolation that the binary cannot provide would make every other promise suspect.

### VI. Portability Across Linux and Windows

Both hosts are first-class; no feature may work on only one of them.

- The product MUST ship as one statically linked binary per OS/architecture, built with
  `CGO_ENABLED=0` and no C toolchain.
- Runtime dependencies MUST be limited to `git`, the embedded SQLite file, and the configured agent
  executable. Node is a build-time dependency and MUST NOT be required at runtime.
- OS-specific behaviour MUST be confined to build-tagged files in `internal/proc`. No other package
  may branch on the operating system.
- Every user story MUST be validated end to end on both operating systems before release.
- Persisted timestamps MUST be UTC; display localisation belongs to the client.

**Rationale**: the operator directive names Linux and Windows, and cross-platform rot starts the
moment a second package learns about `GOOS`.

### VII. Simplicity and Explicit Configuration

Solve the problem in front of us with the smallest defensible mechanism.

- Every operator-visible knob — mappings, base branch, checks, agent, model, timeouts, repair
  limits, concurrency, publishing and reporting policies, retention — MUST be configuration, and a
  configuration change MUST take effect on restart without a rebuild.
- Configuration MUST be strictly parsed: unknown keys are errors, not silent no-ops.
- The standard library MUST be preferred. Each new backend dependency MUST be justified in
  `research.md` with the alternative that was rejected.
- No ORM, no message queue, no reverse proxy, no separate frontend runtime, and no service framework
  that duplicates the platform.
- Work outside the specification's stated scope MUST be deferred, not absorbed.

**Rationale**: a single-operator internal tool earns trust by being small enough to read in an
afternoon; every convenience abstraction is surface area nobody asked to audit.

## Technology Constraints

The stack is fixed by operator directive and is not reopened by feature work.

- **Backend**: Go 1.24 or newer, standard-library HTTP (`net/http` routing and middleware).
- **Storage**: SQLite via the pure-Go `modernc.org/sqlite` driver, WAL journal, embedded numbered
  migrations, one writer connection.
- **Frontend**: SvelteKit 2 with Svelte 5, static adapter, server-side rendering disabled, compiled
  at build time and embedded in the binary.
- **Live progress**: Server-Sent Events with replay by monotonic sequence. No WebSocket, no polling.
- **External executables**: `git` (2.30+) and the configured coding agent. Nothing else.
- **Processes**: every child runs under the containment described in Principle V.
- **Concurrency**: one worker process per database, one active run by default, limit externally
  configurable.
- **Errors**: machine-readable problem documents (`application/problem+json`) whose text is already
  redacted and already actionable.

## Development Workflow & Quality Gates

- **Contract first.** `specs/010-redmine-coding-agent/contracts/openapi.yaml`, `cli.md`, and
  `agent-adapter.md` are authoritative. A route, flag, or adapter behaviour that drifts from them is
  a failing test, not a documentation task.
- **Task discipline.** `tasks.md` is the execution plan. Tasks are ordered by user story, each story
  is independently testable, and a story is done only when its stated Independent Test passes on
  both operating systems.
- **Tests before implementation within a story.** A test task MUST fail for the right reason before
  its implementation task starts. Tests live beside the code (`internal/**/*_test.go`) or in
  `tests/integration/`; contract fixtures come from the OpenAPI document.
- **Non-negotiable tests.** Redaction coverage, process-tree cancellation, state-transition
  legality, the "no pass without exit 0" store constraint, and the secret scan MUST exist and MUST
  pass. They are not optional coverage.
- **Gate suite.** `go vet`, `golangci-lint`, `go test -race ./...` on Linux, `go test ./...` on
  Windows, `npm run check`, `npm test`, the Playwright suite, and the four-target cross-compile.
  A red gate blocks integration.
- **Schema changes** are additive migrations. An applied migration MUST NOT be edited.
- **Commits** are small and per task or tight group. Drive-by refactors and unrelated cleanups are
  rejected in review.
- **Complexity** MUST be justified in the plan's Complexity Tracking table, or removed.

## Governance

This constitution supersedes other practices in this repository, including convenience conventions
in `AGENTS.md`. `AGENTS.md` carries runtime operational guidance; the plan carries design; the
constitution carries authority.

**Amendment procedure**

1. Propose the change with its rationale, its blast radius, and the artifacts it affects.
2. Update this document and prepend a Sync Impact Report recording the version change, modified
   principles, and added or removed sections.
3. Propagate the change to dependent artifacts in the same change: `plan.md` (Constitution Check),
   `tasks.md` (task categories and gates), `spec.md` (mandatory constraints), and the specification
   templates where they encode a gate.
4. Record ratification with a date. An unpropagated amendment is not ratified.

**Versioning policy** (semantic)

- **MAJOR**: a principle is removed or redefined in a way that invalidates existing compliant work.
- **MINOR**: a principle or section is added, or guidance is materially expanded.
- **PATCH**: clarifications, wording, or typo fixes with no semantic change.

**Compliance review**

- Every review MUST verify the gates in Principles I–VII and the Technology Constraints; an
  unjustified violation blocks merge.
- Each `/speckit.plan` MUST re-evaluate the Constitution Check after design; a design that cannot
  satisfy a principle must be redesigned, not waived.
- At minimum once per release, re-read this constitution against the code and record any drift.
- A violation disclosed in the Sync Impact Report is acceptable when justified and time-bounded. An
  undisclosed violation is not.

**Version**: 1.0.0 | **Ratified**: 2026-10-26 | **Last Amended**: 2026-10-26
