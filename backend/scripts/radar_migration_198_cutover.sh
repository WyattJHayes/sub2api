#!/usr/bin/env bash
set -euo pipefail

phase="${1:-}"
database_url="${RADAR_DATABASE_URL:-${DATABASE_URL:-}}"
psql_bin="${RADAR_PSQL_BIN:-psql}"
migrations_dir="${RADAR_MIGRATIONS_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../migrations" && pwd)}"

if [[ -z "${database_url}" ]]; then
  echo "RADAR_DATABASE_URL or DATABASE_URL is required" >&2
  exit 2
fi
case "${phase}" in
  audit|drain|close|migrate|reopen) ;;
  *) echo "usage: $0 audit|drain|close|migrate|reopen" >&2; exit 2 ;;
esac

psql_radar() {
  "${psql_bin}" -X -q -t -A -v ON_ERROR_STOP=1 "${database_url}" "$@"
}

state="$(psql_radar -c "SELECT write_mode || ':' || guard_mode || ':' || minimum_protocol_version FROM evaluation_schema_cutovers WHERE id=1")"
state="${state//[[:space:]]/}"

fail_state() {
  echo "cutover remains in ${state}: $*" >&2
  exit 1
}

sha256_stream() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

migration_checksum() {
  local file="$1"
  local content
  [[ -f "${file}" ]] || fail_state "migration file is missing: ${file}"
  content="$(<"${file}")"
  content="${content#"${content%%[![:space:]]*}"}"
  content="${content%"${content##*[![:space:]]}"}"
  printf '%s' "${content}" | sha256_stream
}

validate_migration_checksum() {
  local expected actual
  expected="$(migration_checksum "${migrations_dir}/198_add_radar_trusted_governance.sql")"
  actual="$(psql_radar -c "SELECT checksum FROM schema_migrations WHERE filename='198_add_radar_trusted_governance.sql'")"
  [[ -n "${actual}" ]] || fail_state "migration 198 is not registered"
  [[ "${actual}" == "${expected}" ]] || fail_state "migration checksum mismatch for 198_add_radar_trusted_governance.sql"
}

storage_mode() {
  psql_radar -c "SELECT mode FROM evaluation_gate_storage_modes WHERE id=1"
}

case "${phase}" in
  audit)
    [[ "${state}" == "open:audit:1" ]] || fail_state "audit requires open/audit/protocol 1"
    validate_migration_checksum
    [[ "$(storage_mode)" == "compatibility" ]] || fail_state "198 audit requires compatibility storage mode"
    rejected="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_protocol_audits WHERE accepted=FALSE AND created_at >= NOW() - INTERVAL '15 minutes'")"
    [[ "${rejected}" == "0" ]] || fail_state "recent rejected writer audit count is ${rejected}"
    echo "migration 198 audit clean window confirmed"
    ;;
  drain)
    [[ "${state}" == "open:audit:1" ]] || fail_state "drain requires open/audit/protocol 1"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode='draining', updated_at=NOW() WHERE id=1"
    echo "migration 198 write mode is draining"
    ;;
  close)
    [[ "${state}" == "draining:audit:1" ]] || fail_state "close requires draining/audit/protocol 1"
    active_writers="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_sessions WHERE heartbeat_expires_at > NOW() OR active_lease_count > 0")"
    [[ "${active_writers}" == "0" ]] || fail_state "active evaluation writer or lease count is ${active_writers}"
    active_leases="$(psql_radar -c "SELECT (SELECT COUNT(*) FROM evaluation_assignments WHERE status IN ('leased','running')) + (SELECT COUNT(*) FROM evaluation_grading_jobs WHERE status='leased') + (SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE status='leased') + (SELECT COUNT(*) FROM evaluation_route_evidence_terminalization_outbox WHERE processed_at IS NULL)")"
    [[ "${active_leases}" == "0" ]] || fail_state "active evaluation lease count is ${active_leases}"
    active_transactions="$(psql_radar -c "SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND backend_type='client backend' AND state IN ('active','idle in transaction','idle in transaction (aborted)','fastpath function call')")"
    [[ "${active_transactions}" == "0" ]] || fail_state "active database transaction count is ${active_transactions}"
    advisory_locks="$(psql_radar -c "SELECT COUNT(*) FROM pg_locks WHERE locktype='advisory' AND granted AND pid<>pg_backend_pid()")"
    [[ "${advisory_locks}" == "0" ]] || fail_state "advisory lock count is ${advisory_locks}"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode='closed', updated_at=NOW() WHERE id=1"
    echo "migration 198 write mode is closed"
    ;;
  migrate)
    [[ "${state}" == "closed:audit:1" ]] || fail_state "migrate requires closed/audit/protocol 1"
    validate_migration_checksum
    [[ "$(storage_mode)" == "compatibility" ]] || fail_state "migration 198 requires compatibility storage mode"
    echo "migration 198 compatibility storage verified"
    ;;
  reopen)
    [[ "${state}" == "closed:audit:1" ]] || fail_state "reopen requires closed/audit/protocol 1"
    [[ "$(storage_mode)" == "compatibility" ]] || fail_state "198 reopen requires compatibility storage mode"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode='open', updated_at=NOW() WHERE id=1"
    echo "migration 198 cutover reopened in compatibility mode"
    ;;
esac
