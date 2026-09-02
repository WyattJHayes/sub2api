#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
MIGRATIONS_DIR=${RADAR_MIGRATIONS_DIR:-$ROOT_DIR/backend/migrations}
MIGRATION_LEDGER_TOOL=${RADAR_MIGRATION_LEDGER_TOOL:-$ROOT_DIR/deploy/radar/migration_ledger.py}
MIGRATION_MANIFEST_DIR=${RADAR_MIGRATION_MANIFEST_DIR:-$ROOT_DIR/deploy/radar/manifests/v0.2.0}
MIGRATION_BASELINE_MANIFEST=${RADAR_MIGRATION_BASELINE_MANIFEST:-$MIGRATION_MANIFEST_DIR/migration-baseline.tsv}
MIGRATION_EXPECTED_NEW=${RADAR_MIGRATION_EXPECTED_NEW:-$MIGRATION_MANIFEST_DIR/expected-new.txt}
MIGRATION_LEGACY_ENTRIES=${RADAR_MIGRATION_LEGACY_ENTRIES:-$MIGRATION_MANIFEST_DIR/legacy-entries.txt}
PROJECT_NAME=${RADAR_MIGRATION_REHEARSAL_PROJECT_NAME:-sub2api-radar-v11-rehearsal}
RESOURCE_PREFIX=${RADAR_MIGRATION_REHEARSAL_RESOURCE_PREFIX:-$PROJECT_NAME}
ENV_FILE=${RADAR_MIGRATION_REHEARSAL_ENV_FILE:-}
BACKUP=${RADAR_MIGRATION_REHEARSAL_BACKUP:-}
BACKUP_SHA256=${RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256:-}
DATABASE_HOST=${RADAR_MIGRATION_REHEARSAL_DATABASE_HOST:-radar-rehearsal-postgres}
DATABASE_NAME=${RADAR_MIGRATION_REHEARSAL_DATABASE_NAME:-radar}
DATABASE_USER=${RADAR_MIGRATION_REHEARSAL_DATABASE_USER:-radar}
POSTGRES_PASSWORD_FILE=${RADAR_MIGRATION_REHEARSAL_POSTGRES_PASSWORD_FILE:-}
DATABASE_PASSWORD_FILE=${RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD_FILE:-}
PGPASS_FILE=${RADAR_MIGRATION_REHEARSAL_PGPASS_FILE:-}
POSTGRES_IMAGE=${RADAR_MIGRATION_REHEARSAL_POSTGRES_IMAGE:-postgres:18-alpine}
REDIS_IMAGE=${RADAR_MIGRATION_REHEARSAL_REDIS_IMAGE:-redis:8-alpine}
CANDIDATE_IMAGE=${RADAR_CONTROL_PLANE_IMAGE:-}
CANDIDATE_DIGEST=${RADAR_CONTROL_PLANE_IMAGE_DIGEST:-}
CANDIDATE_WORKER_IMAGE=${RADAR_WORKER_IMAGE:-}
CANDIDATE_WORKER_DIGEST=${RADAR_WORKER_IMAGE_DIGEST:-}
ROLLBACK_IMAGE=${RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE:-}
ROLLBACK_DIGEST=${RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST:-}
ROLLBACK_WORKER_IMAGE=${RADAR_V10_ROLLBACK_WORKER_IMAGE:-}
ROLLBACK_WORKER_DIGEST=${RADAR_V10_ROLLBACK_WORKER_IMAGE_DIGEST:-}
EVIDENCE_DIR=${RADAR_MIGRATION_REHEARSAL_EVIDENCE_DIR:-}
DRY_RUN=${RADAR_MIGRATION_REHEARSAL_DRY_RUN:-0}
CLONE_TIMEOUT_SECONDS=${RADAR_MIGRATION_REHEARSAL_CLONE_TIMEOUT_SECONDS:-300}
RETAIN_VOLUMES=${RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES:-0}
RETENTION_RECORD=${RADAR_MIGRATION_REHEARSAL_RETENTION_RECORD:-}
RETENTION_SECONDS=${RADAR_MIGRATION_REHEARSAL_RETENTION_SECONDS:-}
RETENTION_RUN_ID=${RADAR_MIGRATION_REHEARSAL_RETENTION_RUN_ID:-}
RETENTION_EVIDENCE_DIR=${RADAR_MIGRATION_REHEARSAL_RETENTION_EVIDENCE_DIR:-}
RETENTION_SCRIPT=${RADAR_MIGRATION_REHEARSAL_RETENTION_SCRIPT:-}
RETENTION_GATE2_PROJECT=${RADAR_MIGRATION_REHEARSAL_RETENTION_GATE2_PROJECT:-}
RETENTION_GATE4_PROJECT=${RADAR_MIGRATION_REHEARSAL_RETENTION_GATE4_PROJECT:-}

# The isolated rehearsal accepts database credentials only through private files.
# Remove legacy inherited variables before invoking any subprocesses.
unset DATABASE_PASSWORD POSTGRES_PASSWORD PGPASSWORD RADAR_POSTGRES_PASSWORD \
    RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD

if [[ -z "$EVIDENCE_DIR" ]]; then
    EVIDENCE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/radar-v01176-rehearsal.XXXXXX")
    chmod 700 "$EVIDENCE_DIR"
fi

DB_CONTAINER="${RESOURCE_PREFIX}-postgres"
ROLLBACK_DB_CONTAINER="${RESOURCE_PREFIX}-rollback-postgres"
REDIS_CONTAINER="${RESOURCE_PREFIX}-redis"
CANDIDATE_CONTAINER="${RESOURCE_PREFIX}-candidate"
ROLLBACK_CONTAINER="${RESOURCE_PREFIX}-rollback"
ROLLBACK_WORKER_CONTAINER="${RESOURCE_PREFIX}-rollback-worker"
NETWORK="${RESOURCE_PREFIX}-network"
VOLUME="${RESOURCE_PREFIX}-postgres"
ROLLBACK_VOLUME="${RESOURCE_PREFIX}-rollback-postgres"
NETWORK_CREATED=0
VOLUME_CREATED=0
ROLLBACK_VOLUME_CREATED=0
CONTAINERS_STARTED=0
MIGRATION_224_CHECKSUM=
MIGRATION_225_CHECKSUM=
MIGRATION_COUNT=
MIGRATION_224_SEMANTICS_OK=false
MIGRATION_225_SEMANTICS_OK=false
MIGRATION_LEDGER_OK=false
MIGRATION_PENDING_JSON=[]
MIGRATION_CHECKSUM_MISMATCHES_JSON=[]
MIGRATION_LEGACY_ENTRIES_JSON=[]
BASELINE_SCHEMA_MIGRATIONS=
ACTUAL_SCHEMA_MIGRATIONS=
EXPECTED_SCHEMA_MIGRATIONS=
BASELINE_LEDGER_SHA256=
CANDIDATE_LEDGER_SHA256=
EXPECTED_CANDIDATE_LEDGER_SHA256=
EXPECTED_RUNTIME_LEDGER_SHA256=
RUNTIME_LEDGER_SHA256=
ROLLBACK_DATABASE_CLONE_USED=false

