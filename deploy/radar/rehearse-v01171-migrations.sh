#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
MIGRATIONS_DIR=${RADAR_MIGRATIONS_DIR:-$ROOT_DIR/backend/migrations}
PROJECT_NAME=${RADAR_MIGRATION_REHEARSAL_PROJECT_NAME:-sub2api-radar-v11-rehearsal}
RESOURCE_PREFIX=${RADAR_MIGRATION_REHEARSAL_RESOURCE_PREFIX:-$PROJECT_NAME}
ENV_FILE=${RADAR_MIGRATION_REHEARSAL_ENV_FILE:-}
BACKUP=${RADAR_MIGRATION_REHEARSAL_BACKUP:-}
BACKUP_SHA256=${RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256:-}
DATABASE_HOST=${RADAR_MIGRATION_REHEARSAL_DATABASE_HOST:-radar-rehearsal-postgres}
DATABASE_NAME=${RADAR_MIGRATION_REHEARSAL_DATABASE_NAME:-radar}
DATABASE_USER=${RADAR_MIGRATION_REHEARSAL_DATABASE_USER:-radar}
DATABASE_PASSWORD=${RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD:-}
POSTGRES_IMAGE=${RADAR_MIGRATION_REHEARSAL_POSTGRES_IMAGE:-postgres:18-alpine}
REDIS_IMAGE=${RADAR_MIGRATION_REHEARSAL_REDIS_IMAGE:-redis:8-alpine}
CANDIDATE_IMAGE=${RADAR_CONTROL_PLANE_IMAGE:-}
CANDIDATE_DIGEST=${RADAR_CONTROL_PLANE_IMAGE_DIGEST:-}
ROLLBACK_IMAGE=${RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE:-}
ROLLBACK_DIGEST=${RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST:-}
EVIDENCE_DIR=${RADAR_MIGRATION_REHEARSAL_EVIDENCE_DIR:-}
DRY_RUN=${RADAR_MIGRATION_REHEARSAL_DRY_RUN:-0}

if [[ -z "$EVIDENCE_DIR" ]]; then
    EVIDENCE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/radar-v01171-rehearsal.XXXXXX")
    chmod 700 "$EVIDENCE_DIR"
fi

DB_CONTAINER="${RESOURCE_PREFIX}-postgres"
REDIS_CONTAINER="${RESOURCE_PREFIX}-redis"
CANDIDATE_CONTAINER="${RESOURCE_PREFIX}-candidate"
ROLLBACK_CONTAINER="${RESOURCE_PREFIX}-rollback"
NETWORK="${RESOURCE_PREFIX}-network"
VOLUME="${RESOURCE_PREFIX}-postgres"
NETWORK_CREATED=0
VOLUME_CREATED=0
CONTAINERS_STARTED=0

