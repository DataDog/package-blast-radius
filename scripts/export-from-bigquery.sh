#!/bin/bash
#
# Export the npm dependency graph from the deps.dev BigQuery public dataset.
# Produces parquet files in a GCS bucket, then downloads them locally.
#
# Prerequisites:
#   - gcloud CLI authenticated: gcloud auth login --update-adc
#   - A GCP project with BigQuery API enabled
#
# Every billed query is priced with a dry run and confirmed before it runs.
#
# The bucket is optional: it defaults to gs://<project>-blast-radius and is
# created if it does not exist.
#
# Usage:
#   ./export-from-bigquery.sh [-y] [--force] <gcp-project> [gcs-bucket] [snapshot-date]
#
# Examples:
#   ./export-from-bigquery.sh my-project
#   ./export-from-bigquery.sh my-project gs://my-bucket 2026-03-23

set -euo pipefail

AUTO_YES=false
FORCE=false
POSITIONAL=()

while [ $# -gt 0 ]; do
  case "$1" in
    -y|--yes)   AUTO_YES=true; shift ;;
    --force)    FORCE=true; shift ;;
    -h|--help)  sed -n '3,20p' "$0" | cut -c3-; exit 0 ;;
    -*)         echo "Unknown option: $1" >&2; exit 2 ;;
    *)          POSITIONAL+=("$1"); shift ;;
  esac
done