MIGRATION_NAMES=(
    "191_add_radar_control_plane.sql"
    "191_passkey_credentials.sql"
    "192_add_evaluation_sample_execution_identity.sql"
    "192_group_profit_control.sql"
    "193_add_radar_grading_statistics.sql"
    "193_group_profit_control_auth_cache_invalidation.sql"
    "221_add_radar_tracked_models.sql"
    "222_add_radar_quality_reports.sql"
    "223_add_quality_observation_context.sql"
    "224_add_quality_report_aggregate_revision.sql"
    "221_group_model_pricing.sql"
    "222_group_usage_daily_rollups.sql"
    "223_group_usage_rollup_timezone.sql"
    "224_user_platform_quotas_add_cn_providers.sql"
    "225_backfill_codex_fingerprint_seed.sql"
    "225_channel_model_time_pricing.sql"
    "226_channel_monitor_quota_mode.sql"
    "227_ops_model_not_found_sla_classification.sql"
)

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

require_digest() {
    local name=$1
    local value=$2
    [[ "$value" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "$name must be a lowercase sha256 digest"
}

require_mode_600() {
    local name=$1
    local path=$2
    [[ -f "$path" && -r "$path" ]] || fail "$name must be a readable regular file: $path"
    local mode
    if mode=$(stat -c '%a' "$path" 2>/dev/null); then
        :
    elif mode=$(stat -f '%Lp' "$path" 2>/dev/null); then
        :
    else
        fail "unable to inspect $name permissions: $path"
    fi
    [[ "$mode" == "600" ]] || fail "$name must have mode 600: $path"
}

require_secret_file() {
    local name=$1
    local path=$2
    [[ ! -L "$path" && -f "$path" && -r "$path" ]] || fail "$name must be a readable non-symlink regular file"
    require_mode_600 "$name" "$path"
    local parent mode
    parent=$(dirname "$path")
    if mode=$(stat -c '%a' "$parent" 2>/dev/null); then
        :
    elif mode=$(stat -f '%Lp' "$parent" 2>/dev/null); then
        :
    else
        fail "unable to inspect $name parent permissions"
    fi
    [[ "$mode" == 700 ]] || fail "$name parent directory must have mode 700"
    [[ -s "$path" ]] || fail "$name must not be empty"
}

retention_authorization_valid() {
    [[ -n "$RETENTION_RECORD" && -f "$RETENTION_RECORD" && -r "$RETENTION_RECORD" ]] || return 1
    local mode
    if mode=$(stat -c '%a' "$RETENTION_RECORD" 2>/dev/null); then
        :
    elif mode=$(stat -f '%Lp' "$RETENTION_RECORD" 2>/dev/null); then
        :
    else
        return 1
    fi
    [[ "$mode" == 600 ]] || return 1
    python3 - "$RETENTION_RECORD" "$PROJECT_NAME" "$RESOURCE_PREFIX" "$RETENTION_SECONDS" \
        "$RETENTION_RUN_ID" "$RETENTION_EVIDENCE_DIR" "$RETENTION_SCRIPT" \
        "$RETENTION_GATE2_PROJECT" "$RETENTION_GATE4_PROJECT" <<'PY'
import json
import pathlib
import shlex
import sys
from datetime import datetime, timezone

path = pathlib.Path(sys.argv[1])
project = sys.argv[2]
resource_prefix = sys.argv[3]
expected_seconds_text = sys.argv[4]
run_id = sys.argv[5]
evidence_dir = sys.argv[6]
script = sys.argv[7]
gate2_project = sys.argv[8]
gate4_project = sys.argv[9]
try:
    document = json.loads(path.read_text(encoding="utf-8"))
except (OSError, json.JSONDecodeError) as error:
    raise SystemExit(f"retention authorization record is invalid: {error}")
if not isinstance(document, dict) or set(document) != {
    "schema_version", "deadline", "deadline_seconds", "cleanup_command", "migration_projects"
}:
    raise SystemExit("retention authorization record has an invalid schema")
if document.get("schema_version") != "radar-local-retention-v1":
    raise SystemExit("retention authorization record has an invalid version")
if resource_prefix != project:
    raise SystemExit("retention authorization project and resource prefix differ")
if (
    not expected_seconds_text.isdigit()
    or expected_seconds_text.startswith("0")
    or int(expected_seconds_text) <= 0
    or int(expected_seconds_text) > 86400
):
    raise SystemExit("expected retention authorization deadline is not bounded")
expected_seconds = int(expected_seconds_text)
seconds = document.get("deadline_seconds")
if type(seconds) is not int or seconds != expected_seconds:
    raise SystemExit("retention authorization deadline is not bounded")
deadline_text = document.get("deadline")
if not isinstance(deadline_text, str) or not deadline_text.endswith("Z"):
    raise SystemExit("retention authorization deadline is invalid")
try:
    deadline = datetime.fromisoformat(deadline_text.removesuffix("Z") + "+00:00")
except ValueError as error:
    raise SystemExit("retention authorization deadline is invalid") from error
remaining = (deadline - datetime.now(timezone.utc)).total_seconds()
if remaining <= 0 or remaining > seconds:
    raise SystemExit("retention authorization deadline is expired or unbounded")
expected_cleanup_command = shlex.join([
    "env",
    f"RADAR_LOCAL_RUN_ID={run_id}",
    f"RADAR_LOCAL_EVIDENCE_DIR={evidence_dir}",
    script,
    "--cleanup-retained",
])
if document.get("cleanup_command") != expected_cleanup_command:
    raise SystemExit("retention authorization cleanup command is invalid")
projects = document.get("migration_projects")
expected_projects = [gate2_project, gate4_project]
if not gate2_project or not gate4_project or gate2_project == gate4_project:
    raise SystemExit("expected retention authorization projects are invalid")
if project == gate2_project:
    project_index = 0
elif project == gate4_project:
    project_index = 1
else:
    raise SystemExit("retention authorization does not cover this migration project")
if projects != expected_projects or projects[project_index] != project:
    raise SystemExit("retention authorization does not cover this migration project")
PY
}

validate_retention_authorization() {
    [[ "$RETAIN_VOLUMES" == 1 ]] || return 0
    retention_authorization_valid || fail "retention authorization record is invalid or expired"
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
        return 0
    fi
    command -v shasum >/dev/null 2>&1 || fail "required command is unavailable: sha256sum or shasum"
    shasum -a 256 "$1" | awk '{print $1}'
}

migration_checksum() {
    python3 - "$1" <<'PY'
import hashlib
import pathlib
import sys

content = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip()
print(hashlib.sha256(content.encode("utf-8")).hexdigest())
PY
}

load_expected_migration_count() {
    python3 - "$MIGRATION_BASELINE_MANIFEST" "$MIGRATION_EXPECTED_NEW" "$MIGRATION_LEGACY_ENTRIES" <<'PY'
import pathlib
import sys

baseline, expected_new, legacy = map(pathlib.Path, sys.argv[1:])
for path in (baseline, expected_new, legacy):
    if not path.is_file():
        raise SystemExit(f"migration manifest input is unavailable: {path}")
def names(path: pathlib.Path) -> list[str]:
    values = [line.strip() for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
    if len(values) != len(set(values)):
        raise SystemExit(f"migration manifest input contains duplicate names: {path}")
    return values
baseline_rows = [line for line in baseline.read_text(encoding="utf-8").splitlines() if line.strip()]
expected = names(expected_new)
names(legacy)
if any("\t" not in row or len(row.split("\t")) != 2 for row in baseline_rows):
    raise SystemExit("migration baseline manifest must contain filename and checksum rows")
print(len(baseline_rows) + len(expected))
PY
}

run_migration_ledger() {
    local actual=$1
    local output=$2
    python3 "$MIGRATION_LEDGER_TOOL" \
        --baseline "$MIGRATION_BASELINE_MANIFEST" \
        --candidate-dir "$MIGRATIONS_DIR" \
        --expected-new "$MIGRATION_EXPECTED_NEW" \
        --legacy-entries "$MIGRATION_LEGACY_ENTRIES" \
        --actual "$actual" \
        --output "$output"
}

load_migration_ledger_summary() {
    local path=$1
    local key value
    while IFS=$'\t' read -r key value; do
        case "$key" in
            baseline_schema_migrations) BASELINE_SCHEMA_MIGRATIONS=$value ;;
            actual_schema_migrations) ACTUAL_SCHEMA_MIGRATIONS=$value ;;
            expected_schema_migrations) EXPECTED_SCHEMA_MIGRATIONS=$value ;;
            migration_ledger_ok) MIGRATION_LEDGER_OK=$value ;;
            candidate_pending_migrations) MIGRATION_PENDING_JSON=$value ;;
            legacy_entries) MIGRATION_LEGACY_ENTRIES_JSON=$value ;;
            checksum_mismatches) MIGRATION_CHECKSUM_MISMATCHES_JSON=$value ;;
            baseline_ledger_sha256) BASELINE_LEDGER_SHA256=$value ;;
            candidate_ledger_sha256) CANDIDATE_LEDGER_SHA256=$value ;;
            expected_candidate_ledger_sha256) EXPECTED_CANDIDATE_LEDGER_SHA256=$value ;;
            expected_runtime_ledger_sha256) EXPECTED_RUNTIME_LEDGER_SHA256=$value ;;
            runtime_ledger_sha256) RUNTIME_LEDGER_SHA256=$value ;;
            *) fail "migration ledger emitted an unknown summary field" ;;
        esac
    done < <(python3 - "$path" <<'PY'
import json
import re
import sys

document = json.loads(open(sys.argv[1], encoding="utf-8").read())
required = {
    "baseline_schema_migrations": int,
    "actual_schema_migrations": int,
    "expected_schema_migrations": int,
    "migration_ledger_ok": bool,
    "candidate_pending_migrations": list,
    "legacy_entries": list,
    "checksum_mismatches": list,
    "baseline_ledger_sha256": str,
    "candidate_ledger_sha256": str,
    "expected_candidate_ledger_sha256": str,
    "expected_runtime_ledger_sha256": str,
    "runtime_ledger_sha256": str,
}
for key, value_type in required.items():
    source_key = "ok" if key == "migration_ledger_ok" else key
    value = document.get(source_key)
    if type(value) is not value_type:
        raise SystemExit(f"migration ledger field is malformed: {source_key}")
    if value_type is int and value < 0:
        raise SystemExit(f"migration ledger count is invalid: {key}")
    if value_type is list and not all(isinstance(item, str) for item in value):
        raise SystemExit(f"migration ledger list is malformed: {key}")
    if value_type is str and not re.fullmatch(r"[0-9a-f]{64}", value):
        raise SystemExit(f"migration ledger fingerprint is malformed: {key}")
    if value_type is bool:
        value = "true" if value else "false"
    elif value_type is list:
        value = json.dumps(value, separators=(",", ":"))
    print(f"{key}\t{value}")
PY
)
    [[ -n "$BASELINE_SCHEMA_MIGRATIONS" && -n "$ACTUAL_SCHEMA_MIGRATIONS" ]] || \
        fail "migration ledger summary is incomplete"
}

