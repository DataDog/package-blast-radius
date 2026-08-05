# Investigate the blast radius of compromised npm packages

`blast-radius` analyzes the blast radius of a compromised dependency. When a legitimate package gets compromised (for instance, [axios](https://securitylabs.datadoghq.com/articles/axios-npm-supply-chain-compromise/) versions 1.14.1 and 0.30.4), one question is hard to answer: **which npm packages, if installed during the compromise window, would have pulled in the malicious version?**

Give `blast-radius` a package name and version, even a yanked one, and it finds every package whose declared version range could resolve to it. The tool queries a local snapshot of the [deps.dev](https://deps.dev) dependency graph, which Google publishes as a BigQuery public dataset.

## Sample usage

In July 2026, version 3.3.1 of the `@asyncapi/generator` package was compromised. Using `blast-radius`, we can find all transitive dependencies of this package that don't lock versions and could end up installing the malicious package.

```
$ blast-radius analyze npm @asyncapi/generator 3.3.1  --depth 10 --enrich-with-download-count

Computing blast radius for @asyncapi/generator@3.3.1 (NPM), depth=10

Depth 1: querying dependents of 1 package(s)...
Depth 1: scanned 1459 edges, found 89 new affected packages
Depth 2: querying dependents of 8 package(s)...
Depth 2: scanned 93 edges, found 93 new affected packages
Depth 3: querying dependents of 2 package(s)...
Depth 3: scanned 0 edges, found 0 new affected packages

Total: 182 affected versions (10 unique packages)
Enriching 10 unique packages with download counts...

Blast radius for @asyncapi/generator@3.3.1 (NPM)
Total dependency edges scanned: 1,552
Affected: 182 versions (10 unique packages)
Max depth: 10 | Took 1.2s

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
  blast-radius.json      full report
  affected-packages.csv  10 packages
  paths.csv              182 rows

Browse the results with:
  blast-radius visualize output/2026-08-05_160650/blast-radius.json
```

## How it works

1. `blast-radius` pulls the [deps.dev BigQuery dataset](https://docs.deps.dev/bigquery/v1/) into local Parquet files and imports them into a single local DuckDB database, containing all npm direct dependency edges (package → dependency + version range).
2. It queries the database, filtering edges where the declared version range includes the target version (for example, `^1.6.1` matches `1.14.1`).
3. Optionally, it enriches results with weekly download counts from the npm API (`--enrich-with-download-count`, disabled by default).

This works even for **yanked or removed versions**, because `blast-radius` checks the declared range rather than what the registry currently resolves to.

## Setup

### Prerequisites

- Go 1.25+
- The [DuckDB CLI](https://duckdb.org/docs/installation/), installed with `brew install duckdb` on macOS
- A Google Cloud account, authenticated using `gcloud auth login --update-adc`

### Overview of the `blast-radius` CLI

One binary provides all three subcommands:

| command | purpose |
| --- | --- |
| `blast-radius download-data` | export the dependency graph from BigQuery and build the local database |
| `blast-radius analyze` | compute the blast radius of one or more compromised versions |
| `blast-radius visualize` | browse a generated JSON report in a local web UI |

### Step 1: Get the data

The dependency graph snapshot comes from the [deps.dev BigQuery public dataset](https://docs.deps.dev/bigquery/v1/). Download it once with `blast-radius download-data`; you won't need to repeat this for every analysis. It persists around 20 GB of files on your machine, so make sure you have enough disk space available.

`blast-radius download-data`:
- creates a BigQuery table in your Google Cloud project (around 20 GB, expected monthly cost < $1)
- queries the BigQuery table and exports it (one-time cost ~$10)
- exports the data as Parquet files into a Google Cloud Storage (GCS) bucket (around 10 GB, expected monthly cost < $1)
- downloads the Parquet files to your machine
- builds a local DuckDB instance (single, self-contained file) from them
- removes the Parquet files from your machine


The command typically takes 20-30 minutes to complete. Usage:


```bash
blast-radius download-data npm --project <your-gcp-project>
```

Sample output:

```
project dd-security-research   snapshot 2026-08-03 (latest)

[1/4] Bucket
      gs://dd-security-research-blast-radius does not exist
      Create it in US? [y/N] y
      created

[2/4] Export query
      destination  dd-security-research:blast_radius.npm_edges
      scan size    1.39 TiB
      cost         ~$8.69   on-demand, $6.25/TiB
      Proceed? [y/N] y
      query        job_3WAX65RHz6AgedIZTvcpBaUjwfu1  done in 16s
      extract      job_qf_SPET85Tr74pPddnQqmOEg0akh  done in 6s
      wrote        gs://dd-security-research-blast-radius/blast-radius/2026-08-03/npm-edges-*.parquet

[3/4] Download
      1000 shards to data/parquet/2026-08-03
       200/1000    2.1 GB
       ...
      1000/1000   10.1 GB   in 1m12s

[4/4] Build
      data/npm-deps.duckdb from 1000 shards, this takes several minutes
      still building, 30s elapsed
      done in 1m47s

Done in 3m41s
      database     data/npm-deps.duckdb
      rows         419,092,480
      size         18.3 GB
      shards       removed from data/parquet/2026-08-03 (--keep-parquet keeps them)

Next
      blast-radius analyze npm <package> <version>
```

### Step 2: Analyze the data

Sample usage:

```bash
blast-radius analyze npm axios 1.14.1

# Rank by weekly download counts (slower, might hit rate limits if the result count is high)
blast-radius analyze npm axios 1.14.1 --enrich-with-download-count

# Find transitive dependencies up to a depth of 5
blast-radius analyze npm axios 1.14.1 --depth 5

# Multiple compromised versions
blast-radius analyze npm axios --versions 1.14.1,0.30.0

# Many compromised packages at once via a CSV.
cat > compromised.csv <<'EOF'
axios;1.14.1,0.30.4
lodash;4.17.20,4.17.21
EOF
blast-radius analyze npm --csv compromised.csv --depth 3
```

Every run writes its full results to `output/<YYYY-MM-DD_HHMMSS>/`.

| file | contents |
| --- | --- |
| `blast-radius.json` | the full report |
| `affected-packages.csv` | `package_name,vulnerable_versions`, one row per unique package |
| `paths.csv` | one row per affected version, with depth, downloads, target and path |

### Step 3: Visualize the data

Spin up a graphical interface to explore the data:

```bash
blast-radius visualize output/2026-08-05_160650/blast-radius.json
```


## Development

```bash
make            # builds bin/blast-radius
make test       # run the unit tests
make vet        # run go vet
make clean      # remove bin/
```

## Known limitations

### Multi-target path attribution

When analyzing multiple compromised packages or versions in one run, `blast-radius` currently keeps one path per affected package version. If the same package version can reach more than one compromised target, the report records the first matching target and path it finds, and later matching targets are not shown for that package version.

The package version is still reported as affected, but `blast-radius.json` and
`paths.csv` may not list every compromised target it could resolve to.