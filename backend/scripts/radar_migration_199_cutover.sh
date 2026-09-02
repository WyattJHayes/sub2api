#!/usr/bin/env bash
set -euo pipefail

phase="${1:-}"
database_url="${RADAR_DATABASE_URL:-${DATABASE_URL:-}}"
psql_bin="${RADAR_PSQL_BIN:-psql}"
migrations_dir="${RADAR_MIGRATIONS_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../migrations" && pwd)}"
target_protocol_version=2
migration_names=(
  "199_add_radar_evidence_revision_pipeline.sql"
  "200_add_radar_reliability_and_dr.sql"
  "200_add_score_idempotency_score_ref.sql"
  "201_add_revision_batch_events.sql"
  "202_add_gate_policy_approvals.sql"
)
if [[ -z "${database_url}" ]]; then
  echo "RADAR_DATABASE_URL or DATABASE_URL is required" >&2
  exit 2
fi
case "${phase}" in
  audit|drain|close|migrate|enforce|register|reopen) ;;
  *) echo "usage: $0 audit|drain|close|migrate|enforce|register|reopen" >&2; exit 2 ;;
esac

psql_radar() {
  "${psql_bin}" -X -q -t -A -v ON_ERROR_STOP=1 "${database_url}" "$@"
}

state="$(psql_radar -c "SELECT write_mode || ':' || guard_mode FROM evaluation_schema_cutovers WHERE id=1")"
state="${state//[[:space:]]/}"
fail_state() {
  echo "cutover remains in ${state}: $*" >&2
  exit 1
}

sha256_stream() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
    return
  fi
  shasum -a 256 | awk '{print $1}'
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

validate_migration_checksums() {
  local name expected actual
  for name in "${migration_names[@]}"; do
    expected="$(migration_checksum "${migrations_dir}/${name}")"
    actual="$(psql_radar -c "SELECT checksum FROM schema_migrations WHERE filename='${name}'")"
    [[ "${actual}" == "${expected}" ]] || fail_state "migration checksum mismatch for ${name}"
  done
}

storage_mode() {
  psql_radar -c "SELECT mode FROM evaluation_gate_storage_modes WHERE id=1"
}