require_image_binding() {
    local name=$1
    local image=$2
    local digest=$3
    [[ -n "$image" ]] || fail "$name image is required"
    [[ "$image" == *"@$digest" ]] || fail "$name image must end with its digest"
}

validate_inputs() {
    [[ -n "$BACKUP" ]] || fail "backup path is required"
    require_mode_600 "backup" "$BACKUP"
    [[ "$BACKUP_SHA256" =~ ^[0-9a-f]{64}$ ]] || fail "backup SHA256 must be lowercase hex"
    local actual_sha
    actual_sha=$(sha256_file "$BACKUP")
    [[ "$actual_sha" == "$BACKUP_SHA256" ]] || fail "backup SHA256 mismatch"

    [[ -n "$ENV_FILE" ]] || fail "rehearsal environment file is required"
    require_mode_600 "rehearsal environment file" "$ENV_FILE"
    [[ -f "$MIGRATION_LEDGER_TOOL" ]] || fail "migration ledger tool is unavailable"
    EXPECTED_SCHEMA_MIGRATIONS=$(load_expected_migration_count) || fail "migration manifest is invalid"
    if awk -F= '$1 == "DATABASE_PASSWORD" || $1 == "POSTGRES_PASSWORD" || $1 == "PGPASSWORD" || $1 == "RADAR_POSTGRES_PASSWORD" {found=1} END {exit !found}' "$ENV_FILE"; then
        fail "rehearsal environment file must not contain a database password value"
    fi
    [[ -n "$POSTGRES_PASSWORD_FILE" ]] || fail "PostgreSQL password file is required"
    [[ -n "$DATABASE_PASSWORD_FILE" ]] || fail "database password file is required"
    [[ -n "$PGPASS_FILE" ]] || fail "database pgpass file is required"
    require_secret_file "PostgreSQL password file" "$POSTGRES_PASSWORD_FILE"
    require_secret_file "database password file" "$DATABASE_PASSWORD_FILE"
    require_secret_file "database pgpass file" "$PGPASS_FILE"
    require_digest "candidate control-plane digest" "$CANDIDATE_DIGEST"
    require_digest "candidate Worker digest" "$CANDIDATE_WORKER_DIGEST"
    require_digest "rollback control-plane digest" "$ROLLBACK_DIGEST"
    require_digest "rollback Worker digest" "$ROLLBACK_WORKER_DIGEST"
    require_image_binding "candidate control-plane" "$CANDIDATE_IMAGE" "$CANDIDATE_DIGEST"
    require_image_binding "candidate Worker" "$CANDIDATE_WORKER_IMAGE" "$CANDIDATE_WORKER_DIGEST"
    require_image_binding "rollback control-plane" "$ROLLBACK_IMAGE" "$ROLLBACK_DIGEST"
    require_image_binding "rollback Worker" "$ROLLBACK_WORKER_IMAGE" "$ROLLBACK_WORKER_DIGEST"

    [[ "$PROJECT_NAME" =~ ^[a-z0-9][a-z0-9_-]{2,62}-rehearsal$ ]] || \
        fail "rehearsal project must end with -rehearsal"
    [[ "$PROJECT_NAME" != "sub2api" && "$PROJECT_NAME" != *production* ]] || \
        fail "rehearsal project cannot be the production project"
    [[ "$RESOURCE_PREFIX" =~ ^[a-z0-9][a-z0-9_-]{2,62}-rehearsal$ ]] || \
        fail "rehearsal resource prefix must end with -rehearsal"
    [[ "$RESOURCE_PREFIX" != "sub2api" && "$RESOURCE_PREFIX" != *production* ]] || \
        fail "rehearsal resource prefix cannot be the production prefix"
    local normalized_host
    normalized_host=$(printf '%s' "$DATABASE_HOST" | tr '[:upper:]' '[:lower:]')
    [[ "$normalized_host" != "192.255.134.229" ]] || \
        fail "rehearsal database host cannot point to production database host"
    [[ "$normalized_host" != "sub2api-postgres" && "$normalized_host" != "production-postgres" ]] || \
        fail "rehearsal database host cannot point to production database host"
    [[ "$normalized_host" != *"production"* && "$normalized_host" != *"prod-"* ]] || \
        fail "rehearsal database host cannot contain a production marker"

    [[ "$DRY_RUN" == 0 || "$DRY_RUN" == 1 ]] || fail "RADAR_MIGRATION_REHEARSAL_DRY_RUN must be 0 or 1"
    [[ "$RETAIN_VOLUMES" == 0 || "$RETAIN_VOLUMES" == 1 ]] || \
        fail "RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES must be 0 or 1"
    [[ "$CLONE_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] || \
        fail "RADAR_MIGRATION_REHEARSAL_CLONE_TIMEOUT_SECONDS must be a strictly positive integer"
    validate_retention_authorization
    mkdir -p "$EVIDENCE_DIR"
}

docker_run() {
    if [[ "$DRY_RUN" == 1 ]]; then
        printf 'docker %q' "$1"
        shift
        printf ' %q' "$@"
        printf '\n'
    else
        docker "$@"
    fi
}

docker_exec() {
    if [[ "$DRY_RUN" == 1 ]]; then
        printf 'docker exec'
        printf ' %q' "$@"
        printf '\n'
    else
        docker exec "$@"
    fi
}

database_exec() {
    docker_exec -e PGPASSFILE=/run/secrets/radar-database.pgpass "$@"
}

cleanup() {
    local retain_at_cleanup=0
    [[ "$DRY_RUN" == 1 ]] && return 0
    [[ -n ${DOCKER_READY:-} ]] || return 0
    if [[ "$RETAIN_VOLUMES" == 1 ]] && retention_authorization_valid >/dev/null 2>&1; then
        retain_at_cleanup=1
    fi
    if [[ "$CONTAINERS_STARTED" == 1 ]]; then
        docker rm -f "$ROLLBACK_WORKER_CONTAINER" "$ROLLBACK_CONTAINER" "$CANDIDATE_CONTAINER" \
            "$REDIS_CONTAINER" "$ROLLBACK_DB_CONTAINER" "$DB_CONTAINER" >/dev/null 2>&1 || true
    fi
    if [[ "$NETWORK_CREATED" == 1 ]]; then
        docker network rm "$NETWORK" >/dev/null 2>&1 || true
    fi
    if [[ "$VOLUME_CREATED" == 1 && "$retain_at_cleanup" == 0 ]]; then
        docker volume rm "$VOLUME" >/dev/null 2>&1 || true
    fi
    if [[ "$ROLLBACK_VOLUME_CREATED" == 1 && "$retain_at_cleanup" == 0 ]]; then
        docker volume rm "$ROLLBACK_VOLUME" >/dev/null 2>&1 || true
    fi
}

write_summary() {
    local status=$1
    local rollback_worker_probe_ok=$2
    local summary_path=$EVIDENCE_DIR/summary.json
    python3 - "$summary_path" "$status" "$BACKUP_SHA256" "$CANDIDATE_DIGEST" \
        "$CANDIDATE_WORKER_DIGEST" "$ROLLBACK_DIGEST" "$ROLLBACK_WORKER_DIGEST" \
        "$PROJECT_NAME" "$rollback_worker_probe_ok" "$MIGRATION_224_CHECKSUM" \
        "$MIGRATION_225_CHECKSUM" "$MIGRATION_COUNT" "$MIGRATION_224_SEMANTICS_OK" \
        "$MIGRATION_225_SEMANTICS_OK" "$ROLLBACK_DATABASE_CLONE_USED" \
        "$EXPECTED_SCHEMA_MIGRATIONS" "$MIGRATION_LEDGER_OK" \
        "$MIGRATION_PENDING_JSON" "$MIGRATION_LEGACY_ENTRIES_JSON" \
        "$MIGRATION_CHECKSUM_MISMATCHES_JSON" "$BASELINE_SCHEMA_MIGRATIONS" \
        "$ACTUAL_SCHEMA_MIGRATIONS" "$BASELINE_LEDGER_SHA256" \
        "$CANDIDATE_LEDGER_SHA256" "$EXPECTED_CANDIDATE_LEDGER_SHA256" \
        "$EXPECTED_RUNTIME_LEDGER_SHA256" "$RUNTIME_LEDGER_SHA256" <<'PY'
import json
import pathlib
import sys
from datetime import datetime, timezone

(
    path,
    status,
    backup_sha256,
    candidate_digest,
    candidate_worker_digest,
    rollback_digest,
    rollback_worker_digest,
    project,
    rollback_worker_probe_ok,
    migration_224_checksum,
    migration_225_checksum,
    migration_count,
    migration_224_semantics_ok,
    migration_225_semantics_ok,
    rollback_database_clone_used,
    expected_schema_migrations,
    migration_ledger_ok,
    candidate_pending_migrations,
    legacy_entries,
    checksum_mismatches,
    baseline_schema_migrations,
    actual_schema_migrations,
    baseline_ledger_sha256,
    candidate_ledger_sha256,
    expected_candidate_ledger_sha256,
    expected_runtime_ledger_sha256,
    runtime_ledger_sha256,
) = sys.argv[1:]
document = {
    "schema_version": "radar-v01178-migration-rehearsal-v4",
    "recorded_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
    "status": status,
    "project": project,
    "backup_sha256": backup_sha256,
    "candidate_control_plane_digest": candidate_digest,
    "candidate_worker_digest": candidate_worker_digest,
    "rollback_control_plane_digest": rollback_digest,
    "rollback_worker_digest": rollback_worker_digest,
    "rollback_worker_probe_ok": rollback_worker_probe_ok == "true",
    "migration_224_checksum": migration_224_checksum or None,
    "migration_225_checksum": migration_225_checksum or None,
    "migration_count": int(migration_count) if migration_count else None,
    "baseline_schema_migrations": int(baseline_schema_migrations) if baseline_schema_migrations else None,
    "actual_schema_migrations": int(actual_schema_migrations) if actual_schema_migrations else None,
    "expected_schema_migrations": int(expected_schema_migrations) if expected_schema_migrations else None,
    "migration_ledger_ok": migration_ledger_ok == "true",
    "candidate_pending_migrations": json.loads(candidate_pending_migrations),
    "legacy_entries": json.loads(legacy_entries),
    "checksum_mismatches": json.loads(checksum_mismatches),
    "baseline_ledger_sha256": baseline_ledger_sha256 or None,
    "candidate_ledger_sha256": candidate_ledger_sha256 or None,
    "expected_candidate_ledger_sha256": expected_candidate_ledger_sha256 or None,
    "expected_runtime_ledger_sha256": expected_runtime_ledger_sha256 or None,
    "runtime_ledger_sha256": runtime_ledger_sha256 or None,
    "migration_224_semantics_ok": migration_224_semantics_ok == "true",
    "migration_225_semantics_ok": migration_225_semantics_ok == "true",
    "rollback_database_clone_used": rollback_database_clone_used == "true",
}
target = pathlib.Path(path)
target.parent.mkdir(parents=True, exist_ok=True)
target.write_text(json.dumps(document, sort_keys=True, indent=2) + "\n", encoding="utf-8")
target.chmod(0o600)
PY
}

render_plan() {
    printf 'migration rehearsal dry-run passed.\n'
    printf 'project=%s\n' "$PROJECT_NAME"
    printf 'postgres_volume=%s\n' "$VOLUME"
    printf 'rollback_postgres_volume=%s\n' "$ROLLBACK_VOLUME"
    printf 'postgres_network=%s\n' "$NETWORK"
    printf 'pg_restore --clean --if-exists --no-owner --dbname=%s\n' "$DATABASE_NAME"
    printf 'candidate_image=%s\n' "$CANDIDATE_IMAGE"
    printf 'candidate_worker_image=%s\n' "$CANDIDATE_WORKER_IMAGE"
    printf 'rollback_image=%s\n' "$ROLLBACK_IMAGE"
    printf 'rollback_worker_image=%s\n' "$ROLLBACK_WORKER_IMAGE"
    write_summary dry_run false
}

wait_for_database() {
    local container=${1:-$DB_CONTAINER}
    for _ in $(seq 1 "${RADAR_MIGRATION_REHEARSAL_DB_WAIT_ATTEMPTS:-60}"); do
        if database_exec "$container" pg_isready -U "$DATABASE_USER" -d "$DATABASE_NAME" >/dev/null 2>&1; then
            sleep 5
            return 0
        fi
        sleep 2
    done
    fail "rehearsal PostgreSQL did not become ready"
}

wait_for_health() {
    local container=$1
    for _ in $(seq 1 "${RADAR_MIGRATION_REHEARSAL_HEALTH_WAIT_ATTEMPTS:-60}"); do
        if docker_exec "$container" wget -q -T 5 -O /dev/null http://127.0.0.1:8080/health >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    fail "rehearsal container did not become healthy: $container"
}

db_query() {
    database_exec "$DB_CONTAINER" psql -Atqc "$1" -U "$DATABASE_USER" -d "$DATABASE_NAME"
}

record_migrations() {
    local path=$1
    local rows
    rows=$(db_query "SELECT filename || '|' || checksum FROM schema_migrations ORDER BY filename")
    printf '%s\n' "$rows" >"$path"
}

require_migrations_once() {
    local path=$1
    local name file checksum count actual
    for name in "${MIGRATION_NAMES[@]}"; do
        file="$MIGRATIONS_DIR/$name"
        [[ -f "$file" ]] || fail "required migration is missing: $name"
        checksum=$(migration_checksum "$file")
        count=$(awk -F'|' -v name="$name" '$1 == name {count++} END {print count + 0}' "$path")
        [[ "$count" == 1 ]] || fail "migration must be recorded exactly once: $name"
        actual=$(awk -F'|' -v name="$name" '$1 == name {print $2}' "$path")
        [[ "$actual" == "$checksum" ]] || fail "migration checksum mismatch: $name"
    done
}

require_migration_224_schema() {
    local legacy_count revision_count unrelated_count
    legacy_count=$(db_query "SELECT COUNT(*) FROM pg_constraint c WHERE c.conrelid='quality_reports'::regclass AND c.contype='u' AND (SELECT array_agg(a.attname ORDER BY a.attname) FROM unnest(c.conkey) k(attnum) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum)=ARRAY['model_alias','run_id','tenant_id']::name[]")
    revision_count=$(db_query "SELECT COUNT(*) FROM pg_constraint WHERE conrelid='quality_reports'::regclass AND conname='uq_quality_reports_tenant_run_model_revision'")
    # PostgreSQL derives the name of UNIQUE (id, tenant_id) from the column
    # names, so validate the preserved key by definition instead of a guessed
    # auto-generated constraint name.
    unrelated_count=$(db_query "SELECT COUNT(*) FROM pg_constraint c WHERE c.conrelid='quality_reports'::regclass AND c.contype='u' AND (SELECT array_agg(a.attname ORDER BY a.attname) FROM unnest(c.conkey) k(attnum) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum)=ARRAY['id','tenant_id']::name[]")
    [[ "$legacy_count" == 0 ]] || fail "migration 224 left the legacy quality-report constraint"
    [[ "$revision_count" == 1 ]] || fail "migration 224 revision constraint is missing or duplicated"
    [[ "$unrelated_count" == 1 ]] || fail "migration 224 removed an unrelated unique constraint"
}

require_migration_225_schema() {
    local pricing_columns disabled_count
    pricing_columns=$(db_query "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'groups' AND ((column_name = 'long_context_pricing_enabled' AND data_type = 'boolean' AND is_nullable = 'NO') OR (column_name = 'model_pricing' AND data_type = 'jsonb'))")
    disabled_count=$(db_query "SELECT COUNT(*) FROM groups WHERE long_context_pricing_enabled IS DISTINCT FROM TRUE")
    [[ "$pricing_columns" == 2 ]] || fail "migration 225 group pricing columns are invalid"
    [[ "$disabled_count" == 0 ]] || fail "migration 225 did not preserve long-context pricing defaults"
}

require_migration_224_revision_probe() {
    docker exec -i -e PGPASSFILE=/run/secrets/radar-database.pgpass "$DB_CONTAINER" \
        psql -v ON_ERROR_STOP=1 -U "$DATABASE_USER" -d "$DATABASE_NAME" <<'SQL'
BEGIN;
DO $$
DECLARE
    rehearsal_run_id UUID;
    rehearsal_tenant_id BIGINT;
    rehearsal_policy_version VARCHAR(100);
    rehearsal_alias TEXT := 'rehearsal-migration-224-' || substr(md5(clock_timestamp()::text || random()::text), 1, 24);
    report_count INTEGER;
BEGIN
    SELECT run.id, run.tenant_id, policy.version
    INTO rehearsal_run_id, rehearsal_tenant_id, rehearsal_policy_version
    FROM evaluation_runs run
    JOIN quality_policy_versions policy ON policy.tenant_id = run.tenant_id
    ORDER BY run.id
    LIMIT 1;

    IF rehearsal_run_id IS NULL THEN
        RAISE EXCEPTION 'migration 224 revision probe requires an evaluation run with a tenant policy';
    END IF;

    INSERT INTO quality_reports (
        id, tenant_id, run_id, model_alias, overall_conclusion, adulteration_risk,
        degradation_risk, policy_version, generated_at, fresh_until, aggregate_revision
    ) VALUES
        (gen_random_uuid(), rehearsal_tenant_id, rehearsal_run_id, rehearsal_alias,
         'no_significant_anomaly', 'no_significant_anomaly', 'no_significant_anomaly',
         rehearsal_policy_version, transaction_timestamp(), transaction_timestamp() + INTERVAL '1 hour', 0),
        (gen_random_uuid(), rehearsal_tenant_id, rehearsal_run_id, rehearsal_alias,
         'no_significant_anomaly', 'no_significant_anomaly', 'no_significant_anomaly',
         rehearsal_policy_version, transaction_timestamp(), transaction_timestamp() + INTERVAL '1 hour', 1);

    BEGIN
        INSERT INTO quality_reports (
            id, tenant_id, run_id, model_alias, overall_conclusion, adulteration_risk,
            degradation_risk, policy_version, generated_at, fresh_until, aggregate_revision
        ) VALUES (
            gen_random_uuid(), rehearsal_tenant_id, rehearsal_run_id, rehearsal_alias,
            'no_significant_anomaly', 'no_significant_anomaly', 'no_significant_anomaly',
            rehearsal_policy_version, transaction_timestamp(), transaction_timestamp() + INTERVAL '1 hour', 1
        );
        RAISE EXCEPTION 'migration 224 revision probe accepted a duplicate revision';
    EXCEPTION
        WHEN unique_violation THEN
            NULL;
    END;

    SELECT COUNT(*) INTO report_count
    FROM quality_reports
    WHERE tenant_id = rehearsal_tenant_id
      AND run_id = rehearsal_run_id
      AND model_alias = rehearsal_alias
      AND aggregate_revision IN (0, 1);
    IF report_count != 2 THEN
        RAISE EXCEPTION 'migration 224 revision probe expected two rows, found %', report_count;
    END IF;
END $$;
ROLLBACK;
SQL
}

require_historical_radar_migrations() {
    local path=$1
    local name file checksum count actual
    for name in \
        "191_add_radar_control_plane.sql" \
        "192_add_evaluation_sample_execution_identity.sql" \
        "193_add_radar_grading_statistics.sql"; do
        file="$MIGRATIONS_DIR/$name"
        [[ -f "$file" ]] || fail "historical Radar migration is missing: $name"
        checksum=$(migration_checksum "$file")
        count=$(awk -F'|' -v name="$name" '$1 == name {count++} END {print count + 0}' "$path")
        [[ "$count" == 1 ]] || fail "historical migration must be recorded exactly once: $name"
        actual=$(awk -F'|' -v name="$name" '$1 == name {print $2}' "$path")
        [[ "$actual" == "$checksum" ]] || fail "historical migration checksum mismatch: $name"
    done
}

clone_migrated_database() {
    python3 - "$CLONE_TIMEOUT_SECONDS" "$DB_CONTAINER" \
        "$ROLLBACK_DB_CONTAINER" "$DATABASE_USER" "$DATABASE_NAME" <<'PY'
import subprocess
import sys
import time

(
    timeout_seconds,
    source_container,
    target_container,
    database_user,
    database_name,
) = sys.argv[1:]
timeout = int(timeout_seconds)
deadline = time.monotonic() + timeout
dump_process = None
restore_process = None


def remaining_seconds() -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise subprocess.TimeoutExpired("rollback PostgreSQL clone", timeout)
    return remaining


def stop_process(process) -> None:
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=1)
    except subprocess.TimeoutExpired:
        process.kill()
        try:
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            return


try:
    dump_process = subprocess.Popen(
        [
            "docker",
            "exec",
            source_container,
            "pg_dump",
            "-Fc",
            "-U",
            database_user,
            database_name,
        ],
        stdout=subprocess.PIPE,
    )
    restore_process = subprocess.Popen(
        [
            "docker",
            "exec",
            "-i",
            "-e",
            "PGPASSFILE=/run/secrets/radar-database.pgpass",
            target_container,
            "pg_restore",
            "--clean",
            "--if-exists",
            "--no-owner",
            "-U",
            database_user,
            "-d",
            database_name,
        ],
        stdin=dump_process.stdout,
    )
    if dump_process.stdout is not None:
        dump_process.stdout.close()

    dump_returncode = dump_process.wait(timeout=remaining_seconds())
    restore_returncode = restore_process.wait(timeout=remaining_seconds())
except subprocess.TimeoutExpired:
    stop_process(dump_process)
    stop_process(restore_process)
    print(f"rollback PostgreSQL clone timed out after {timeout} seconds", file=sys.stderr)
    sys.exit(1)
except OSError as error:
    stop_process(dump_process)
    stop_process(restore_process)
    print(f"rollback PostgreSQL clone could not start: {error}", file=sys.stderr)
    sys.exit(1)

if dump_returncode != 0:
    print(f"rollback PostgreSQL clone dump failed with exit code {dump_returncode}", file=sys.stderr)
    sys.exit(dump_returncode)
if restore_returncode != 0:
    print(f"rollback PostgreSQL clone restore failed with exit code {restore_returncode}", file=sys.stderr)
    sys.exit(restore_returncode)
PY
}

run_rehearsal() {
    docker run -d --name "$DB_CONTAINER" --network "$NETWORK" \
        --network-alias "$DATABASE_HOST" \
        --label "radar.rehearsal.project=$PROJECT_NAME" \
        --mount "type=bind,src=$POSTGRES_PASSWORD_FILE,dst=/run/secrets/radar-postgres-password,readonly" \
        --mount "type=bind,src=$PGPASS_FILE,dst=/run/secrets/radar-database.pgpass,readonly" \
        -e POSTGRES_USER="$DATABASE_USER" -e POSTGRES_PASSWORD_FILE=/run/secrets/radar-postgres-password \
        -e POSTGRES_DB="$DATABASE_NAME" -v "$VOLUME:/var/lib/postgresql" "$POSTGRES_IMAGE" >/dev/null
    CONTAINERS_STARTED=1
    DATABASE_HOST=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$DB_CONTAINER")
    [[ -n "$DATABASE_HOST" ]] || fail "rehearsal PostgreSQL did not receive an isolated network address"
    wait_for_database
    docker run --rm --network "$NETWORK" -v "$BACKUP:/backup.dump:ro" \
        --mount "type=bind,src=$PGPASS_FILE,dst=/run/secrets/radar-database.pgpass,readonly" \
        -e PGPASSFILE=/run/secrets/radar-database.pgpass "$POSTGRES_IMAGE" \
        pg_restore --clean --if-exists --no-owner -h "$DATABASE_HOST" -U "$DATABASE_USER" -d "$DATABASE_NAME" /backup.dump
    record_migrations "${EVIDENCE_DIR}/migrations-before.txt"
    require_historical_radar_migrations "${EVIDENCE_DIR}/migrations-before.txt"

    docker run -d --name "$REDIS_CONTAINER" --network "$NETWORK" \
        --label "radar.rehearsal.project=$PROJECT_NAME" "$REDIS_IMAGE" >/dev/null
    docker run -d --name "$CANDIDATE_CONTAINER" --network "$NETWORK" \
        --label "radar.rehearsal.project=$PROJECT_NAME" --env-file "$ENV_FILE" \
        --mount "type=bind,src=$DATABASE_PASSWORD_FILE,dst=/run/secrets/radar-database-password,readonly" \
        -e AUTO_SETUP=true -e DATABASE_HOST="$DATABASE_HOST" -e DATABASE_PORT=5432 \
        -e DATABASE_USER="$DATABASE_USER" -e DATABASE_PASSWORD_FILE=/run/secrets/radar-database-password \
        -e DATABASE_DBNAME="$DATABASE_NAME" -e REDIS_HOST="$REDIS_CONTAINER" \
        --entrypoint /bin/sh "$CANDIDATE_IMAGE" \
        -ec 'export DATABASE_PASSWORD="$(cat "$DATABASE_PASSWORD_FILE")"; exec /app/docker-entrypoint.sh /app/sub2api' >/dev/null
    wait_for_health "$CANDIDATE_CONTAINER"
    record_migrations "${EVIDENCE_DIR}/migrations-candidate.txt"
    require_migrations_once "${EVIDENCE_DIR}/migrations-candidate.txt"
    MIGRATION_224_CHECKSUM=$(migration_checksum "$MIGRATIONS_DIR/224_add_quality_report_aggregate_revision.sql")
    MIGRATION_225_CHECKSUM=$(migration_checksum "$MIGRATIONS_DIR/221_group_model_pricing.sql")
    local ledger_output="${EVIDENCE_DIR}/migration-ledger.json"
    if ! run_migration_ledger "${EVIDENCE_DIR}/migrations-candidate.txt" "$ledger_output"; then
        fail "migration ledger validation failed"
    fi
    load_migration_ledger_summary "$ledger_output"
    [[ "$MIGRATION_LEDGER_OK" == true ]] || fail "migration ledger did not pass"
    MIGRATION_COUNT=$(db_query "SELECT COUNT(*) FROM schema_migrations")
    [[ "$MIGRATION_COUNT" == "$EXPECTED_SCHEMA_MIGRATIONS" ]] || \
        fail "schema_migrations count must match the authoritative migration manifest"
    [[ "$MIGRATION_COUNT" == "$ACTUAL_SCHEMA_MIGRATIONS" ]] || \
        fail "schema_migrations count does not match the migration ledger"
    require_migration_224_schema
    require_migration_224_revision_probe
    MIGRATION_224_SEMANTICS_OK=true
    require_migration_225_schema
    MIGRATION_225_SEMANTICS_OK=true
    docker restart "$CANDIDATE_CONTAINER" >/dev/null
    wait_for_health "$CANDIDATE_CONTAINER"
    record_migrations "${EVIDENCE_DIR}/migrations-candidate-restart.txt"
    cmp -s "${EVIDENCE_DIR}/migrations-candidate.txt" "${EVIDENCE_DIR}/migrations-candidate-restart.txt" || \
        fail "candidate restart changed schema_migrations"

    docker run -d --name "$ROLLBACK_DB_CONTAINER" --network "$NETWORK" \
        --label "radar.rehearsal.project=$PROJECT_NAME" \
        --mount "type=bind,src=$POSTGRES_PASSWORD_FILE,dst=/run/secrets/radar-postgres-password,readonly" \
        --mount "type=bind,src=$PGPASS_FILE,dst=/run/secrets/radar-database.pgpass,readonly" \
        -e POSTGRES_USER="$DATABASE_USER" -e POSTGRES_PASSWORD_FILE=/run/secrets/radar-postgres-password \
        -e POSTGRES_DB="$DATABASE_NAME" -v "$ROLLBACK_VOLUME:/var/lib/postgresql" "$POSTGRES_IMAGE" >/dev/null
    wait_for_database "$ROLLBACK_DB_CONTAINER"
    clone_migrated_database
    ROLLBACK_DATABASE_HOST=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ROLLBACK_DB_CONTAINER")
    [[ -n "$ROLLBACK_DATABASE_HOST" ]] || fail "rollback PostgreSQL clone has no isolated address"
    ROLLBACK_DATABASE_CLONE_USED=true

    docker run -d --name "$ROLLBACK_CONTAINER" --network "$NETWORK" \
        --label "radar.rehearsal.project=$PROJECT_NAME" --env-file "$ENV_FILE" \
        --mount "type=bind,src=$DATABASE_PASSWORD_FILE,dst=/run/secrets/radar-database-password,readonly" \
        -e AUTO_SETUP=true -e DATABASE_HOST="$ROLLBACK_DATABASE_HOST" -e DATABASE_PORT=5432 \
        -e DATABASE_USER="$DATABASE_USER" -e DATABASE_PASSWORD_FILE=/run/secrets/radar-database-password \
        -e DATABASE_DBNAME="$DATABASE_NAME" -e REDIS_HOST="$REDIS_CONTAINER" \
        --entrypoint /bin/sh "$ROLLBACK_IMAGE" \
        -ec 'export DATABASE_PASSWORD="$(cat "$DATABASE_PASSWORD_FILE")"; exec /app/docker-entrypoint.sh /app/sub2api' >/dev/null
    wait_for_health "$ROLLBACK_CONTAINER"
    docker run --rm --name "$ROLLBACK_WORKER_CONTAINER" --network "$NETWORK" \
        --entrypoint python -e RADAR_LIFECYCLE_PROTOCOL_VERSION=2 "$ROLLBACK_WORKER_IMAGE" \
        -c 'import importlib.metadata as m, os; '\
'assert m.version("sub2api-radar-worker"); '\
'assert os.environ["RADAR_LIFECYCLE_PROTOCOL_VERSION"] == "2"'
    write_summary passed true
    printf 'migration rehearsal passed.\n'
}

validate_inputs
if [[ "$DRY_RUN" == 1 ]]; then
    require_command docker
    docker version >/dev/null 2>&1 || fail "Docker is unavailable"
    render_plan
    exit 0
fi

require_command docker
require_command python3
mkdir -p "$EVIDENCE_DIR"
DOCKER_READY=1
trap 'cleanup' EXIT
for resource in "$DB_CONTAINER" "$ROLLBACK_DB_CONTAINER" "$REDIS_CONTAINER" "$CANDIDATE_CONTAINER" "$ROLLBACK_CONTAINER" \
    "$ROLLBACK_WORKER_CONTAINER"; do
    if docker container inspect "$resource" >/dev/null 2>&1; then
        fail "rehearsal container already exists: $resource"
    fi
done
if docker network inspect "$NETWORK" >/dev/null 2>&1; then
    fail "rehearsal network already exists: $NETWORK"
fi
if docker volume inspect "$VOLUME" >/dev/null 2>&1; then
    fail "rehearsal volume already exists: $VOLUME"
fi
if docker volume inspect "$ROLLBACK_VOLUME" >/dev/null 2>&1; then
    fail "rehearsal volume already exists: $ROLLBACK_VOLUME"
fi
docker network create --label "radar.rehearsal.project=$PROJECT_NAME" "$NETWORK" >/dev/null
NETWORK_CREATED=1
docker volume create --label "radar.rehearsal.project=$PROJECT_NAME" "$VOLUME" >/dev/null
VOLUME_CREATED=1
docker volume create --label "radar.rehearsal.project=$PROJECT_NAME" "$ROLLBACK_VOLUME" >/dev/null
ROLLBACK_VOLUME_CREATED=1
run_rehearsal