MIGRATION_NAMES=(
    "191_add_radar_control_plane.sql"
    "191_passkey_credentials.sql"
    "192_add_evaluation_sample_execution_identity.sql"
    "192_group_profit_control.sql"
    "193_add_radar_grading_statistics.sql"
    "193_group_profit_control_auth_cache_invalidation.sql"
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

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
        return 0
    fi
    command -v shasum >/dev/null 2>&1 || fail "required command is unavailable: sha256sum or shasum"
    shasum -a 256 "$1" | awk '{print $1}'
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
    if [[ -z "$DATABASE_PASSWORD" ]]; then
        DATABASE_PASSWORD=$(awk -F= '$1 == "DATABASE_PASSWORD" {sub(/^[^=]*=/, ""); print; exit}' "$ENV_FILE")
    fi
    require_digest "candidate control-plane digest" "$CANDIDATE_DIGEST"
    require_digest "rollback control-plane digest" "$ROLLBACK_DIGEST"
    require_image_binding "candidate control-plane" "$CANDIDATE_IMAGE" "$CANDIDATE_DIGEST"
    require_image_binding "rollback control-plane" "$ROLLBACK_IMAGE" "$ROLLBACK_DIGEST"

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
    [[ "$normalized_host" != "sub2api-postgres" && "$normalized_host" != "production-postgres" ]] || \
        fail "rehearsal database host cannot point to production database host"
    [[ "$normalized_host" != *"production"* && "$normalized_host" != *"prod-"* ]] || \
        fail "rehearsal database host cannot contain a production marker"

    [[ "$DRY_RUN" == 0 || "$DRY_RUN" == 1 ]] || fail "RADAR_MIGRATION_REHEARSAL_DRY_RUN must be 0 or 1"
    if [[ "$DRY_RUN" == 0 ]]; then
        [[ -n "$DATABASE_PASSWORD" ]] || fail "rehearsal database password is required"
    fi
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

cleanup() {
    [[ "$DRY_RUN" == 1 ]] && return 0
    [[ -n ${DOCKER_READY:-} ]] || return 0
    if [[ "$CONTAINERS_STARTED" == 1 ]]; then
        docker rm -f "$ROLLBACK_CONTAINER" "$CANDIDATE_CONTAINER" "$REDIS_CONTAINER" "$DB_CONTAINER" >/dev/null 2>&1 || true
    fi
    if [[ "$NETWORK_CREATED" == 1 ]]; then
        docker network rm "$NETWORK" >/dev/null 2>&1 || true
    fi
    if [[ "$VOLUME_CREATED" == 1 ]]; then
        docker volume rm "$VOLUME" >/dev/null 2>&1 || true
    fi
}

write_summary() {
    local status=$1
    local summary_path=$EVIDENCE_DIR/summary.json
    python3 - "$summary_path" "$status" "$BACKUP_SHA256" "$CANDIDATE_DIGEST" "$ROLLBACK_DIGEST" "$PROJECT_NAME" <<'PY'
import json
import pathlib
import sys
from datetime import datetime, timezone

path, status, backup_sha256, candidate_digest, rollback_digest, project = sys.argv[1:]
document = {
    "schema_version": "radar-v01171-migration-rehearsal-v1",
    "recorded_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
    "status": status,
    "project": project,
    "backup_sha256": backup_sha256,
    "candidate_control_plane_digest": candidate_digest,
    "rollback_control_plane_digest": rollback_digest,
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
    printf 'postgres_network=%s\n' "$NETWORK"
    printf 'pg_restore --clean --if-exists --no-owner --dbname=%s\n' "$DATABASE_NAME"
    printf 'candidate_image=%s\n' "$CANDIDATE_IMAGE"
    printf 'rollback_image=%s\n' "$ROLLBACK_IMAGE"
    write_summary dry_run
}

wait_for_database() {
    for _ in $(seq 1 "${RADAR_MIGRATION_REHEARSAL_DB_WAIT_ATTEMPTS:-60}"); do
        if docker_exec "$DB_CONTAINER" pg_isready -U "$DATABASE_USER" -d "$DATABASE_NAME" >/dev/null 2>&1; then
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
    docker_exec "$DB_CONTAINER" psql -Atqc "$1" -U "$DATABASE_USER" -d "$DATABASE_NAME"
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
        checksum=$(sha256_file "$file")
        count=$(awk -F'|' -v name="$name" '$1 == name {count++} END {print count + 0}' "$path")
        [[ "$count" == 1 ]] || fail "migration must be recorded exactly once: $name"
        actual=$(awk -F'|' -v name="$name" '$1 == name {print $2}' "$path")
        [[ "$actual" == "$checksum" ]] || fail "migration checksum mismatch: $name"
    done
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
        checksum=$(sha256_file "$file")
        count=$(awk -F'|' -v name="$name" '$1 == name {count++} END {print count + 0}' "$path")
        [[ "$count" == 1 ]] || fail "historical migration must be recorded exactly once: $name"
        actual=$(awk -F'|' -v name="$name" '$1 == name {print $2}' "$path")
        [[ "$actual" == "$checksum" ]] || fail "historical migration checksum mismatch: $name"
    done
}

run_rehearsal() {
    docker run -d --name "$DB_CONTAINER" --network "$NETWORK" \
        --network-alias "$DATABASE_HOST" \
        --label "radar.rehearsal.project=$PROJECT_NAME" \
        -e POSTGRES_USER="$DATABASE_USER" -e POSTGRES_PASSWORD="$DATABASE_PASSWORD" \
        -e POSTGRES_DB="$DATABASE_NAME" -v "$VOLUME:/var/lib/postgresql/data" "$POSTGRES_IMAGE" >/dev/null
    CONTAINERS_STARTED=1
    wait_for_database
    docker run --rm --network "$NETWORK" -v "$BACKUP:/backup.dump:ro" \
        -e PGPASSWORD="$DATABASE_PASSWORD" "$POSTGRES_IMAGE" \
        pg_restore --clean --if-exists --no-owner -h "$DATABASE_HOST" -U "$DATABASE_USER" -d "$DATABASE_NAME" /backup.dump
    record_migrations "${EVIDENCE_DIR}/migrations-before.txt"
    require_historical_radar_migrations "${EVIDENCE_DIR}/migrations-before.txt"

    docker run -d --name "$REDIS_CONTAINER" --network "$NETWORK" \
        --label "radar.rehearsal.project=$PROJECT_NAME" "$REDIS_IMAGE" >/dev/null
    docker run -d --name "$CANDIDATE_CONTAINER" --network "$NETWORK" \
        --label "radar.rehearsal.project=$PROJECT_NAME" --env-file "$ENV_FILE" \
        -e AUTO_SETUP=true -e DATABASE_HOST="$DATABASE_HOST" -e DATABASE_PORT=5432 \
        -e DATABASE_USER="$DATABASE_USER" -e DATABASE_PASSWORD="$DATABASE_PASSWORD" \
        -e DATABASE_DBNAME="$DATABASE_NAME" -e REDIS_HOST="$REDIS_CONTAINER" \
        "$CANDIDATE_IMAGE" >/dev/null
    wait_for_health "$CANDIDATE_CONTAINER"
    record_migrations "${EVIDENCE_DIR}/migrations-candidate.txt"
    require_migrations_once "${EVIDENCE_DIR}/migrations-candidate.txt"
    docker restart "$CANDIDATE_CONTAINER" >/dev/null
    wait_for_health "$CANDIDATE_CONTAINER"
    record_migrations "${EVIDENCE_DIR}/migrations-candidate-restart.txt"
    cmp -s "${EVIDENCE_DIR}/migrations-candidate.txt" "${EVIDENCE_DIR}/migrations-candidate-restart.txt" || \
        fail "candidate restart changed schema_migrations"

    docker run -d --name "$ROLLBACK_CONTAINER" --network "$NETWORK" \
        --label "radar.rehearsal.project=$PROJECT_NAME" --env-file "$ENV_FILE" \
        -e AUTO_SETUP=false -e DATABASE_HOST="$DATABASE_HOST" -e DATABASE_PORT=5432 \
        -e DATABASE_USER="$DATABASE_USER" -e DATABASE_PASSWORD="$DATABASE_PASSWORD" \
        -e DATABASE_DBNAME="$DATABASE_NAME" -e REDIS_HOST="$REDIS_CONTAINER" \
        "$ROLLBACK_IMAGE" >/dev/null
    wait_for_health "$ROLLBACK_CONTAINER"
    write_summary passed
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
for resource in "$DB_CONTAINER" "$REDIS_CONTAINER" "$CANDIDATE_CONTAINER" "$ROLLBACK_CONTAINER"; do
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
docker network create --label "radar.rehearsal.project=$PROJECT_NAME" "$NETWORK" >/dev/null
NETWORK_CREATED=1
docker volume create --label "radar.rehearsal.project=$PROJECT_NAME" "$VOLUME" >/dev/null
VOLUME_CREATED=1
run_rehearsal