case "${phase}" in
  audit)
    [[ "${state}" == "open:audit" ]] || fail_state "audit requires open/audit"
    [[ "$(storage_mode)" == "compatibility" ]] || fail_state "199 audit requires compatibility storage mode"
    rejected="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_protocol_audits WHERE accepted=FALSE AND created_at >= NOW() - INTERVAL '15 minutes'")"
    [[ "${rejected}" == "0" ]] || fail_state "recent rejected writer audit count is ${rejected}"
    echo "migration 199 audit clean window confirmed"
    ;;
  drain)
    [[ "${state}" == "open:audit" ]] || fail_state "drain requires open/audit"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode='draining', updated_at=NOW() WHERE id=1"
    echo "migration 199 write mode is draining"
    ;;
  close)
    [[ "${state}" == "draining:audit" ]] || fail_state "close requires draining/audit"
    active_writer_sessions="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_sessions WHERE writer_kind <> 'migration' AND heartbeat_expires_at > NOW()")"
    [[ "${active_writer_sessions}" == "0" ]] || fail_state "active writer session count is ${active_writer_sessions}"

    active_evaluation_leases="$(psql_radar -c "SELECT (SELECT COUNT(*) FROM evaluation_assignments WHERE status IN ('leased','running')) + (SELECT COUNT(*) FROM evaluation_grading_jobs WHERE status='leased') + (SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE status='leased') + (SELECT COALESCE(SUM(active_lease_count),0) FROM evaluation_writer_sessions WHERE heartbeat_expires_at > NOW())")"
    active_outbox_leases=0
    outbox_table_exists="$(psql_radar -c "SELECT to_regclass('public.evaluation_outbox_events') IS NOT NULL")"
    if [[ "${outbox_table_exists}" == "t" ]]; then
      active_outbox_leases="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_outbox_events WHERE status='leased'")"
    fi
    active_evaluation_leases=$((active_evaluation_leases + active_outbox_leases))
    [[ "${active_evaluation_leases}" == "0" ]] || fail_state "active evaluation lease count is ${active_evaluation_leases}"

    active_transactions="$(psql_radar -c "SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND backend_type='client backend' AND state IN ('active','idle in transaction','idle in transaction (aborted)','fastpath function call')")"
    [[ "${active_transactions}" == "0" ]] || fail_state "active database transaction count is ${active_transactions}"

    advisory_locks="$(psql_radar -c "SELECT COUNT(*) FROM pg_locks WHERE locktype='advisory' AND granted AND pid<>pg_backend_pid()")"
    [[ "${advisory_locks}" == "0" ]] || fail_state "advisory lock count is ${advisory_locks}"

    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode='closed', updated_at=NOW() WHERE id=1"
    echo "migration 199 write mode is closed"
    ;;
  migrate)
    [[ "${state}" == "closed:audit" ]] || fail_state "migrate requires closed/audit"
    [[ "$(storage_mode)" == "compatibility" ]] || fail_state "199 migrate requires compatibility storage mode"
    trusted_evidence_table="$(psql_radar -c "SELECT to_regclass('public.evaluation_gate_evidence_manifests') IS NOT NULL")"
    [[ "${trusted_evidence_table}" == "t" ]] || fail_state "trusted gate evidence schema is missing"
    applied_count="$(psql_radar -c "SELECT COUNT(*) FROM schema_migrations WHERE filename IN ('199_add_radar_evidence_revision_pipeline.sql','200_add_radar_reliability_and_dr.sql','200_add_score_idempotency_score_ref.sql','201_add_revision_batch_events.sql','202_add_gate_policy_approvals.sql')")"
    expected_count="${#migration_names[@]}"
    if [[ "${applied_count}" == "0" ]]; then
      {
        echo "BEGIN;"
        echo "SELECT set_config('app.evaluation_writer_kind','migration',false);"
        echo "SELECT set_config('app.evaluation_writer_protocol','${target_protocol_version}',false);"
        echo "SELECT set_config('app.evaluation_writer_instance_id','00000000-0000-0000-0000-000000000199',false);"
        for name in "${migration_names[@]}"; do
          printf "\\i '%s'\n" "${migrations_dir}/${name}"
          checksum="$(migration_checksum "${migrations_dir}/${name}")"
          printf "INSERT INTO schema_migrations (filename,checksum) VALUES ('%s','%s');\n" "${name}" "${checksum}"
        done
        echo "UPDATE evaluation_schema_cutovers SET minimum_protocol_version=${target_protocol_version}, updated_at=NOW() WHERE id=1;"
        echo "COMMIT;"
      } | psql_radar
    elif [[ "${applied_count}" == "${expected_count}" ]]; then
      validate_migration_checksums
      psql_radar -c "BEGIN; SELECT set_config('app.evaluation_writer_kind','migration',true); UPDATE evaluation_schema_cutovers SET minimum_protocol_version=${target_protocol_version}, updated_at=NOW() WHERE id=1; COMMIT;"
    elif [[ "${applied_count}" -lt "${expected_count}" ]]; then
      for name in "${migration_names[@]}"; do
        already_applied="$(psql_radar -c "SELECT COUNT(*) FROM schema_migrations WHERE filename='${name}'")"
        [[ "${already_applied}" == "1" ]] && continue
        checksum="$(migration_checksum "${migrations_dir}/${name}")"
        {
          echo "BEGIN;"
          echo "SELECT set_config('app.evaluation_writer_kind','migration',false);"
          echo "SELECT set_config('app.evaluation_writer_protocol','${target_protocol_version}',false);"
          echo "SELECT set_config('app.evaluation_writer_instance_id','00000000-0000-0000-0000-000000000199',false);"
          printf "\\i '%s'\n" "${migrations_dir}/${name}"
          printf "INSERT INTO schema_migrations (filename,checksum) VALUES ('%s','%s');\n" "${name}" "${checksum}"
          echo "COMMIT;"
        } | psql_radar
      done
      validate_migration_checksums
    else
      fail_state "expected migration set has an invalid applied count"
    fi
    psql_radar -c "UPDATE evaluation_gate_storage_modes SET mode='trusted', updated_at=NOW() WHERE id=1"
    echo "migrations 199, 200 reliability, 200 score, 201 revision events and 202 policy approvals verified at writer protocol ${target_protocol_version}"
    ;;
  enforce)
    [[ "${state}" == "closed:audit" || "${state}" == "closed:enforce" ]] || fail_state "enforce requires closed"
    protocol="$(psql_radar -c "SELECT minimum_protocol_version FROM evaluation_schema_cutovers WHERE id=1")"
    [[ "${protocol}" == "${target_protocol_version}" ]] || fail_state "minimum writer protocol is ${protocol}, expected ${target_protocol_version}"
    validate_migration_checksums
    [[ "$(storage_mode)" == "trusted" ]] || fail_state "199 enforce requires trusted storage mode"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET guard_mode='enforce', updated_at=NOW() WHERE id=1"
    echo "migration 199 writer guard is enforce"
    ;;
  register)
    [[ "${state}" == "closed:enforce" ]] || fail_state "register requires closed/enforce"
    writer_instance_id="${RADAR_WRITER_INSTANCE_ID:-}"
    writer_kind="${RADAR_WRITER_KIND:-}"
    [[ -n "${writer_instance_id}" ]] || fail_state "RADAR_WRITER_INSTANCE_ID is required"
    [[ -n "${writer_kind}" && ${#writer_kind} -le 32 ]] || fail_state "RADAR_WRITER_KIND must be between 1 and 32 characters"
    printf '%s\n' \
      "INSERT INTO evaluation_writer_sessions (instance_id,writer_kind,protocol_version,active_lease_count,heartbeat_expires_at,last_transaction_at)" \
      "VALUES (:'writer_instance_id'::uuid,:'writer_kind',${target_protocol_version},0,NOW()+INTERVAL '5 minutes',NOW())" \
      "ON CONFLICT (instance_id) DO UPDATE SET writer_kind=EXCLUDED.writer_kind,protocol_version=EXCLUDED.protocol_version,active_lease_count=0,heartbeat_expires_at=EXCLUDED.heartbeat_expires_at,last_transaction_at=EXCLUDED.last_transaction_at,updated_at=NOW();" |
      "${psql_bin}" -X -q -t -A -v ON_ERROR_STOP=1 -v writer_instance_id="${writer_instance_id}" -v writer_kind="${writer_kind}" "${database_url}"
    echo "protocol ${target_protocol_version} writer session registered"
    ;;
  reopen)
    [[ "${state}" == "closed:enforce" ]] || fail_state "reopen requires closed/enforce"
    protocol="$(psql_radar -c "SELECT minimum_protocol_version FROM evaluation_schema_cutovers WHERE id=1")"
    [[ "${protocol}" == "${target_protocol_version}" ]] || fail_state "minimum writer protocol is ${protocol}, expected ${target_protocol_version}"
    old_sessions="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_sessions WHERE heartbeat_expires_at>NOW() AND protocol_version<${target_protocol_version}")"
    [[ "${old_sessions}" == "0" ]] || fail_state "old writer session count is ${old_sessions}"
    current_sessions="$(psql_radar -c "SELECT COUNT(*) FROM evaluation_writer_sessions WHERE heartbeat_expires_at>NOW() AND protocol_version>=${target_protocol_version}")"
    [[ "${current_sessions}" != "0" ]] || fail_state "protocol ${target_protocol_version} writer session is required"
    [[ "$(storage_mode)" == "trusted" ]] || fail_state "199 reopen requires trusted storage mode"
    psql_radar -c "UPDATE evaluation_schema_cutovers SET write_mode='open', updated_at=NOW() WHERE id=1"
    echo "migration 199 cutover reopened in enforce mode"
    ;;
esac
