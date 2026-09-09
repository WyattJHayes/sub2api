#!/usr/bin/env bash
set -euo pipefail

phase="${1:-}"
database_url="${RADAR_DATABASE_URL:-${DATABASE_URL:-}}"
if [[ -z "${database_url}" ]]; then
  echo "RADAR_DATABASE_URL or DATABASE_URL is required" >&2
  exit 2
fi
case "${phase}" in
  audit|drain|close|enforce|reopen) ;;
  *) echo "usage: $0 audit|drain|close|enforce|reopen" >&2; exit 2 ;;
esac

psql_radar() {
  psql -X -q -t -A -v ON_ERROR_STOP=1 "${database_url}" "$@"
}

state="$(psql_radar -c "SELECT write_mode || ':' || guard_mode FROM evaluation_schema_cutovers WHERE id = 1")"
state="${state//[[:space:]]/}"

fail_state() {
  echo "cutover remains in ${state}: $*" >&2
  exit 1
}

case "${phase}" in
  audit)
    [[ "${state}" == "open:audit" ]] || fail_state "audit requires open/audit"
    rejected="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_protocol_audits WHERE accepted = FALSE AND created_at >= NOW() - INTERVAL '15 minutes'")"
    [[ "${rejected}" == "0" ]] || fail_state "recent rejected writer audit count is ${rejected}"
    echo "audit clean window confirmed"
    ;;
  drain)
    [[ "${state}" == "open:audit" ]] || fail_state "drain requires open/audit"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode = 'draining', updated_at = NOW() WHERE id = 1"
    echo "write mode is draining"
    ;;
  close)
    [[ "${state}" == "draining:audit" ]] || fail_state "close requires draining/audit"
    active_leases="$(psql_radar -c "SELECT (SELECT COUNT(*) FROM evaluation_assignments WHERE status IN ('leased','running')) + (SELECT COUNT(*) FROM evaluation_grading_jobs WHERE status = 'leased') + (SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE status = 'leased') + (SELECT COALESCE(SUM(active_lease_count), 0) FROM evaluation_writer_sessions WHERE heartbeat_expires_at > NOW())")"
    old_sessions="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_sessions s JOIN evaluation_schema_cutovers c ON c.id = 1 WHERE s.heartbeat_expires_at > NOW() AND s.protocol_version < c.minimum_protocol_version")"
    advisory_locks="$(psql_radar -c "SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND granted")"
    [[ "${active_leases}" == "0" ]] || fail_state "active lease count is ${active_leases}"
    [[ "${old_sessions}" == "0" ]] || fail_state "old writer session count is ${old_sessions}"
    [[ "${advisory_locks}" == "0" ]] || fail_state "advisory lock count is ${advisory_locks}"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode = 'closed', updated_at = NOW() WHERE id = 1"
    echo "write mode is closed"
    ;;
  enforce)
    [[ "${state}" == "closed:audit" || "${state}" == "closed:enforce" ]] || fail_state "enforce requires closed"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET guard_mode = 'enforce', updated_at = NOW() WHERE id = 1"
    echo "writer guard is enforce"
    ;;
  reopen)
    [[ "${state}" == "closed:enforce" ]] || fail_state "reopen requires closed/enforce"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode = 'open', guard_mode = 'audit', updated_at = NOW() WHERE id = 1"
    echo "cutover reopened in audit mode"
    ;;
esac
