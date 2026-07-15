# dbcensus

Past a few hundred resources, nobody has ground truth on their databases anymore: instances accumulate across regions, accounts, and teams faster than any spreadsheet or wiki page keeps up. dbcensus is a read-only census of every managed database reachable from your AWS credentials. It runs locally on your machine, calls only AWS APIs, and writes its output (terminal summary, HTML report, JSON, CSV) to your local disk. Nothing leaves your machine.

## Quickstart

Homebrew (available with the v0.1 release):

```sh
brew install hoophq/tap/dbcensus
```

Install script (available with the v0.1 release):

```sh
curl -fsSL https://dbcensus.hoop.dev/install.sh | sh
```

From source (Go 1.26+; works today):

```sh
go install github.com/hoophq/dbcensus@main
```

Then, with AWS credentials available (env vars, `~/.aws` profile, SSO — the standard chain):

```sh
dbcensus scan
```

No credentials handy? See what the output looks like with built-in fixture data:

```sh
dbcensus scan --demo
```

## Usage

```sh
dbcensus scan                          # scan all enabled regions of the current account
dbcensus scan --profile prod           # use a specific AWS shared-config profile
dbcensus scan --regions us-east-1,eu-west-1
dbcensus scan --org                    # scan all AWS Organizations member accounts
dbcensus scan --org --role-name dbcensus-readonly
dbcensus scan --formats html,json,csv  # choose outputs (default: html,json)
dbcensus scan --out ./reports          # directory for output files (default: .)
dbcensus scan --demo                   # render from fixture data, no AWS calls
```

## What gets scanned

- RDS (all engines)
- Aurora (MySQL and PostgreSQL clusters)
- DocumentDB
- Neptune
- DynamoDB
- ElastiCache (Redis, Valkey, Memcached)
- Redshift, including Redshift Serverless

Every resource is normalized into one model: engine, version, instance class, storage, endpoint host, status, region, account, creation time, tags. Environment and owner are taken from tags only — imported, never inferred.

## Outputs

- **Terminal**: a sprawl summary — total databases, distinct engines/regions/accounts, a per-service breakdown, and counts of resources with no owner or environment tag.
- **HTML**: a single self-contained file (`dbcensus-YYYY-MM-DD.html`) you can open in a browser or attach to a doc. No external assets, no CDN calls.
- **JSON**: the complete snapshot (`dbcensus-YYYY-MM-DD.json`) — every resource, plus the failure ledger.
- **CSV**: one row per resource (`dbcensus-YYYY-MM-DD.csv`) for spreadsheets.

## Required IAM permissions

dbcensus needs read-only describe/list permissions. The minimal policy ([docs/iam-policy.json](docs/iam-policy.json)):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DbcensusReadOnly",
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
        "ec2:DescribeRegions",
        "sts:GetCallerIdentity"
      ],
      "Resource": "*"
    }
  ]
}
```

The AWS managed policies `ReadOnlyAccess` or `SecurityAudit` also cover everything dbcensus calls, if you already have one of those attached.

### Org mode

`dbcensus scan --org` enumerates all ACTIVE accounts in your AWS Organization and scans each one by assuming a role in it.

Requirements:

- Run it with credentials from the organization's **management account** or a **delegated administrator** account, with `organizations:ListAccounts` allowed.
- A role with the read-only policy above must exist in **every member account**, and its trust policy must allow the calling account to assume it. The default role name is `OrganizationAccountAccessRole` (created automatically for accounts made through Organizations); override with `--role-name`.
- The caller additionally needs `organizations:ListAccounts` and `sts:AssumeRole` on the member-account roles — see [docs/iam-policy-org.json](docs/iam-policy-org.json), replacing `${RoleName}` with your actual role name.

Accounts where the role is missing or untrusting do not abort the scan: they show up as failures in the ledger, and everything else is still scanned.

## Honest coverage

dbcensus maps every managed database reachable from the AWS credentials you give it — nothing more, and it tells you what it couldn't see. Every scan unit (account × region × service) that fails — access denied, missing role, throttling that outlasted retries — is recorded in a failure ledger shown in the terminal summary and included in the JSON and HTML outputs (the CSV contains resource rows only; pair it with the JSON when coverage matters). A census that silently skips what it can't reach isn't a census.

## Zero telemetry

dbcensus phones home to no one. No usage analytics, no crash reporting, no update checks, not even anonymous pings. The only network calls it makes are to AWS APIs, using the credentials you provide. Output files are written to your local disk and go nowhere unless you send them somewhere.

## License

MIT — see [LICENSE](LICENSE).
