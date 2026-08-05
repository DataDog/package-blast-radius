# blast-radius

Find all public packages affected by a compromised npm dependency.

Given a package name and version (even if yanked), finds all packages whose declared version range **could resolve** to it. This project uses a local snapshot of the [deps.dev](https://deps.dev) dependency graph made available for Google through a BigQuery public dataset.

```
$ blast-radius analyze npm @asyncapi/generator 3.3.1 --db ./data/npm-deps-old-removeme.duckdb --enrich-with-download-count --depth 2

Computing blast radius for @asyncapi/generator@3.3.1 (NPM), depth=2

Depth 1: querying dependents of 1 package(s)...
Depth 1: got 1459 edges, filtering by version range...
Depth 1: found 89 new affected packages
Depth 2: querying dependents of 8 package(s)...
Depth 2: got 93 edges, filtering by version range...
Depth 2: found 93 new affected packages

Total: 182 affected versions (10 unique packages)
Enriching 10 unique packages with download counts...

Blast radius for @asyncapi/generator@3.3.1 (NPM)
Total dependency edges scanned: 1,552
Affected: 182 versions (10 unique packages)
Max depth: 2 | Took 1.2s

PACKAGE                                                      VERSION  DEPTH  DOWNLOADS/wk  PATH
@asyncapi/cli                                                5.0.5    1      55,458        @asyncapi/cli@5.0.5 ──(^3.0.1)──▶ @asyncapi/generator@3.3.1
@powerlines/plugin-asyncapi                                  0.1.646  2      3,116         @powerlines/plugin-asyncapi@0.1.646 ──(^0.0.59)──▶ @power-plant/asyncapi@0.0.59 ──(^3.3.0)──▶ @asyncapi/generator@3.3.1
@power-plant/asyncapi                                        0.0.29   1      2,200         @power-plant/asyncapi@0.0.29 ──(^3.3.0)──▶ @asyncapi/generator@3.3.1
@bymbly/api-tools                                            1.4.63   2      550           @bymbly/api-tools@1.4.63 ──(6.0.2)──▶ @asyncapi/cli@6.0.2 ──(^3.2.0)──▶ @asyncapi/generator@3.3.1
@nmime/nestjs-asyncapi                                       2.0.5    1      327           @nmime/nestjs-asyncapi@2.0.5 ──(^3.1.0)──▶ @asyncapi/generator@3.3.1
asyncapi-mcp-server                                          3.0.0    1      8             asyncapi-mcp-server@3.0.0 ──(^3.2.1)──▶ @asyncapi/generator@3.3.1
@asyncapi-actions-test/trusted-publishing-test_asyncapi-cli  5.3.0    1      3             @asyncapi-actions-test/trusted-publishing-test_asyncapi-cli@5.3.0 ──(^3.0.1)──▶ @asyncapi/generator@3.3.1
@achinet/nestjs-async                                        0.2.0    1      2             @achinet/nestjs-async@0.2.0 ──(^3.1.2)──▶ @asyncapi/generator@3.3.1
@leandrose/project-documentation                             0.2.1    1      1             @leandrose/project-documentation@0.2.1 ──(^3.2.1)──▶ @asyncapi/generator@3.3.1
trusted-publishing-test_asyncapi-cli                         4.1.3    1      1             trusted-publishing-test_asyncapi-cli@4.1.3 ──(^3.0.1)──▶ @asyncapi/generator@3.3.1

Saved to output/2026-08-05_160650/
  blast-radius.json      feeds `blast-radius visualize`
  affected-packages.csv  10 packages
  paths.csv              182 rows
```


## How it works

1. Queries a local DuckDB database containing all npm direct dependency edges (package → dependency + version range)
2. Filters edges where the declared semver range includes the target version (e.g., `^1.6.1` matches `1.14.1`)
3. Optionally enriches results with weekly download counts from the npm API (`--enrich-with-download-count`, off by default)
4. Outputs sorted by impact (downloads), and saves the full results to `output/<timestamp>/`

This works even for **yanked/removed versions** because we check the declared range, not what the registry currently resolves to.

## Setup

### Prerequisites

- Go 1.25+
- [DuckDB CLI](https://duckdb.org/docs/installation/): `brew install duckdb`
- For data export: `gcloud` CLI with BigQuery access

### Build the CLI

```bash
make            # builds bin/blast-radius
make test       # run the unit tests
make vet        # run go vet
make clean      # remove bin/
```

A single binary provides both subcommands:

| command | purpose |
| --- | --- |
| `blast-radius analyze` | compute the blast radius of one or more compromised versions |
| `blast-radius visualize` | browse a generated JSON report in a local web UI |

### Get the data

The dependency graph snapshot comes from the [deps.dev BigQuery public dataset](https://docs.deps.dev/bigquery/v1/). Export it once:

```bash
# Step 1: Export from BigQuery to parquet files.
# Prices the query with a dry run and asks before spending anything.
# The bucket is optional and defaults to gs://<project>-blast-radius, created if missing.
./scripts/export-from-bigquery.sh <your-gcp-project> [gs://your-bucket] [snapshot-date]

# Step 2: Build the indexed DuckDB database from the path the export printed
./scripts/build-duckdb.sh data/parquet/<snapshot-date>
```

This produces `data/npm-deps.duckdb` (~19 GB, 419M edges covering all of npm).

## Usage

### `analyze`

```bash
# Basic usage (looks for npm-deps.duckdb in ./data/)
./bin/blast-radius analyze npm axios 1.14.1

# Specify database path
./bin/blast-radius analyze npm axios 1.14.1 --db data/npm-deps.duckdb

# Rank by weekly download counts (slow, ~3min vs ~20s, requires network)
./bin/blast-radius analyze npm axios 1.14.1 --enrich-with-download-count

# Transitive dependents (depth 2 = packages that depend on packages that depend on axios)
./bin/blast-radius analyze npm axios 1.14.1 --depth 2

# Multiple compromised versions
./bin/blast-radius analyze npm axios --versions 1.14.1,0.30.0

# Many compromised packages at once via a CSV.
# Each line: package_name;version1,version2,...
# Lines starting with '#' and blank lines are ignored.
cat > compromised.csv <<'EOF'
axios;1.14.1,0.30.4
lodash;4.17.20,4.17.21
EOF
./bin/blast-radius analyze npm --csv compromised.csv --depth 3

# An affected-packages.csv from a previous run is also valid input, so one
# run's results can become the next run's targets.
./bin/blast-radius analyze npm --csv output/2026-08-05_154305/affected-packages.csv

# Show more/fewer results
./bin/blast-radius analyze npm axios 1.14.1 --top 100
./bin/blast-radius analyze npm axios 1.14.1 --top 0        # show all

# Choose what goes to stdout (the saved files are written either way)
./bin/blast-radius analyze npm axios 1.14.1 --output json
./bin/blast-radius analyze npm axios 1.14.1 --output csv > affected.csv

# Save somewhere specific, or not at all
./bin/blast-radius analyze npm axios 1.14.1 --output-dir /tmp/axios-run
./bin/blast-radius analyze npm axios 1.14.1 --no-save
```

#### Saved results

Every run writes its full results to `output/<YYYY-MM-DD_HHMMSS>/` (gitignored), so nothing is lost when the table is truncated to `--top`:

| file | contents |
| --- | --- |
| `blast-radius.json` | the full report, and the input to `blast-radius visualize` |
| `affected-packages.csv` | `package_name,vulnerable_versions`, one row per unique package |
| `paths.csv` | one row per affected version, with depth, downloads, target and path |

These are large: a depth-1 `axios` run produces ~400 MB of JSON and ~150 MB of CSV.

### `visualize`

A local web UI for exploring large JSON outputs. It provides filtering, sorting, pagination, CSV export, and a visual dependency path graph for each package.

```bash
# Every analyze run already saves the JSON the viewer needs
./bin/blast-radius analyze npm axios 1.14.1 --depth 2
./bin/blast-radius visualize output/2026-08-05_154305/blast-radius.json

# Opens at http://127.0.0.1:8080; override with --port
```

The server loads and deduplicates the JSON on startup (handles files up to ~1 GB), then serves a paginated API to the browser. Click any package name to see the full dependency path to the compromised target.

## Refreshing the data

The snapshot is a point-in-time export from the [deps.dev BigQuery public dataset](https://docs.deps.dev/bigquery/v1/).

### Option A: Automated (script)

```bash
# Query BigQuery, save to a table, export to GCS, download the parquet shards.
# Add -y to skip the cost confirmation, --force to overwrite a previous export
# of the same snapshot date.
./scripts/export-from-bigquery.sh <your-gcp-project>
./scripts/build-duckdb.sh data/parquet/<snapshot-date>
```

Both the GCS prefix and the local download directory are scoped by snapshot date, so re-running for a new snapshot never mixes shards from two exports.

### Option B: Manual (BigQuery UI + CLI)

**Step 1** — Run the query in the [BigQuery console](https://console.cloud.google.com/bigquery), saving the result to a destination table (Query Settings > Set a destination table):

```sql
SELECT Name, Version, `To`.Name AS DepName, Requirement
FROM `bigquery-public-data.deps_dev_v1.DependencyGraphEdges`
WHERE DATE(SnapshotAt) = '<snapshot-date>'
  AND System = 'NPM'
  AND `From`.Name = Name AND `From`.Version = Version
```

**Step 2** — Export the table to GCS as sharded parquet (the UI export doesn't support wildcards, so use the CLI):

```bash
bq extract \
  --destination_format=PARQUET \
  --compression=SNAPPY \
  'your-project:your_dataset.your_table' \
  'gs://your-bucket/path/npm-edges-*.parquet'
```

**Step 3** — Download and build the DuckDB database:

```bash
mkdir -p data/parquet/<snapshot-date>
gcloud storage cp 'gs://your-bucket/path/*.parquet' data/parquet/<snapshot-date>/
./scripts/build-duckdb.sh data/parquet/<snapshot-date>
```

### Cost and snapshot dates

This scans ~1.3 TB per snapshot (~$8 at BigQuery's $6.25/TiB rate). The script prices each query with a dry run and asks for confirmation before spending anything, so you see the real figure rather than this estimate. The table is partitioned by `SnapshotAt`, so filtering by date keeps the cost low. Without the date filter, a full scan is ~166 TB.

Find available snapshot dates with:

```sql
SELECT DISTINCT DATE(SnapshotAt) as snapshot_date
FROM `bigquery-public-data.deps_dev_v1.DependencyGraphEdges`
WHERE SnapshotAt >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 30 DAY)
ORDER BY snapshot_date DESC
```
