# blueprint

Read-only census of AWS-managed databases. Single Go binary, runs locally,
writes local artifacts only. PLG wedge for hoop.dev — third tool in the
family alongside leash and hooprs. Linear project: "Resources Mapping for
DBAs" (team Attract, issues ATR-1xx).

## Commands

```sh
go build ./...                                  # build everything
go test ./...                                   # full test suite
go run . scan --demo --no-open -o /tmp/out      # render all outputs from fixtures, no AWS
gofmt -w internal/                              # format before committing
```

There is no lint config beyond gofmt. CI (`.github/workflows/ci.yml`) runs
build + tests on every push.

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

## Invariants — do not break these

- **Read-only**: only AWS describe/list/get calls, ever. New scanner calls
  must be covered by `docs/iam-policy.json`.
- **Offline report**: the HTML report loads zero external resources (no
  fonts, scripts, links); tests in `internal/render/html_test.go` enforce
  this plus script-breakout escaping. Keep everything inline.
- **Honesty guardrails**: environment/owner come from tags only (imported,
  never inferred); scan units the tool could not see go to the failure
  ledger; exposure fields are pointer-typed so "not reported" (nil) never
  masquerades as a safe value.
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
