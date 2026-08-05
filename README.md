<div align="center">

# 📐 blueprint

### What are you running on AWS, and what does it cost?

`blueprint` is a read-only census of what you are actually running on AWS —
compute, storage, databases, networking — and, when you ask for it, **what AWS
charged for it**, **entirely from your machine**.

Runs locally &nbsp;·&nbsp; Stays local &nbsp;·&nbsp; Read-only

[![Release](https://img.shields.io/github/v/release/hoophq/blueprint?color=4fb477&label=release)](https://github.com/hoophq/blueprint/releases/latest)
[![CI](https://github.com/hoophq/blueprint/actions/workflows/ci.yml/badge.svg)](https://github.com/hoophq/blueprint/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

<img src="docs/assets/report.png" alt="The blueprint HTML report, cost first: 98 resources across 2 accounts and 4 regions, a spend headline beside the account total, disclosures for incomplete scan coverage and partial cost coverage, an attribution score, spend broken down by service, and a separate section of Cost Optimization Hub savings suggestions ranked by modelled monthly saving" width="760">

</div>

Past a few hundred resources, nobody has ground truth on their AWS estate anymore: instances, volumes, buckets, and addresses accumulate across regions, accounts, and teams faster than any spreadsheet or wiki page keeps up, and the bill arrives as one number a month later. blueprint runs locally, calls only AWS APIs, and writes its output (terminal summary, HTML report, JSON, CSV) to your local disk. Nothing leaves your machine.

Cost is opt-in — `--costs`, because AWS charges for the APIs that answer — but once it is on, it is the organizing principle of the report, and every figure in it is one AWS reported. There is exactly one price in the report and it is Cost Explorer's: what AWS billed. Cost Optimization Hub answers a different question — what you could stop paying — and its savings suggestions get their own section rather than a second price column, because a modelled saving is not money anyone was charged and is never netted off a bill. blueprint never estimates a price from a rate card, never divides an account total across the resources inside it, and never projects a run rate. Where AWS has no number for something, the census says so — see [Coverage](#coverage) for exactly where those edges are.

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

The fixture is a curated estate — one honest example of every resource shape
the report knows how to draw, including the awkward ones: a bucket with a size
and no price, an unattached volume, a stopped instance still billing for its
storage, a function on a retired runtime, a load balancer with no targets, and
rows where a field is simply absent because AWS never reported it. To see how
the report behaves on a large account, `--demo-scale N` grows it to N resources
by generating extras that follow the same distribution. It is deterministic —
the same N always produces the same census — and it is refused without `--demo`,
because a real scan reports only what AWS returned.

## Usage

```sh
blueprint scan                          # scan all enabled regions of the current account
blueprint scan --profile prod           # use a specific AWS shared-config profile
blueprint scan --regions us-east-1,eu-west-1
blueprint scan --org                    # scan all AWS Organizations member accounts
blueprint scan --org --role-name blueprint-readonly
blueprint scan --concurrency 4          # max concurrent AWS API scan units (default 8)
blueprint scan --formats html,json,csv  # choose outputs (default: html,json)
blueprint scan --out ./reports          # directory for output files (default: .)
blueprint scan --no-open                # don't open the HTML report in the browser
blueprint scan --compare last.json      # diff against a specific census JSON instead of history
blueprint scan --fail-on-change         # non-zero exit when the diff finds differences
blueprint scan --no-history             # don't archive this scan or auto-diff
blueprint scan --demo                   # render from fixture data, no AWS calls
blueprint scan --demo --demo-scale 20000  # grow the fixture to an estate of that size
blueprint scan --costs                  # also report last month's spend (AWS bills $0.01/request)
blueprint scan --costs --cost-resources # also ask what AWS billed each resource (AWS bills $0.01/request, at least one per service)
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

Scans are bucketed by scope — accounts, regions, *and* the set of services
scanned — so scanning a different account or region set never diffs against the
wrong baseline. Adding a scanner therefore starts a fresh bucket by design,
rather than reporting a wider census as an estate that suddenly grew. Cost and
metrics flags are deliberately not part of the scope, so turning them on never
re-buckets anything. Each scope keeps its last 30 censuses; history lives on
your disk and nowhere else.

## Coverage

blueprint enumerates 22 resource types. Eleven scanners produce them, and they
are reported under fourteen service names, because the shared RDS control plane
answers for RDS, Aurora, DocumentDB and Neptune alike. A "total view"
has real per-service limits, and they belong in a table rather than in your
inbox as a bug report. Every cell below describes what shipped.

| Resource type | Enumerated | Savings suggestions | Cost Explorer rollup line |
| --- | --- | --- | --- |
| `AWS::RDS::DBInstance` | every standalone instance <sup>1</sup> | `RdsDbInstance`, plus `RdsDbInstanceStorage` for storage alone | Amazon Relational Database Service |
| `AWS::RDS::DBCluster` (Aurora) | every cluster | `AuroraDbClusterStorage` — **storage only** | Amazon Relational Database Service |
| `AWS::DocDB::DBCluster` | every cluster | `DocumentDBCluster` | Amazon DocumentDB (with MongoDB compatibility) |
| `AWS::DocDB::DBInstance` | standalone only <sup>1</sup> | — | Amazon DocumentDB (with MongoDB compatibility) |
| `AWS::Neptune::DBCluster` | every cluster | — | Amazon Neptune |
| `AWS::Neptune::DBInstance` | standalone only <sup>1</sup> | — | Amazon Neptune |
| `AWS::DynamoDB::Table` | every table | `DynamoDBTable` | Amazon DynamoDB |
| `AWS::ElastiCache::ReplicationGroup` | every group | `ElastiCacheCluster` | Amazon ElastiCache |
| `AWS::ElastiCache::CacheCluster` | standalone only <sup>2</sup> | `ElastiCacheCluster` | Amazon ElastiCache |
| `AWS::ElastiCache::ServerlessCache` | every cache | — | Amazon ElastiCache |
| `AWS::Redshift::Cluster` | every cluster | — | Amazon Redshift |
| `AWS::RedshiftServerless::Workgroup` | every workgroup | — | Amazon Redshift |
| `AWS::EC2::Instance` | every instance, stopped included | `Ec2Instance` | Amazon Elastic Compute Cloud - Compute · EC2 - Other |
| `AWS::EC2::Volume` | every volume, attached or not | `EbsVolume` | Amazon Elastic Block Store · EC2 - Other |
| `AWS::EC2::Snapshot` | this account's own <sup>3</sup> | — | Amazon Elastic Block Store · EC2 - Other |
| `AWS::Lambda::Function` | every function | `LambdaFunction` | AWS Lambda |
| `AWS::S3::Bucket` | every bucket, listed per region <sup>4</sup> | — | Amazon Simple Storage Service |
| `AWS::EC2::NatGateway` | every gateway | `NatGateway` | Amazon Virtual Private Cloud · EC2 - Other |
| `AWS::EC2::EIP` | every allocated address | — | EC2 - Other |
| `AWS::EC2::NetworkInterface` | only those holding a billable public IPv4 <sup>5</sup> | — | EC2 - Other |
| `AWS::ElasticLoadBalancingV2::LoadBalancer` | every Application, Network and Gateway load balancer | — | Amazon Elastic Load Balancing |
| `AWS::ElasticLoadBalancing::LoadBalancer` | every Classic load balancer | — | Amazon Elastic Load Balancing |

**Per-resource spend** is not in the table because it has no per-row answer:
every service above is asked (`--cost-resources`), because AWS's own
documentation contradicts itself about which services answer. What came back
is recorded per service, per run, in the report — blueprint asking is not AWS
replying, and the report shows which it was.

**Savings suggestions** — Cost Optimization Hub (`--costs`), in AWS's own
resource-type names. These are the types the hub models; the list is AWS's and
is reproduced here rather than encoded in blueprint, which attaches whatever
comes back by exact ARN and says of every other resource that the hub had no
suggestion — which is a statement about advice, not about cost, and never
lowers or raises a price. Twelve rows above have no suggestion type; the
notable absences are **S3 and load balancers**, which the hub does not model at
all.

**Cost Explorer rollup line** — the service line (`--costs`) a row's spend
lands on when there is no per-resource figure. *EC2 - Other* is Cost Explorer's
catch-all, carrying EBS, NAT gateway data processing and idle public IPv4
charges, which is why several rows name it beside their own service.

In an account that has not enabled resource-level data, every row is
**rollup-only**: you get the service total and the inventory, and no
per-resource number is invented to fill the gap. Savings suggestions are
unaffected — they are free, they come from a different API, and they are not a
price, so they arrive whether or not anything on the page has one.

<sup>1</sup> RDS, DocumentDB and Neptune cluster members are represented by
their cluster and never listed twice. DocumentDB and Neptune are clustered in
practice, so those instance rows exist for the unclustered case and are usually
empty.
<sup>2</sup> Cache clusters belonging to a replication group are represented by
the group.
<sup>3</sup> Snapshots and AMIs are scoped to `self`; unscoped,
`DescribeSnapshots` returns every public snapshot in the region. AMIs are read
so an instance can name its image, but they are not census rows.
<sup>4</sup> S3's namespace is global; buckets are listed per region through the
bucket-region filter, so each bucket is scanned by the region it lives in and
appears exactly once.
<sup>5</sup> AWS bills every public IPv4 address, so the census is one row per
billable address — see below.

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

Cost arrives in three tiers. All of them are opt-in, they answer different
questions, and blueprint never adds figures from different tiers together.
Two of them are money AWS charged; the third is money AWS thinks you could
stop being charged, and it is kept out of every total on the page.

| Tier | Flag | Source | Answers | Price |
| --- | --- | --- | --- | --- |
| Service and account rollups | `--costs` | Cost Explorer `GetCostAndUsage` | what the last closed month cost, by service and by account | **$0.01 per request**; a normal run is two |
| Per-resource billed spend | `--costs --cost-resources` | Cost Explorer `GetCostAndUsageWithResources` | what AWS billed each resource over the last 14 days | **$0.01 per request** — at least one per service probed, and one per page when an answer paginates; needs a paid account preference |
| Savings suggestions | `--costs` | Cost Optimization Hub | what you could stop paying, per resource, if you took an action | free; needs a one-off enrollment |

Only the first works everywhere. The second needs an account-level preference
that is itself paid, and AWS's guarantees for it start and end at EC2. The
third needs Cost Optimization Hub switched on and covers the resource types AWS
models — see [Coverage](#coverage) — and it is not a third price: a suggestion
is a modelled saving from an action nobody has taken yet.

There is deliberately no fourth tier, and in particular **no price computed
from the AWS Pricing API**. List price is not spend. Reserved Instances,
Savings Plans, enterprise agreements and private pricing routinely move the
real figure by large factors, and always downward from list — so a rate-card
estimate is wrong in precisely the direction that makes an estate look more
expensive than it is, which is the direction that gets things deleted. A number
like that is worse than no number, because it survives being copied into a
slide. blueprint reports what AWS billed, or reports that it has nothing.

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
add `--cost-resources`, which bills at least one request per service (see
below).
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

### Amortized or unblended?

`--cost-metric` decides how commitments are spread, and it changes what the
same instance appears to cost. **Amortized** (the default) spreads the upfront
and recurring portions of Reserved Instances and Savings Plans across the hours
they covered, so a fully-committed instance shows what it consumes.
**Unblended** is cash as AWS charged it that month: commitment charges stay on
the account instead of on the resources they covered, so the same
fully-committed instance shows **$0** — not because it is free, but because it
was paid for in advance and no line item lands against it this month. Both are
true; they answer *what does this cost to run* and *what did we pay this
month*. There is no per-resource commitment flag in the report, because AWS
publishes none — instead the report states the consequence in words beside
every figure, keyed to the metric that produced it: amortized is the right
signal for ranking what a resource costs and the wrong one for reading a
saving, because the commitment is already bought and deleting a covered row
does not return the amount shown. At the account level the fees themselves are
visible as their own unattributed record types, never folded into a service.
`BlendedCost` is not offered at all: it averages a rate across an
organization's accounts, so a per-account blended figure is a chargeback
artifact rather than what that account cost.

A month stays estimated for a few days after it closes. When AWS flags the data
that way, the census records it and the run says so, because estimated figures
still move and will not reconcile to the invoice yet.

Cost is deliberately invisible to history bucketing and to the resource diff,
so turning `--costs` on or off never re-buckets your history or reports an
unchanged estate as drifted.

### Per-resource billed spend

`--cost-resources` asks Cost Explorer what it actually **billed** each
individual resource over the last 14 days. It needs `--costs`, because it takes
its list of services to ask about from the account rollup:

```sh
blueprint scan --costs --cost-resources
```

```
  … cost-resources: asking Cost Explorer what it billed per resource over 2026-07-17→2026-07-30 — at least one billed request per service, more if a service's answer paginates, 18 left in this run's budget
  ✓ cost-resources: 7 service(s) probed, 52 resource(s) priced, 7 request(s), ~$0.07 charged by AWS
```

**This one bills per service, not per run.** Each service probed is at least
one $0.01 request — a service whose answer paginates costs one per page —
drawn from the same `--cost-max-requests` allowance as the rollup, so the
rollup spends first and the overlay gets whatever is left.
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

Figures carry the method `ce`, and it is the only method a per-resource amount
has ever been written with in this schema — one basis for money, so a column of
figures is a column of comparable ones. Where a resource also has a savings
suggestion, that lands in its own fields and never beside the spend as a
second price.

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

### Savings suggestions

`--costs` also asks **Cost Optimization Hub** what you could stop paying. That
API is free, so it adds nothing to the bill above:

```
  … cost-hub: reading savings suggestions from Cost Optimization Hub — not billed by AWS
  ✓ cost-hub: 38 resource(s) with suggestions from 41 recommendation(s) in 3 call(s), no charge
```

They render in their own section, ranked by modelled monthly saving, each with
the action AWS suggests, how much work it grades that action, and whether it
needs a restart:

```
  ── ways to cut this bill ── AWS Cost Optimization Hub  ·  modelled monthly, never billed
    ranked by modelled monthly saving (USD)
      1310.72 USD  Delete gp3  ·  vol-0d4e5f6a7b8c90093 (ebs, us-east-1)  ·  very low effort  ·  no restart  ·  not reversible
       481.00 USD  Rightsize db.m6g.2xlarge → db.m6g.xlarge  ·  telemetry-tsdb (rds, us-west-2)  ·  medium effort  ·  restart needed  ·  reversible
      … and 39 more (full list in the JSON and CSV output)
      Σ 4627.51 USD across 44 suggestion(s), if every one were acted on
    ⓘ modelled by AWS from recent usage, not amounts you were charged; nothing here is added to or subtracted from the spend above
```

**A saving is not a price.** The hub reports what a resource would cost *after*
a change you have not made, and the figure blueprint publishes is the
difference — the part that would go away. There is exactly one money basis in
the report and it is Cost Explorer's. Nothing here is added to, subtracted
from, or reconciled against the spend above; the Σ line totals the suggestions
themselves and says out loud that it assumes every one of them was acted on,
which nobody has. Deleting something does not make the fortnight it already
cost disappear, and only a later scan can say what actually changed.

Savings in different currencies are never summed — each currency gets its own
ranking and its own total, and suggestions whose currency AWS did not report
get a bucket of their own rather than being called USD. A suggestion AWS gave
with no savings figure at all is still shown, still counted, and its saving
column is left empty rather than filled with `0`; a reported `0.00` is a
different thing — a change worth making that saves nothing this month — and
survives as itself.

Coverage is partial by design. The hub only models resource types it has
recommendations for — the *Savings suggestions* column in
[Coverage](#coverage) is AWS's list, and S3 and load balancers are not on it —
which is why the run reports how many resources got suggestions against how
many recommendations were read. blueprint maintains no coverage list of its
own: it attaches what came back by exact ARN, and a resource with no suggestion
is a resource the hub said nothing about, not a resource that costs nothing.
"The hub does not model this type" and "the hub had nothing to say about this
one" are not distinguishable from the response, and guessing between them would
be this tool asserting something AWS did not.

Where AWS modelled a recommendation over a window the resource did not exist
for all of, that is recorded as a caveat on the suggestion. Recommendations
that name no resource — Savings Plans and Reserved Instance purchases, which
are account-level commitments rather than changes to a thing in the census —
have nothing to attach to and are ledgered instead of being pinned to an
arbitrary row.

Cost Optimization Hub has to be switched on, once, from its console — it is
free and takes about a day to produce the first recommendations. Until then an
unenrolled account returns an empty list that is indistinguishable from "no
recommendations", so blueprint checks enrollment first and puts the answer in
the failure ledger rather than reporting a silent absence of advice.

## Metrics

`--metrics` adds CloudWatch readings the describe APIs do not carry — today,
free storage space on RDS instances, and bucket size and object count on S3
buckets, which the S3 control plane does not report at all.

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
series that do not exist are never paid for. Ten thousand metrics cost ten
cents.

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
- **HTML**: a single self-contained file (`blueprint-YYYY-MM-DD.html`) you can open in a browser or attach to a doc. No external assets, no CDN calls. Past 5,000 resources the inventory is compressed into the page and unpacked by the browser — a 50,000-resource census lands under three megabytes instead of about nineteen — which needs Chrome or Edge 80+, Firefox 113+, or Safari 16.4+. Smaller reports stay plain text in the file and open anywhere. Separately, past 500 resources the inventory opens grouped by service and collapsed, so the first screen is a few dozen rows rather than the whole estate.
- **JSON**: the complete snapshot (`blueprint-YYYY-MM-DD.json`) — every resource, plus the failure ledger, plus the cost report when `--costs` is on.
- **CSV**: one row per resource (`blueprint-YYYY-MM-DD.csv`) for spreadsheets. The columns are the narrow core and stay fixed as new services land; attributes and measures ride in one `k=v;k=v` cell. With `--costs`, billed spend and savings suggestions get separate closed blocks of columns after it — `cost_ce_*` and `tip_*` — so a spend column stays summable and a saving is never mixed into it. A resource with several suggestions shows the largest and counts the rest in `tip_count`; the full set is in the JSON.

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
- A role must exist in **every member account** carrying the `BlueprintReadOnly` statement above (plus `BlueprintCloudWatchMetrics` if you use `--metrics`, since CloudWatch is read through the assumed role), and its trust policy must allow the calling account to assume it. The two Cost Explorer and Cost Optimization Hub statements are *not* needed on member roles — those APIs are only ever called with the caller's own credentials. The default role name is `OrganizationAccountAccessRole` (created automatically for accounts made through Organizations); override with `--role-name`.
- The caller needs everything in the single-account policy — its own account is scanned with its own credentials, not through a role — plus `organizations:ListAccounts` and `sts:AssumeRole` on the member-account roles. That whole policy is [docs/iam-policy-org.json](docs/iam-policy-org.json); replace `${RoleName}` with your actual role name.
- With `--costs`, Cost Explorer is queried once from the calling account for the whole organization, not once per member account — so `ce:GetCostAndUsage` belongs on the caller, and the bill stays the same two requests no matter how many accounts you scan. `--cost-resources` is org-wide in the same way, filtered to the accounts actually scanned, so `ce:GetCostAndUsageWithResources` belongs on the caller too and its bill scales with the number of *services* probed rather than the number of accounts. Cost Optimization Hub works the same way: one org-wide list read with the caller's credentials, so `cost-optimization-hub:ListEnrollmentStatuses` and `cost-optimization-hub:ListRecommendations` belong on the caller too — those two actions and no others. Member accounts are only included if the organization enrolled them, and blueprint says so in the ledger when it finds they were not.

Accounts where the role is missing or untrusting do not abort the scan: they show up as failures in the ledger, and everything else is still scanned.

## Zero telemetry

blueprint phones home to no one. No usage analytics, no crash reporting, no update checks, not even anonymous pings. The only network calls it makes are to AWS APIs, using the credentials you provide. Output files are written to your local disk and go nowhere unless you send them somewhere.

Every one of those calls is a describe/list/get — blueprint never creates, modifies, or deletes anything in your account, including your Cost Explorer or Cost Optimization Hub preferences — blueprint reads your Cost Optimization Hub enrollment status but never changes it. Three of them are billed by AWS: `ce:GetCostAndUsage` behind `--costs`, `ce:GetCostAndUsageWithResources` behind `--cost-resources`, and `cloudwatch:GetMetricData` behind `--metrics`. All three flags are off by default, each prints the rate before spending anything, and each reports what was actually spent when the run ends.

## License

MIT © [hoop.dev](https://hoop.dev) — built by the team behind [hoop](https://github.com/hoophq/hoop). See [LICENSE](LICENSE).
