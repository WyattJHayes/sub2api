#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.radar-staging.yml"
E2E_COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.radar-artifact-e2e.yml"
PROJECT_NAME=${RADAR_ARTIFACT_E2E_PROJECT:-sub2api-radar-artifact-e2e}
MINIO_PORT=${RADAR_ARTIFACT_E2E_MINIO_PORT:-19000}
CLAMAV_PORT=${RADAR_ARTIFACT_E2E_CLAMAV_PORT:-13310}
SSH_CONFIG=${RADAR_ARTIFACT_E2E_SSH_CONFIG:-}
SSH_HOST=${RADAR_ARTIFACT_E2E_SSH_HOST:-lima-colima}
TUNNEL_ACTIVE=false

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

[[ ${RADAR_ARTIFACT_LIVE_E2E:-} == "1" ]] || \
    fail "set RADAR_ARTIFACT_LIVE_E2E=1 to run live MinIO and ClamAV checks"
[[ "$PROJECT_NAME" =~ ^[a-z0-9][a-z0-9_-]{2,62}$ ]] || fail "invalid E2E project name"
[[ "$MINIO_PORT" =~ ^[0-9]+$ ]] && ((MINIO_PORT >= 1024 && MINIO_PORT <= 65535)) || \
    fail "invalid MinIO host port"
[[ "$CLAMAV_PORT" =~ ^[0-9]+$ ]] && ((CLAMAV_PORT >= 1024 && CLAMAV_PORT <= 65535)) || \
    fail "invalid ClamAV host port"

require_command docker
require_command go
require_command curl
require_command nc

compose_env=(
    "RADAR_COMPOSE_PROJECT_NAME=$PROJECT_NAME"
    "RADAR_COMPOSE_RESOURCE_PREFIX=$PROJECT_NAME"
    "RADAR_ARTIFACT_E2E_MINIO_PORT=$MINIO_PORT"
    "RADAR_ARTIFACT_E2E_CLAMAV_PORT=$CLAMAV_PORT"
    "RADAR_RELEASE_VERSION=artifact-e2e"
    "RADAR_RELEASE_COMMIT=0123456789abcdef0123456789abcdef01234567"
    "RADAR_RELEASE_DATE=2026-07-30T00:00:00Z"
    "RADAR_POSTGRES_PASSWORD=artifact-e2e-postgres-password"
    "RADAR_JWT_SECRET=artifact-e2e-jwt-secret-with-more-than-32-bytes"
    "RADAR_ADMIN_PASSWORD=artifact-e2e-admin-password"
    "RADAR_CONTEXT_SIGNING_KEY=artifact-e2e-context-signing-key-with-32-bytes"
    "RADAR_EVIDENCE_HASH_KEY=artifact-e2e-evidence-hash-key-with-32-bytes"
    "RADAR_API_WRITER_INSTANCE_ID=00000000-0000-4000-8000-000000000001"
    "RADAR_SYNTHETIC_UPSTREAM_API_KEY=artifact-e2e-synthetic-upstream-key"
    "RADAR_RUNNER_WORKER_TOKEN=artifact-e2e-runner-worker-token-more-than-32-bytes"
    "RADAR_GRADER_WORKER_TOKEN=artifact-e2e-grader-worker-token-more-than-32-bytes"
    "RADAR_STATISTICS_WORKER_TOKEN=artifact-e2e-statistics-worker-token-more-than-32-bytes"
    "RADAR_MINIO_ROOT_USER=radar-artifact-access"
    "RADAR_MINIO_ROOT_PASSWORD=artifact-e2e-minio-secret-more-than-32-bytes"
    "RADAR_ARTIFACT_STORAGE_BUCKET=radar-artifacts"
    "RADAR_ARTIFACT_STORAGE_REGION=us-east-1"
)

compose() {
    env "${compose_env[@]}" docker compose \
        -f "$COMPOSE_FILE" -f "$E2E_COMPOSE_FILE" "$@"
}

services_reachable() {
    curl -fsS --max-time 2 "http://127.0.0.1:$MINIO_PORT/minio/health/live" >/dev/null 2>&1 || return 1
    nc -z -w 2 127.0.0.1 "$CLAMAV_PORT" >/dev/null 2>&1 || return 1
}

detect_colima_ssh_config() {
    [[ -n "$SSH_CONFIG" ]] && return 0
    local docker_endpoint socket_path colima_root candidate
    docker_endpoint=$(docker context inspect --format '{{ (index .Endpoints "docker").Host }}' 2>/dev/null || true)
    socket_path=${docker_endpoint#unix://}
    case "$socket_path" in
        */.colima/*/docker.sock)
            colima_root=${socket_path%%/.colima/*}/.colima
            candidate="$colima_root/_lima/colima/ssh.config"
            [[ -r "$candidate" ]] && SSH_CONFIG=$candidate
            ;;
    esac
}

start_tunnel_if_needed() {
    services_reachable && return 0
    detect_colima_ssh_config
    [[ -n "$SSH_CONFIG" && -r "$SSH_CONFIG" ]] || \
        fail "artifact services are not reachable on localhost and no readable SSH tunnel config was found"
    require_command ssh
    ssh -F "$SSH_CONFIG" -o ExitOnForwardFailure=yes -fN \
        -L "127.0.0.1:$MINIO_PORT:127.0.0.1:$MINIO_PORT" \
        -L "127.0.0.1:$CLAMAV_PORT:127.0.0.1:$CLAMAV_PORT" \
        "$SSH_HOST"
    TUNNEL_ACTIVE=true
    for _ in {1..30}; do
        services_reachable && return 0
        sleep 1
    done
    fail "artifact services remain unreachable after SSH tunnel setup"
}

cleanup() {
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
    if [[ "$TUNNEL_ACTIVE" == "true" ]]; then
        ssh -F "$SSH_CONFIG" -O cancel \
            -L "127.0.0.1:$MINIO_PORT:127.0.0.1:$MINIO_PORT" \
            -L "127.0.0.1:$CLAMAV_PORT:127.0.0.1:$CLAMAV_PORT" \
            "$SSH_HOST" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

cleanup
compose config --quiet
compose up -d --wait --wait-timeout 300 minio-staging clamav-staging
compose run --rm --no-deps minio-init
start_tunnel_if_needed

(
    cd "$ROOT_DIR/backend"
    env \
        RADAR_ARTIFACT_LIVE_E2E=1 \
        RADAR_ARTIFACT_STORAGE_ENDPOINT="http://127.0.0.1:$MINIO_PORT" \
        RADAR_ARTIFACT_STORAGE_REGION=us-east-1 \
        RADAR_ARTIFACT_STORAGE_BUCKET=radar-artifacts \
        RADAR_ARTIFACT_STORAGE_ACCESS_KEY_ID=radar-artifact-access \
        RADAR_ARTIFACT_STORAGE_SECRET_ACCESS_KEY=artifact-e2e-minio-secret-more-than-32-bytes \
        RADAR_ARTIFACT_STORAGE_CLAMAV_ADDRESS="127.0.0.1:$CLAMAV_PORT" \
        go test -tags=integration ./internal/repository \
            -run '^TestEvaluationArtifactLiveE2EAcceptsCleanAndRejectsEICAR$' -count=1 -v
)

printf 'Radar artifact live E2E passed.\n'
