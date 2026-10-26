<!-- SPECKIT START -->
For additional context about technologies to be used, project structure,
shell commands, and other important information, read the current plan
at specs/010-redmine-coding-agent/plan.md

Supporting design artifacts for the active feature:

- specs/010-redmine-coding-agent/spec.md — feature specification
- specs/010-redmine-coding-agent/research.md — technology decisions and rationale
- specs/010-redmine-coding-agent/data-model.md — schema, state machine, invariants
- specs/010-redmine-coding-agent/contracts/openapi.yaml — HTTP + SSE contract
- specs/010-redmine-coding-agent/contracts/cli.md — CLI contract and exit codes
- specs/010-redmine-coding-agent/contracts/agent-adapter.md — coding-agent boundary
- specs/010-redmine-coding-agent/quickstart.md — run and validation guide

Stack: Go 1.24 (CGO_ENABLED=0), SQLite via modernc.org/sqlite, SvelteKit 2 +
Svelte 5 embedded in the binary, targeting Linux and Windows.
<!-- SPECKIT END -->
