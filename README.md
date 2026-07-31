<div align="center">

# 📐 blueprint

### What databases are you actually running?

`blueprint` is a read-only census of what you are actually running on AWS —
RDS, Aurora, DocumentDB, Neptune, DynamoDB, ElastiCache, Redshift, EC2, EBS —
**entirely from your machine**.

Runs locally &nbsp;·&nbsp; Stays local &nbsp;·&nbsp; Read-only

[![Release](https://img.shields.io/github/v/release/hoophq/blueprint?color=4fb477&label=release)](https://github.com/hoophq/blueprint/releases/latest)
[![CI](https://github.com/hoophq/blueprint/actions/workflows/ci.yml/badge.svg)](https://github.com/hoophq/blueprint/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

<img src="docs/assets/report.png" alt="The blueprint HTML report: 46 databases across 2 accounts, attribution score, engines breakdown, and an inventory table with environment and status tags" width="760">

</div>

Past a few hundred resources, nobody has ground truth on their databases anymore: instances accumulate across regions, accounts, and teams faster than any spreadsheet or wiki page keeps up. blueprint runs locally, calls only AWS APIs, and writes its output (terminal summary, HTML report, JSON, CSV) to your local disk. Nothing leaves your machine.

## Quickstart

Homebrew (macOS & Linux):

```sh
brew install hoophq/tap/blueprint
```

Install script (macOS & Linux; verifies the release checksum, installs to `/usr/local/bin` or `~/.local/bin`):

```sh
curl -fsSL https://raw.githubusercontent.com/hoophq/blueprint/main/install.sh | sh
```

From source (Go 1.26+):

```sh
go install github.com/hoophq/blueprint@latest
```

Then, with AWS credentials available (env vars, `~/.aws` profile, SSO — the standard chain):

```sh
blueprint scan
```

No credentials handy? See what the output looks like with built-in fixture data:

```sh
blueprint scan --demo
```

## Usage

```sh
blueprint scan                          # scan all enabled regions of the current account
blueprint scan --profile prod           # use a specific AWS shared-config profile
blueprint scan --regions us-east-1,eu-west-1
blueprint scan --org                    # scan all AWS Organizations member accounts
blueprint scan --org --role-name blueprint-readonly
blueprint scan --formats html,json,csv  # choose outputs (default: html,json)
blueprint scan --out ./reports          # directory for output files (default: .)
blueprint scan --no-open                # don't open the HTML report in the browser
blueprint scan --compare last.json      # diff against a specific census JSON instead of history
blueprint scan --fail-on-change         # non-zero exit when the diff finds differences
blueprint scan --no-history             # don't archive this scan or auto-diff
blueprint scan --demo                   # render from fixture data, no AWS calls
blueprint scan --costs                  # also report last month's spend (AWS bills $0.01/request)
blueprint scan --metrics                # also read CloudWatch metrics (AWS bills $0.01/1,000 metrics)
```

## History

Every scan is archived locally under `~/.blueprint/history/` (override with
`BLUEPRINT_HISTORY_DIR`), and the next scan of the same scope automatically
shows what changed:

```
━━ changes vs last scan (Jun 12, 2026 · 33 days ago) ━━
  +2 new  ·  −1 removed  ·  ~1 changed
  + reporting-replica (rds postgres, us-east-1)
  ~ orders-prod (rds, us-east-1): engine_version 13.13 → 15.4
```

Scans are bucketed by scope (accounts + regions), so scanning a different
account or region set never diffs against the wrong baseline. Each scope
keeps its last 30 censuses; history lives on your disk and nowhere else.

## What gets scanned

- RDS (all engines)
- Aurora (MySQL and PostgreSQL clusters)
- DocumentDB
- Neptune
- DynamoDB
- ElastiCache (Redis, Valkey, Memcached)
- Redshift, including Redshift Serverless
- EC2 instances
- EBS volumes and snapshots
- NAT gateways
- Public IPv4 addresses — Elastic IPs *and* auto-assigned instance addresses
- Load balancers (Application, Network, Gateway, and Classic)
- Lambda functions

Volumes and snapshots are rows of their own rather than fields on the
instance. An instance names the volumes attached to it and carries none of
their bytes, so the same storage is never counted twice — and a volume nothing
is attached to still shows up, which is the one no console view puts in front
of you.

The public IPv4 census is deliberately built from two calls, not one. Since
February 2024 AWS bills every public IPv4 address it has handed you — about
$3.60 a month each, attached or not. `DescribeAddresses` only returns
addresses you explicitly allocated, so a census built on it alone silently
omits every address an instance was auto-assigned at launch, which in most
accounts is the majority. Those are read from the network interfaces instead,
and the overlap between the two listings is removed by the address itself.
The result is one row per billable address, including the allocated ones
attached to nothing at all.

Every resource is normalized into one model with a narrow core — CloudFormation type (`AWS::RDS::DBInstance`), name, status, region, account, creation time, tags, and the exposure flags — plus an open bag of service-specific `attributes` (engine, version, instance class, endpoint host, …) and numeric `measures` (`size_bytes`, `backup_retention_days`, …) keyed by AWS's own field names. A key that is absent means the service did not report it; it never means zero. Environment and owner are taken from tags only — imported, never inferred.

## Cost

`--costs` attaches last full calendar month's spend to the census, grouped by
service and by account, from AWS Cost Explorer.

```sh
blueprint scan --costs
blueprint scan --costs --cost-metric unblended     # amortized (default), unblended, net_amortized, net_unblended
blueprint scan --costs --cost-max-requests 5       # hard cap on billed requests (default 20)
```

**AWS charges $0.01 per Cost Explorer request** — it is the one paid API
blueprint touches, which is why cost is off by default, why the run prints the
price before spending it, and why the census records what was actually spent:

```
  … cost: querying Cost Explorer for 2026-06 across 2 account(s) — AWS bills $0.01 per request, up to $0.20
  ✓ cost: 2 Cost Explorer request(s), ~$0.02 charged by AWS
```

A normal run costs two requests ($0.02); more only if AWS paginates, or if you
add `--cost-resources`, which bills one request per service (see below).
Requests are issued one at a time against a client with retries disabled, so a
logical call is always exactly one charge, and the run stops at
`--cost-max-requests` rather than paginating into a surprise bill. That cap is
one allowance shared by every billed pass in the run, so it is a ceiling on the
whole scan and not on each pass separately. If the cap truncates the results,
the census says so instead of reporting a short total.

Spend is partitioned into what Cost Explorer attributes to a service
(`attributed`) and what it does not — taxes, support, credits, refunds
(`unattributed`) — and the two always sum to the total. Credits stay negative.
Currencies are never mixed or assumed: an amount whose currency AWS did not
report lands in its own bucket rather than being called USD.

A month stays estimated for a few days after it closes. When AWS flags the data
that way, the census records it and the run says so, because estimated figures
still move and will not reconcile to the invoice yet. `BlendedCost` is not
offered: it averages a rate across an organization's accounts, so a per-account
blended figure is a chargeback artifact rather than what that account cost.

Cost is deliberately invisible to history bucketing and to the resource diff,
so turning `--costs` on or off never re-buckets your history or reports an
unchanged estate as drifted.

### Per-resource estimates

`--costs` also asks **Cost Optimization Hub** what each individual resource
costs. That API is free, so it adds nothing to the bill above:

```
  … cost-hub: reading per-resource estimates from Cost Optimization Hub — not billed by AWS
  ✓ cost-hub: 38 resource(s) priced from 41 recommendation(s) in 3 call(s), no charge
```

It is the only AWS API that reports a dollar figure for one resource without
the caller inventing an allocation, which is why blueprint uses it and why it
never divides an account rollup across the resources in it. A per-resource
number nobody reported per resource is a number this tool made up.

The two figures answer different questions and are never added together or
reconciled: the rollup above is what AWS billed over a closed month, while a
Cost Optimization Hub figure is a forward-looking **monthly rate modelled from
recent usage**. Every per-resource amount therefore carries its method
(`coh`), an `estimated` flag, and the usage window the model ran over, so a
stale figure is visible as one.

Coverage is partial by design. Cost Optimization Hub only models resource
types it has recommendations for, so resources it does not cover simply have
no cost — which is why the run reports how many resources were priced against
how many recommendations were read. Where a figure covers only part of a
resource (storage but not compute, say), that is recorded as a caveat on the
figure rather than left for the reader to discover.

Cost Optimization Hub has to be switched on, once, from its console — it is
free and takes about a day to produce the first recommendations. Until then an
unenrolled account returns an empty list that is indistinguishable from "no
recommendations", so blueprint checks enrollment first and puts the answer in
the failure ledger rather than reporting a silent absence of cost.

### Per-resource billed spend

`--cost-resources` asks Cost Explorer what it actually **billed** each
individual resource over the last 14 days. It needs `--costs`, because it takes
its list of services to ask about from the account rollup:

```sh
blueprint scan --costs --cost-resources
```

```
  … cost-resources: asking Cost Explorer what it billed per resource over 2026-07-17 → 2026-07-30 — one billed request per service, 18 left in this run's budget
  ✓ cost-resources: 7 service(s) probed, 52 resource(s) priced, 7 request(s), ~$0.07 charged by AWS
```

**This one bills per service, not per run.** Each service probed is one
$0.01 request, drawn from the same `--cost-max-requests` allowance as the
rollup — so the rollup spends first and the overlay gets whatever is left.
Services are probed most-expensive-first, so a budget too small to cover them
all is spent where per-resource detail is worth the most, and the ones that
were never reached are recorded as skipped rather than as costing nothing.

Two things have to be true before AWS will answer at all. The account-level
**"resource-level data" preference** must be switched on in the Cost Explorer
console; it takes about a day to take effect and **is not retroactive**, so
switching it on today does not produce data for last week. And the caller needs
`ce:GetCostAndUsageWithResources`, which is a separate IAM action from
`ce:GetCostAndUsage`.

The window is **the last 14 days**, which is as far back as this API reaches —
so `ce` figures and the `--costs` rollup above cover different periods and are
never added or reconciled. AWS's own documentation disagrees with itself about
which services report resource-level data, so blueprint does not guess: it asks
each service the rollup shows spend for and records what came back, per
service, in the report:

| outcome | means |
| --- | --- |
| `rows` | it answered, with per-resource figures |
| `empty` | it answered, with nothing — no resource-level spend in the window |
| `unsupported` | AWS refused the query for this service |
| `denied` | permission or the console preference is missing |
| `failed` | the request errored for some other reason |
| `uncensused` | it has spend but no scanner covers it, so it was not asked |
| `skipped` | the request budget ran out before reaching it — never asked, not zero |

Figures carry the method `ce` and sit alongside any `coh` figure on the same
resource, in their own columns, because they answer different questions: `ce`
is what AWS billed over a closed fortnight, `coh` is a modelled monthly rate
for the period ahead. Nothing in blueprint ever adds them together.

The join from a Cost Explorer row to a census resource is exact — full ARN, or
the AWS-assigned identifier at the end of one. A row matching two resources is
refused and ledgered rather than assigned to one at random, and a row matching
none is reported as a scanner coverage gap. Where AWS splits one resource's
bill across two service names (an instance's hours under EC2-Compute and its
data transfer under "EC2 - Other", say) the components are summed and the
figure says which services it combines.

Two limits are disclosed rather than hidden. Cost Explorer returns at most
5,000 resources per service **and truncates without saying so**, so a list at
that ceiling is flagged as possibly short. And the last fortnight is data AWS
is still restating, so these figures are marked estimated until they settle.

## Metrics

`--metrics` adds CloudWatch readings the describe APIs do not carry — today,
free storage space on RDS instances.

```sh
blueprint scan --metrics
```

**AWS charges $0.01 per 1,000 metrics requested** through `GetMetricData`, so
this is off by default and the run reports the bill:

```
  … metrics: reading CloudWatch for scanned resources — AWS bills $0.01 per 1,000 metrics requested
  ✓ metrics: 38 series requested in 1 call(s), ~$0.00038 charged by AWS
```

Queries are batched 500 to a call and preceded by a free `ListMetrics` pass, so
series that do not exist are never paid for. An estate of 10,000 resources
costs a tenth of a cent.

Every reading carries the instant AWS observed it (`<measure>_as_of`), because
daily CloudWatch statistics lag the scan by a day or more and a stale number
presented as current is worse than no number. A resource that published nothing
in the lookback window gets no measure at all — that is an absent key, never a
zero, since a stopped instance and a full disk must not read the same.

Turning the flag on does not re-bucket your existing history — CloudWatch is
not a scanner, so it does not widen the census scope. A reading that moves
between scans does show up as drift, since a shrinking volume is exactly the
kind of change a recurring scan exists to catch; the observation timestamp
does not, because a clock advancing is not a finding.

## Outputs

- **Terminal**: a sprawl summary — total resources, distinct types/regions/accounts, a per-service breakdown, and counts of resources with no owner or environment tag.
- **HTML**: a single self-contained file (`blueprint-YYYY-MM-DD.html`) you can open in a browser or attach to a doc. No external assets, no CDN calls.
- **JSON**: the complete snapshot (`blueprint-YYYY-MM-DD.json`) — every resource, plus the failure ledger, plus the cost report when `--costs` is on.
- **CSV**: one row per resource (`blueprint-YYYY-MM-DD.csv`) for spreadsheets. The columns are the narrow core and stay fixed as new services land; attributes and measures ride in a final `k=v;k=v` cell.

## Required IAM permissions

blueprint needs read-only describe/list permissions. The minimal policy ([docs/iam-policy.json](docs/iam-policy.json)):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BlueprintReadOnly",
      "Effect": "Allow",
      "Action": [
        "rds:Describe*",
        "dynamodb:ListTables",
        "dynamodb:DescribeTable",
        "dynamodb:ListTagsOfResource",
        "elasticache:Describe*",
        "elasticache:ListTagsForResource",
        "redshift:Describe*",
        "redshift-serverless:List*",
        "ec2:DescribeInstances",
        "ec2:DescribeVolumes",
        "ec2:DescribeSnapshots",
        "ec2:DescribeImages",
        "ec2:DescribeNatGateways",
        "ec2:DescribeAddresses",
        "ec2:DescribeNetworkInterfaces",
        "ec2:DescribeRegions",
        "elasticloadbalancing:DescribeLoadBalancers",
        "elasticloadbalancing:DescribeTargetGroups",
        "elasticloadbalancing:DescribeTags",
        "lambda:ListFunctions",
        "lambda:ListTags",
        "s3:ListAllMyBuckets",
        "s3:GetBucketTagging",
        "s3:GetEncryptionConfiguration",
        "s3:GetBucketPublicAccessBlock",
        "s3:GetBucketPolicyStatus",
        "s3:GetBucketVersioning",
        "sts:GetCallerIdentity"
      ],
      "Resource": "*"
    },
    {
      "Sid": "BlueprintCostExplorer",
      "Effect": "Allow",
      "Action": [
        "ce:GetCostAndUsage",
        "ce:GetCostAndUsageWithResources"
      ],
      "Resource": "*"
    },
    {
      "Sid": "BlueprintCostOptimizationHub",
      "Effect": "Allow",
      "Action": [
        "cost-optimization-hub:ListEnrollmentStatuses",
        "cost-optimization-hub:ListRecommendations"
      ],
      "Resource": "*"
    },
    {
      "Sid": "BlueprintCloudWatchMetrics",
      "Effect": "Allow",
      "Action": [
        "cloudwatch:ListMetrics",
        "cloudwatch:GetMetricData"
      ],
      "Resource": "*"
    }
  ]
}
```

`BlueprintCostExplorer` and `BlueprintCostOptimizationHub` are only needed for `--costs`, and `BlueprintCloudWatchMetrics` only for `--metrics`; drop any statement whose flag you never use. Without it, the scan still completes and the missing permission is recorded in the failure ledger.

The AWS managed policies `ReadOnlyAccess` or `SecurityAudit` also cover everything blueprint calls, if you already have one of those attached.

### Org mode

`blueprint scan --org` enumerates all ACTIVE accounts in your AWS Organization and scans each one by assuming a role in it.

Requirements:

- Run it with credentials from the organization's **management account** or a **delegated administrator** account, with `organizations:ListAccounts` allowed.
- A role with the read-only policy above must exist in **every member account**, and its trust policy must allow the calling account to assume it. The default role name is `OrganizationAccountAccessRole` (created automatically for accounts made through Organizations); override with `--role-name`.
- The caller additionally needs `organizations:ListAccounts` and `sts:AssumeRole` on the member-account roles — see [docs/iam-policy-org.json](docs/iam-policy-org.json), replacing `${RoleName}` with your actual role name.
- With `--costs`, Cost Explorer is queried once from the calling account for the whole organization, not once per member account — so `ce:GetCostAndUsage` belongs on the caller, and the bill stays the same two requests no matter how many accounts you scan. `--cost-resources` is org-wide in the same way, filtered to the accounts actually scanned, so `ce:GetCostAndUsageWithResources` belongs on the caller too and its bill scales with the number of *services* probed rather than the number of accounts. Cost Optimization Hub works the same way: one org-wide list read with the caller's credentials, so `cost-optimization-hub:ListEnrollmentStatuses` and `cost-optimization-hub:ListRecommendations` belong on the caller too — those two actions and no others. Member accounts are only included if the organization enrolled them, and blueprint says so in the ledger when it finds they were not.

Accounts where the role is missing or untrusting do not abort the scan: they show up as failures in the ledger, and everything else is still scanned.

## Zero telemetry

blueprint phones home to no one. No usage analytics, no crash reporting, no update checks, not even anonymous pings. The only network calls it makes are to AWS APIs, using the credentials you provide. Output files are written to your local disk and go nowhere unless you send them somewhere.

Every one of those calls is a describe/list/get — blueprint never creates, modifies, or deletes anything in your account, including your Cost Explorer or Cost Optimization Hub preferences — blueprint reads your Cost Optimization Hub enrollment status but never changes it. Three of them are billed by AWS: `ce:GetCostAndUsage` behind `--costs`, `ce:GetCostAndUsageWithResources` behind `--cost-resources`, and `cloudwatch:GetMetricData` behind `--metrics`. All three flags are off by default, each prints the rate before spending anything, and each reports what was actually spent when the run ends.

## License

MIT © [hoop.dev](https://hoop.dev) — built by the team behind [hoop](https://github.com/hoophq/hoop). See [LICENSE](LICENSE).
