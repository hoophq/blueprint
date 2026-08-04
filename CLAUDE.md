# blueprint

Read-only census of what an AWS account runs — compute, storage, databases,
networking — with what AWS billed for it attached. Single Go binary, runs
locally, writes local artifacts only. PLG wedge for hoop.dev — third tool in
the family alongside leash and hooprs. Linear (team Attract): "Resources
Mapping for DBAs" shipped v1; current work is "Blueprint Evolution"
(total AWS resource + cost census, ATR-170…187).

## Commands

```sh
go build ./...                                  # build everything
go vet ./...                                    # CI gates on this too
go test ./...                                   # full test suite
gofmt -w internal/                              # format before committing

# render every output from fixtures, no AWS, no browser:
go run . scan --demo --costs --cost-resources --metrics \
  --formats html,json,csv --no-open -o /tmp/out
```

`--demo` alone renders only `html,json` and produces no cost or metric data —
pass the flags above when you need to see those paths.

There is no lint config beyond gofmt. CI (`.github/workflows/ci.yml`) runs
build, vet, and tests on push to main and on pull requests targeting main; it
pins Node 22 because `internal/render` evaluates the report's JS under node.

## Workflow

- One PR per Linear issue, branched on the issue's `gitBranchName`
  (`perotto/atr-NNN-…`) so Linear auto-links. Never commit to main.
- Qodo (qodo-code-review bot) reviews every PR automatically; run the
  `qodo-pr-resolver` skill to address its findings before merge.

## Architecture

- `main.go` → `internal/cli` (cobra): `scan` is the only command.
- `internal/awsx`: credential/config loading on the standard AWS chain, plus
  `AssumeRole` and region discovery. blueprint stores no credentials.
- `internal/scan`: scanner registry + concurrent runner over scan units
  (account × region × service), then a post-scan **enrichment stage**
  (`enricher.go`) that runs after every scanner and before `Finalize()`.
  Scanners self-register via `init()` in `internal/scanners`; enrichers have
  no registry — the CLI appends them explicitly, only on opt-in, and they run
  sequentially.
- `internal/scanners`: eleven scanners. `rds.go` covers RDS/Aurora/DocumentDB/
  Neptune via the shared RDS control plane; `dynamodb`, `elasticache`,
  `redshift`, `ec2`, `ebs`, `natgateway`, `publicip`, `loadbalancer` (v1+v2),
  `lambda` and `s3` are separate. Cluster members are never emitted as their
  own rows — the cluster represents them.
- `internal/model`: normalized `Resource`/`Snapshot`. A `Resource` is a narrow
  core (CloudFormation `Type` like `AWS::RDS::DBInstance`, name, status,
  scope, tags, exposure flags) plus two open bags: `Attributes`
  (`map[string]string`) and `Measures` (`map[string]int64`), keyed by AWS's
  own field names and declared as `Attr*`/`Measure*` consts. Add a service
  by adding keys, not struct fields. `Finalize()` derives tag-based fields
  (env/owner), EOL flags, and sorts for deterministic JSON. `cost.go` holds
  the cost report types and the `Probe*` outcome constants.
- `internal/cost`: Cost Explorer. `cost.go` is the `--costs` rollup
  (`GetCostAndUsage`, grouped by service and account, split into attributed
  and unattributed record types); `resources.go` is the `--cost-resources`
  per-resource pass (`GetCostAndUsageWithResources`, 14-day window, probed
  per service, outcome recorded per service). Both draw on one shared
  `Budget` of billed requests.
- `internal/enrich`: the opt-in enrichment stages. `costhub.go` reads Cost
  Optimization Hub per-resource estimates (free, enrollment-gated, attached
  by exact ARN); `metrics.go`/`specs.go` read CloudWatch through
  `ListMetrics` + `GetMetricData` (RDS free storage, S3 bucket size and
  object count), stamping every reading with `<measure>_as_of`.
- `internal/render`: terminal, JSON, CSV, and the single-file HTML report
  (`report.html.tmpl` — vanilla JS; the census rides in an inline JSON script
  block, gzip+base64 above 5,000 resources and inflated in the browser).
- `internal/orgmode`: AWS Organizations fan-out via assume-role. The caller's
  own account is scanned with its own credentials, not through a role.
- `internal/demo`: fixture snapshot behind `--demo` (also used by tests),
  plus the seeded `--demo-scale` generator.
- `internal/diff`: `--compare` census diffing, matched by ARN.
- `internal/history`: local scan archive (`~/.blueprint/history/`, override
  `BLUEPRINT_HISTORY_DIR`), bucketed by scope (accounts+regions+services);
  every scan auto-diffs against the previous one of the same scope.
  Local-only.

