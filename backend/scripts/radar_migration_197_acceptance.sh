#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cutover_script="${script_dir}/radar_migration_197_cutover.sh"

"${cutover_script}" audit
"${cutover_script}" drain
"${cutover_script}" close
"${cutover_script}" enforce

database_url="${RADAR_DATABASE_URL:-${DATABASE_URL:-}}"
state="$(psql -X -q -t -A -v ON_ERROR_STOP=1 "${database_url}" -c "SELECT write_mode || ':' || guard_mode FROM evaluation_schema_cutovers WHERE id = 1")"
state="${state//[[:space:]]/}"
[[ "${state}" == "closed:enforce" ]] || { echo "acceptance expected closed:enforce, got ${state}" >&2; exit 1; }

"${cutover_script}" reopen
state="$(psql -X -q -t -A -v ON_ERROR_STOP=1 "${database_url}" -c "SELECT write_mode || ':' || guard_mode FROM evaluation_schema_cutovers WHERE id = 1")"
state="${state//[[:space:]]/}"
[[ "${state}" == "open:audit" ]] || { echo "acceptance expected open:audit after reopen, got ${state}" >&2; exit 1; }
echo "migration 197 cutover acceptance passed"