if [ ${#POSITIONAL[@]} -lt 1 ]; then
  echo "Usage: $0 [-y] [--force] <gcp-project> [gcs-bucket] [snapshot-date]" >&2
  exit 2
fi

PROJECT="${POSITIONAL[0]}"
BUCKET="${POSITIONAL[1]:-}"
SNAPSHOT_DATE="${POSITIONAL[2]:-}"

# The bucket is optional, so "<project> 2026-03-23" means the date, not a bucket.
if [[ "$BUCKET" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] && [ -z "$SNAPSHOT_DATE" ]; then
  SNAPSHOT_DATE="$BUCKET"
  BUCKET=""
fi
BUCKET="${BUCKET:-gs://${PROJECT}-blast-radius}"

# Keep '.' as the decimal separator regardless of the caller's locale.
export LC_NUMERIC=C

DATASET="blast_radius"
TABLE="npm_edges"
# bq extract requires the bucket and the dataset to share a region, and the
# deps.dev public dataset lives in the US multi-region.
LOCATION="US"
PRICE_PER_TIB=6.25

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

confirm() {
  local prompt="$1"
  if $AUTO_YES; then
    echo "$prompt [auto-confirmed with -y]"
    return 0
  fi
  if [ ! -t 0 ]; then
    echo "$prompt" >&2
    echo "Not running interactively; re-run with -y to confirm." >&2
    exit 1
  fi
  read -r -p "$prompt [y/N] " answer
  case "$answer" in
    [yY]|[yY][eE][sS]) return 0 ;;
    *) echo "Aborted."; exit 1 ;;
  esac
}

# price_and_confirm <label> <sql> — dry-runs the query and asks before billing.
price_and_confirm() {
  local label="$1" sql="$2" bytes

  # A failed dry run must fall through to the prompt below, not abort the
  # script, so pipefail's status is deliberately discarded here.
  bytes=$(bq query \
    --project_id="$PROJECT" \
    --use_legacy_sql=false \
    --dry_run \
    --format=json \
    "$sql" 2>/dev/null \
    | sed -n 's/.*"totalBytesProcessed": *"\([0-9]*\)".*/\1/p' | head -1) || true

  if [ -z "$bytes" ]; then
    echo "Could not determine the size of the $label query (dry run gave no byte count)."
    confirm "Run it anyway, without knowing the cost?"
    return 0
  fi

  awk -v b="$bytes" -v p="$PRICE_PER_TIB" -v l="$label" \
    'BEGIN { t = b / 1099511627776; printf "%s query will scan %.2f TiB, costing ~$%.2f (on-demand, $%.2f/TiB).\n", l, t, t * p, p }'
  confirm "Proceed?"
}

if [ -z "$SNAPSHOT_DATE" ]; then
  echo "=== Finding latest snapshot date ==="
  LATEST_SQL="SELECT FORMAT_DATE('%Y-%m-%d', DATE(MAX(SnapshotAt))) as d
     FROM \`bigquery-public-data.deps_dev_v1.DependencyGraphEdges\`
     WHERE SnapshotAt >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 14 DAY)"

  price_and_confirm "Snapshot discovery" "$LATEST_SQL"

  SNAPSHOT_DATE=$(bq query \
    --project_id="$PROJECT" \
    --use_legacy_sql=false \
    --format=csv \
    --max_rows=1 \
    "$LATEST_SQL" \
    | tail -1)
  echo "Latest snapshot: $SNAPSHOT_DATE"
fi

# Everything downstream is scoped by snapshot date so two runs never mix shards.
GCS_PREFIX="${BUCKET}/blast-radius/${SNAPSHOT_DATE}"
OUTDIR="${REPO_ROOT}/data/parquet/${SNAPSHOT_DATE}"

echo "=== Step 1: Bucket ==="
if bucket_location=$(gcloud storage buckets describe "$BUCKET" \
      --project="$PROJECT" --format="value(location)" 2>/dev/null); then
  echo "Using existing bucket $BUCKET (location: $bucket_location)"
  if [ "$bucket_location" != "$LOCATION" ]; then
    echo "Error: bq extract needs a $LOCATION bucket, but $BUCKET is in $bucket_location." >&2
    echo "Pass a $LOCATION bucket, or omit the argument to create one." >&2
    exit 1
  fi
else
  echo "Bucket $BUCKET does not exist."
  confirm "Create it in $LOCATION?"
  gcloud storage buckets create "$BUCKET" --project="$PROJECT" --location="$LOCATION"
fi

# Stale objects under the export prefix would be picked up by the wildcard
# download and silently merged into the local dataset.
if existing=$(gcloud storage ls "${GCS_PREFIX}/**" 2>/dev/null) && [ -n "$existing" ]; then
  echo "Error: $GCS_PREFIX already contains objects:" >&2
  echo "$existing" | head -5 >&2
  if ! $FORCE; then
    echo "Re-run with --force to delete them first." >&2
    exit 1
  fi
  confirm "Delete everything under $GCS_PREFIX?"
  gcloud storage rm --recursive "$GCS_PREFIX"
fi

echo "=== Step 2: Create dataset ==="
bq --project_id="$PROJECT" --location="$LOCATION" mk -d "$DATASET" 2>/dev/null || true

echo "=== Step 3: Run query (snapshot: $SNAPSHOT_DATE) ==="
EDGES_SQL="
  SELECT Name, Version, \`To\`.Name AS DepName, Requirement
  FROM \`bigquery-public-data.deps_dev_v1.DependencyGraphEdges\`
  WHERE DATE(SnapshotAt) = '$SNAPSHOT_DATE'
    AND System = 'NPM'
    AND \`From\`.Name = Name AND \`From\`.Version = Version
  "

echo "Snapshot: $SNAPSHOT_DATE   Destination: ${PROJECT}:${DATASET}.${TABLE}"
price_and_confirm "Edge export" "$EDGES_SQL"

bq query \
  --project_id="$PROJECT" \
  --use_legacy_sql=false \
  --destination_table="${PROJECT}:${DATASET}.${TABLE}" \
  --replace \
  --max_rows=0 \
  "$EDGES_SQL"

echo "=== Step 4: Export to GCS ==="
bq extract \
  --project_id="$PROJECT" \
  --destination_format=PARQUET \
  --compression=SNAPPY \
  "${PROJECT}:${DATASET}.${TABLE}" \
  "${GCS_PREFIX}/npm-edges-*.parquet"

echo "=== Step 5: Download parquet files ==="
mkdir -p "$OUTDIR"
gcloud storage cp "${GCS_PREFIX}/npm-edges-*.parquet" "$OUTDIR/"

echo "=== Done ==="
echo "Parquet files downloaded to: $OUTDIR"
echo "File count: $(find "$OUTDIR" -name 'npm-edges-*.parquet' | wc -l | tr -d ' ')"
echo "Total size: $(du -sh "$OUTDIR" | cut -f1)"
echo ""
echo "Next step: ./build-duckdb.sh $OUTDIR"