## Invariants — do not break these

- **Read-only**: only AWS describe/list/get calls, ever. New scanner calls
  must be covered by `docs/iam-policy.json`, by the copy of it inlined in the
  README, and by `docs/iam-policy-org.json` — the three drift apart silently
  otherwise, and the org one is where it hurts (a member role missing an
  action loses a whole scanner to AccessDenied).
- **Offline report**: the HTML report loads zero external resources (no
  fonts, scripts, links); tests in `internal/render/html_test.go` enforce
  this plus script-breakout escaping. Keep everything inline.
- **Honesty guardrails**: environment/owner come from tags only (imported,
  never inferred); scan units the tool could not see go to the failure
  ledger; nothing a service did not report may render as a value. In the
  core that means pointer-typed fields (nil = "not reported"); in the bags
  it means an **absent key** — use `SetAttr`/`SetMeasure`, which skip empty
  values, and read through `Attr`/`Measure`. A *stored* zero (0 backup days,
  0-byte table) is a real finding and must survive — end to end, including
  rendering, so a formatter may not floor a small value up to the next unit.
  Scanners decide absence from the **SDK pointer**, never from the converted
  value (`SetMeasureInt32`/`SetMeasureInt64` do this); a `> 0` filter is the
  recurring bug, because it silently reclassifies "reported as zero" as
  "not reported". Whether a zero means "none" or "stale" is the reader's
  problem, not a licence to drop it.
- **Cost honesty**: a missing figure is *not* `$0`. An unpriced resource
  carries the reason it is unpriced, and renderers must show that reason
  rather than a zero or a blank. A group total is never divided across the
  resources inside it — a per-resource number nobody reported per resource is
  one this tool invented — and no figure is ever extrapolated into a run rate
  or a projection. Amounts are verbatim decimal strings backed by `big.Rat`,
  never `float64` (COH's `estimatedMonthlyCost` is the single float entry
  point, and it is converted once, at the edge). Amounts in different
  currencies are never summed; an amount whose currency AWS did not report
  gets its own bucket. Figures from different methods or windows (`ce` billed
  over 14 days, `coh` modelled forward monthly, the closed-month rollup) are
  never added or reconciled, and every figure carries its method, window and
  estimated flag. **No price is ever computed from a rate card** — list price
  is not spend, and the error runs in the direction that gets things deleted.
- **Paid-API disclosure**: every AWS call AWS bills for is opt-in and off by
  default (`--costs`, `--cost-resources`, `--metrics`), prints its rate before
  spending anything, and reports what was actually spent when the run ends.
  Both Cost Explorer passes draw on one shared `cost.Budget` capped by
  `--cost-max-requests`, so the ceiling is per run, not per pass, and hitting
  it is disclosed rather than silently truncating a total. CloudWatch is
  billed per metric rather than per request and meters separately
  (`enrich.ChargeUSD`), but the same rule applies: quote first, report after.
  Retries are disabled on the per-request-billed Cost Explorer clients, so one
  logical call is exactly one charge; a throttle goes to the ledger for the
  user to re-run deliberately.
  Anything AWS truncates without saying so (Cost Explorer's 5,000 resources
  per service) must be flagged as possibly short.
- **Schema version**: bump `model.SchemaVersion` whenever a JSON field's
  representation changes — diff/--compare refuse cross-schema baselines
  instead of fabricating drift. `Snapshot.Services` is part of
  `history.ScopeKey`, so adding a scanner starts fresh history buckets by
  design.
- **CSV**: the column set is closed — the narrow core plus one block of cost
  columns per attribution method — so it stays stable as services land;
  attributes and measures flatten into the final `attributes` cell. Methods
  get separate blocks rather than a shared one so every column stays summable
  and the two are never silently mixed. Cells are formula-injection guarded
  (`guardFormula`); keep new string columns guarded.
- **Determinism**: JSON artifacts must stay byte-for-byte stable for a given
  snapshot (`Finalize` sorts everything).

## Release

Tag `vX.Y.Z` on main → `.github/workflows/release.yml` → goreleaser builds
darwin/linux/windows (amd64+arm64), creates the GitHub release, and pushes
the Homebrew formula to `hoophq/homebrew-tap` (needs the
`HOMEBREW_TAP_GITHUB_TOKEN` repo secret — a PAT with write access to the
tap). `install.sh` is served raw from main and downloads release archives,
verifying `checksums.txt`.
