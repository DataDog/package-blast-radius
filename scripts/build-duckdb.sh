#!/bin/bash
#
# Build an indexed DuckDB database from the exported parquet files.
#
# Prerequisites:
#   - duckdb CLI: brew install duckdb
#   - Parquet files in ../data/parquet/ (from export-from-bigquery.sh)
#
# Produces: ../data/npm-deps.duckdb (~19 GB)
#
# Usage:
#   ./build-duckdb.sh [parquet-dir]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DATA_DIR="$(cd "$SCRIPT_DIR/.." && pwd)/data"
PARQUET_DIR="${1:-$DATA_DIR/parquet}"
DB_PATH="$DATA_DIR/npm-deps.duckdb"

if ! command -v duckdb &>/dev/null; then
  echo "Error: duckdb CLI not found. Install with: brew install duckdb"
  exit 1
fi

if ! ls "$PARQUET_DIR"/npm-edges-*.parquet &>/dev/null; then
  echo "Error: No parquet files found in $PARQUET_DIR"
  echo "Run export-from-bigquery.sh first."
  exit 1
fi

echo "=== Building DuckDB database ==="
echo "Source: $PARQUET_DIR"
echo "Target: $DB_PATH"

rm -f "$DB_PATH"

duckdb "$DB_PATH" -c "
  CREATE TABLE edges AS
  SELECT * FROM '${PARQUET_DIR}/npm-edges-*.parquet';

  CREATE INDEX idx_depname ON edges(DepName);
"

ROWS=$(duckdb "$DB_PATH" -csv -c "SELECT COUNT(*) FROM edges;" | tail -1)
SIZE=$(du -sh "$DB_PATH" | cut -f1)

echo ""
echo "=== Done ==="
echo "Database: $DB_PATH"
echo "Rows:     $ROWS"
echo "Size:     $SIZE"
echo ""
echo "Usage:"
echo "  blast-radius npm axios 1.14.1 --db $DB_PATH"
