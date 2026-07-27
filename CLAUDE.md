# blueprint

Read-only census of AWS-managed databases. Single Go binary, runs locally,
writes local artifacts only. PLG wedge for hoop.dev — third tool in the
family alongside leash and hooprs. Linear (team Attract): "Resources
Mapping for DBAs" shipped v1; current work is "Blueprint Evolution"
(total AWS resource + cost census, ATR-170…187).

## Commands

```sh
go build ./...                                  # build everything
go test ./...                                   # full test suite
go run . scan --demo --no-open -o /tmp/out      # render all outputs from fixtures, no AWS
gofmt -w internal/                              # format before committing
```

There is no lint config beyond gofmt. CI (`.github/workflows/ci.yml`) runs
build + tests on every push.

## Workflow

- One PR per Linear issue, branched on the issue's `gitBranchName`
  (`perotto/atr-NNN-…`) so Linear auto-links. Never commit to main.
- Qodo (qodo-code-review bot) reviews every PR automatically; run the
  `qodo-pr-resolver` skill to address its findings before merge.

## Architecture

- `main.go` → `internal/cli` (cobra): `scan` is the only command.
- `internal/scan`: scanner registry + concurrent runner over scan units
  (account × region × service). Scanners self-register via `init()` in
  `internal/scanners` (rds.go covers RDS/Aurora/DocumentDB/Neptune via the
  shared RDS control plane; dynamodb, elasticache, redshift are separate).
- `internal/model`: normalized `Resource`/`Snapshot`. `Finalize()` derives
  tag-based fields (env/owner), EOL flags, and sorts for deterministic JSON.
- `internal/render`: terminal, JSON, CSV, and the single-file HTML report
  (`report.html.tmpl` — vanilla JS, data embedded as a JSON script block).
- `internal/orgmode`: AWS Organizations fan-out via assume-role.
- `internal/demo`: fixture snapshot behind `--demo` (also used by tests).
- `internal/diff`: `--compare` census diffing, matched by ARN.
- `internal/history`: local scan archive (`~/.blueprint/history/`, override
  `BLUEPRINT_HISTORY_DIR`), bucketed by scope (accounts+regions); every scan
  auto-diffs against the previous one of the same scope. Local-only.

## Invariants — do not break these

- **Read-only**: only AWS describe/list/get calls, ever. New scanner calls
  must be covered by `docs/iam-policy.json`.
- **Offline report**: the HTML report loads zero external resources (no
  fonts, scripts, links); tests in `internal/render/html_test.go` enforce
  this plus script-breakout escaping. Keep everything inline.
- **Honesty guardrails**: environment/owner come from tags only (imported,
  never inferred); scan units the tool could not see go to the failure
  ledger; any Resource field some service does not report is pointer-typed
  (nil = "not reported" — e.g. exposure fields, MultiAZ), never a
  zero-valued bool/int.
- **Schema version**: bump `model.SchemaVersion` whenever a JSON field's
  representation changes — diff/--compare refuse cross-schema baselines
  instead of fabricating drift. `Snapshot.Services` is part of
  `history.ScopeKey`, so adding a scanner starts fresh history buckets by
  design.
- **CSV**: cells are formula-injection guarded (`guardFormula`); keep new
  string columns guarded.
- **Determinism**: JSON artifacts must stay byte-for-byte stable for a given
  snapshot (`Finalize` sorts everything).

## Release

Tag `vX.Y.Z` on main → `.github/workflows/release.yml` → goreleaser builds
darwin/linux/windows (amd64+arm64), creates the GitHub release, and pushes
the Homebrew formula to `hoophq/homebrew-tap` (needs the
`HOMEBREW_TAP_GITHUB_TOKEN` repo secret — a PAT with write access to the
tap). `install.sh` is served raw from main and downloads release archives,
verifying `checksums.txt`.
