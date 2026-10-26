# Performance baseline (T141)

Measured on the implementation host, against the deterministic fake services
(`tests/integration`), so the numbers are reproducible in CI rather than
dependent on a live Redmine/GitLab.

| Gate | Budget | Measured | How |
|---|---|---|---|
| Preview latency (SC-001) | p95 ≤ 3 s | **p95 0.55 ms** (p50 0.42 ms, 40 requests) | `TestPreviewPerformanceBudget` |
| Embedded SPA first paint | < 1 s | **0.34 ms**, 1 653 bytes | `TestEmbeddedSPAIsServedQuickly` |
| Stripped binary size | ≤ 40 MB | **12.0 MB** | `TestStrippedBinarySizeBudget` (`-trimpath -ldflags "-s -w"`, `CGO_ENABLED=0`) |
| Full Go suite | green | ~74 s wall clock | `go test ./cmd/... ./internal/... ./tests/... -count=1` |

## Notes

- The preview budget is dominated by the pipeline's own read of Redmine and the
  SQLite repository list. Against a real Redmine over a VPN the network RTT is
  the number that matters; the budget leaves > 5 000× headroom for it.
- The binary measured here embeds the placeholder `internal/web/dist` unless
  the frontend has been built first. A release build after `npm run build`
  embeds the real assets; the 40 MB ceiling still applies (`make build-all`
  strips with `-s -w`).
- Timings were taken on Linux. Windows runs the same tests in the
  `windows-latest` CI leg; the budgets are intentionally loose enough that CI
  variance cannot flap.

Reproduce:

```bash
go test ./tests/integration -run 'TestPreviewPerformanceBudget|TestEmbeddedSPAIsServedQuickly|TestStrippedBinarySizeBudget' -v
```
