#!/usr/bin/env bash

set -uo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CLOSURE_TOOL="$ROOT_DIR/deploy/radar/local_prerelease_closure.py"
PREFLIGHT_TOOL="$ROOT_DIR/deploy/radar/local_prerelease_preflight.py"
ISOLATION_TOOL="$ROOT_DIR/deploy/radar/rehearsal_isolation_gate.py"
MIGRATION_TOOL="$ROOT_DIR/deploy/radar/rehearse-v01176-migrations.sh"
COMPOSE_ROLLBACK_TOOL="$ROOT_DIR/deploy/radar/local_compose_rollback.py"
FIXTURE_TOOL="$ROOT_DIR/deploy/radar/local_quality_fixture.py"
QUALITY_TOOL="$ROOT_DIR/deploy/tests/radar-quality-report-e2e.sh"
COMPOSE_BASE="$ROOT_DIR/deploy/docker-compose.radar-staging.yml"
COMPOSE_OVERRIDE="$ROOT_DIR/deploy/docker-compose.radar-rehearsal.yml"

DRY_RUN=${RADAR_LOCAL_PRERELEASE_DRY_RUN:-0}
RUN_ID=${RADAR_LOCAL_RUN_ID:-}
EVIDENCE_DIR=${RADAR_LOCAL_EVIDENCE_DIR:-}
BROWSER_ORIGIN=${RADAR_LOCAL_BROWSER_ORIGIN:-}
RETAIN_DEBUG=${RADAR_LOCAL_RETAIN_DEBUG:-0}
RETAIN_SECONDS=${RADAR_LOCAL_RETAIN_DEBUG_SECONDS:-3600}
HEALTH_TIMEOUT_SECONDS=${RADAR_LOCAL_HEALTH_TIMEOUT_SECONDS:-120}
CLOCK_MAX_SKEW_SECONDS=${RADAR_LOCAL_CLOCK_MAX_SKEW_SECONDS:-300}
TRUSTED_UTC=${RADAR_LOCAL_TRUSTED_UTC:-}
DOCKER_BIN=${RADAR_LOCAL_DOCKER_BIN:-docker}
TEST_DRIVER=${RADAR_LOCAL_TEST_DRIVER:-}
FRONTEND_RESULTS_DIR=${RADAR_LOCAL_FRONTEND_RESULTS_DIR:-$ROOT_DIR/frontend/test-results}
CLEANUP_ONLY=0
DOCKER_CLEANUP_ENABLED=0
RETENTION_AUTHORIZED=0
GATE2_PROJECT=
GATE4_PROJECT=
LOOPBACK_FORWARDER_PID=

# Database credentials for this workflow are private files referenced by rehearsal.env.
unset DATABASE_PASSWORD POSTGRES_PASSWORD PGPASSWORD RADAR_POSTGRES_PASSWORD \
    RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD

if [[ ${1:-} == "--cleanup-retained" && $# == 1 ]]; then
    CLEANUP_ONLY=1
    RETAIN_DEBUG=0
elif [[ $# != 0 ]]; then
    printf 'FAIL: unsupported arguments\n' >&2
    exit 2
fi

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    return 1
}

validate_run_id() {
    [[ "$RUN_ID" =~ ^[a-z0-9][a-z0-9_-]{2,62}-rehearsal$ ]] || fail "RADAR_LOCAL_RUN_ID is invalid"
}

validate_evidence_path() {
    [[ -n "$EVIDENCE_DIR" && "$EVIDENCE_DIR" == /* ]] || fail "RADAR_LOCAL_EVIDENCE_DIR must be an absolute path"
    [[ "$EVIDENCE_DIR" != / && "$EVIDENCE_DIR" != "$ROOT_DIR" ]] || fail "RADAR_LOCAL_EVIDENCE_DIR is unsafe"
}

migration_project() {
    local suffix=$1
    local project="${RUN_ID%-rehearsal}-${suffix}-rehearsal"
    if [[ ${#project} -gt 64 || ! "$project" =~ ^[a-z0-9][a-z0-9_-]*-rehearsal$ ]]; then
        fail "migration project for $suffix is invalid or exceeds 64 characters"
        return 1
    fi
    printf '%s' "$project"
}

validate_migration_projects() {
    GATE2_PROJECT=$(migration_project gate2) || return 1
    GATE4_PROJECT=$(migration_project gate4) || return 1
}

validate_loopback_origin() {
    if [[ "$BROWSER_ORIGIN" =~ ^http://(127\.0\.0\.1|localhost|\[::1\]):([1-9][0-9]{0,4})/?$ ]]; then
        (( 10#${BASH_REMATCH[2]} <= 65535 )) && return 0
    fi
    fail "RADAR_LOCAL_BROWSER_ORIGIN must be an explicit HTTP loopback origin"
}

validate_static_interfaces() {
    local path
    for path in "$CLOSURE_TOOL" "$PREFLIGHT_TOOL" "$ISOLATION_TOOL" "$MIGRATION_TOOL" "$COMPOSE_ROLLBACK_TOOL" "$FIXTURE_TOOL" "$QUALITY_TOOL" \
        "$COMPOSE_BASE" "$COMPOSE_OVERRIDE" "$ROOT_DIR/frontend/package.json" "$ROOT_DIR/frontend/playwright.config.ts"; do
        [[ -f "$path" ]] || fail "required local interface is unavailable"
    done
}

render_dry_run() {
    validate_static_interfaces || return 1
    if [[ -n "$RUN_ID" ]]; then
        validate_run_id || return 1
    fi
    if [[ -n "$BROWSER_ORIGIN" ]]; then
        validate_loopback_origin || return 1
    fi
    printf 'dry-run=validated-local-input-contract\n'
    printf 'phase=1 immutable-inputs-and-code\n'
    printf 'phase=2 migration-225\n'
    printf 'phase=3 radar-browser-workflows\n'
    printf 'phase=4 restart-and-rollback\n'
    printf 'phase=5 evidence-closure-input\n'
    printf 'bindings=<redacted-evidence>/bindings.json\n'
    printf 'private-logs=<redacted-evidence>/private/<gate>.log\n'
    printf 'public-closure=<redacted-evidence>/public/closure.json\n'
}

read_env_value() {
    local key=$1
    local file=$2
    local line
    while IFS= read -r line; do
        if [[ "$line" == "$key="* ]]; then
            printf '%s' "${line#*=}"
            return 0
        fi
    done <"$file"
    return 1
}

run_in_dir() {
    local directory=$1
    shift
    (cd "$directory" && "$@")
}

run_phase_driver() {
    local phase=$1
    shift
    if [[ -n "$TEST_DRIVER" ]]; then
        [[ -x "$TEST_DRIVER" ]] || return 2
        "$TEST_DRIVER" "$phase" "$@"
        return $?
    fi
    return 125
}

phase_1() {
    if [[ -n "$TEST_DRIVER" ]]; then run_phase_driver "immutable-inputs-and-code"; return $?; fi
    [[ -n "$TRUSTED_UTC" ]] || fail "RADAR_LOCAL_TRUSTED_UTC is required from an independent time source" || return $?
    python3 "$PREFLIGHT_TOOL" --candidate-root "$ROOT_DIR" || return $?
    python3 "$CLOSURE_TOOL" clock-sanity --trusted-utc "$TRUSTED_UTC" \
        --max-skew-seconds "$CLOCK_MAX_SKEW_SECONDS" \
        --output "$EVIDENCE_DIR/private/clock-sanity.json" || return $?
    run_in_dir "$ROOT_DIR/backend" go test ./... || return $?
    run_in_dir "$ROOT_DIR/backend" go test -tags unit ./internal/server/middleware || return $?
    run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev pytest -q || return $?
    run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev ruff check src tests || return $?
    run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev mypy src || return $?
    pnpm --dir "$ROOT_DIR/frontend" run lint:check || return $?
    pnpm --dir "$ROOT_DIR/frontend" run typecheck || return $?
    pnpm --dir "$ROOT_DIR/frontend" run test:run || return $?
    pnpm --dir "$ROOT_DIR/frontend" run build || return $?
    run_in_dir "$ROOT_DIR" python3 -m unittest discover -s deploy/radar -p 'test_*.py' -v || return $?
    run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev pytest "$ROOT_DIR/deploy/tests/test_radar_quality_report_contract.py" -q
}

run_migration_rehearsal() {
    local suffix=$1
    local output_dir=$2
    local project
    project=$(migration_project "$suffix") || return 1
    local environment_file="$EVIDENCE_DIR/rehearsal.env"
    local postgres_password_file database_password_file pgpass_file
    local control_image worker_image rollback_control_image rollback_worker_image
    local postgres_image redis_image
    postgres_password_file=$(read_env_value RADAR_POSTGRES_PASSWORD_FILE "$environment_file") || return 1
    database_password_file=$(read_env_value RADAR_DATABASE_PASSWORD_FILE "$environment_file") || return 1
    pgpass_file=$(read_env_value RADAR_DATABASE_PGPASS_FILE "$environment_file") || return 1
    control_image=$(read_env_value RADAR_CONTROL_PLANE_IMAGE "$environment_file") || return 1
    worker_image=$(read_env_value RADAR_WORKER_IMAGE "$environment_file") || return 1
    rollback_control_image=$(read_env_value RADAR_ROLLBACK_CONTROL_PLANE_IMAGE "$environment_file") || return 1
    rollback_worker_image=$(read_env_value RADAR_ROLLBACK_WORKER_IMAGE "$environment_file") || return 1
    postgres_image=$(read_env_value RADAR_POSTGRES_IMAGE "$environment_file") || return 1
    redis_image=$(read_env_value RADAR_REDIS_IMAGE "$environment_file") || return 1
    mkdir -p "$output_dir"
    chmod 700 "$output_dir"
    env \
        RADAR_MIGRATION_REHEARSAL_PROJECT_NAME="$project" \
        RADAR_MIGRATION_REHEARSAL_RESOURCE_PREFIX="$project" \
        RADAR_MIGRATION_REHEARSAL_ENV_FILE="$environment_file" \
        RADAR_MIGRATION_REHEARSAL_BACKUP="${RADAR_LOCAL_BACKUP_PATH:-}" \
        RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256="${RADAR_LOCAL_BACKUP_SHA256:-}" \
        RADAR_MIGRATION_REHEARSAL_POSTGRES_PASSWORD_FILE="$postgres_password_file" \
        RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD_FILE="$database_password_file" \
        RADAR_MIGRATION_REHEARSAL_PGPASS_FILE="$pgpass_file" \
        RADAR_MIGRATION_REHEARSAL_EVIDENCE_DIR="$output_dir" \
        RADAR_MIGRATION_REHEARSAL_POSTGRES_IMAGE="$postgres_image" \
        RADAR_MIGRATION_REHEARSAL_REDIS_IMAGE="$redis_image" \
        RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES="$RETAIN_DEBUG" \
        RADAR_MIGRATION_REHEARSAL_RETENTION_RECORD="$EVIDENCE_DIR/private/retention.json" \
        RADAR_MIGRATION_REHEARSAL_RETENTION_SECONDS="$RETAIN_SECONDS" \
        RADAR_MIGRATION_REHEARSAL_RETENTION_RUN_ID="$RUN_ID" \
        RADAR_MIGRATION_REHEARSAL_RETENTION_EVIDENCE_DIR="$EVIDENCE_DIR" \
        RADAR_MIGRATION_REHEARSAL_RETENTION_SCRIPT="$0" \
        RADAR_MIGRATION_REHEARSAL_RETENTION_GATE2_PROJECT="$GATE2_PROJECT" \
        RADAR_MIGRATION_REHEARSAL_RETENTION_GATE4_PROJECT="$GATE4_PROJECT" \
        RADAR_CONTROL_PLANE_IMAGE="$control_image" \
        RADAR_CONTROL_PLANE_IMAGE_DIGEST="${control_image#*@}" \
        RADAR_WORKER_IMAGE="$worker_image" \
        RADAR_WORKER_IMAGE_DIGEST="${worker_image#*@}" \
        RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE="$rollback_control_image" \
        RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST="${rollback_control_image#*@}" \
        RADAR_V10_ROLLBACK_WORKER_IMAGE="$rollback_worker_image" \
        RADAR_V10_ROLLBACK_WORKER_IMAGE_DIGEST="${rollback_worker_image#*@}" \
        bash "$MIGRATION_TOOL"
}

phase_2() {
    if [[ -n "$TEST_DRIVER" ]]; then run_phase_driver "migration-225" "$GATE2_PROJECT" "$GATE2_PROJECT"; return $?; fi
    run_migration_rehearsal gate2 "$EVIDENCE_DIR/private/gate-2-migration"
}

compose_command() {
    "$DOCKER_BIN" compose --env-file "$EVIDENCE_DIR/rehearsal.env" \
        -f "$COMPOSE_BASE" -f "$COMPOSE_OVERRIDE" "$@"
}

start_loopback_forwarder() {
    local browser_port upstream_ip browser_host
    if [[ "$BROWSER_ORIGIN" =~ ^http://\[::1\]:([1-9][0-9]{0,4})/?$ ]]; then
        browser_host='::1'
        browser_port=${BASH_REMATCH[1]}
    elif [[ "$BROWSER_ORIGIN" =~ ^http://(127\.0\.0\.1|localhost):([1-9][0-9]{0,4})/?$ ]]; then
        browser_host='127.0.0.1'
        browser_port=${BASH_REMATCH[2]}
    else
        fail "loopback forwarder requires an explicit HTTP loopback origin"
        return 1
    fi
    upstream_ip=$("$DOCKER_BIN" inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
        "${RUN_ID}-sub2api-rehearsal" 2>/dev/null) || {
        fail "control-plane container IP is unavailable"
        return 1
    }
    [[ "$upstream_ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || {
        fail "control-plane container IP is invalid"
        return 1
    }
    python3 -u - "$browser_host" "$browser_port" "$upstream_ip" >"$EVIDENCE_DIR/private/loopback-forwarder.log" 2>&1 <<'PY' &
import select
import socket
import socketserver
import sys

listen_host, listen_port, upstream_host = sys.argv[1], int(sys.argv[2]), sys.argv[3]


class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        upstream = socket.create_connection((upstream_host, 8080), timeout=5)
        try:
            peers = (self.request, upstream)
            while True:
                readable, _, _ = select.select(peers, [], [], 30)
                if not readable:
                    continue
                for source in readable:
                    data = source.recv(65536)
                    if not data:
                        return
                    (upstream if source is self.request else self.request).sendall(data)
        finally:
            upstream.close()


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


if listen_host == '::1':
    Server.address_family = socket.AF_INET6
with Server((listen_host, listen_port), Handler) as server:
    print('loopback-forwarder-ready', flush=True)
    server.serve_forever()
PY
    LOOPBACK_FORWARDER_PID=$!
    local forwarder_pid_file="$EVIDENCE_DIR/private/loopback-forwarder.pid"
    printf '%s\n' "$LOOPBACK_FORWARDER_PID" >"$forwarder_pid_file"
    chmod 600 "$forwarder_pid_file"
    local deadline=$((SECONDS + 15))
    while (( SECONDS <= deadline )); do
        if ! kill -0 "$LOOPBACK_FORWARDER_PID" 2>/dev/null; then
            fail "loopback forwarder exited before readiness"
            return 1
        fi
        if curl --fail --silent --show-error --max-time 2 "$BROWSER_ORIGIN/health" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    fail "loopback forwarder did not expose candidate health"
    return 1
}

initialize_evidence_signing_key() {
    local postgres_container database_user database_name signing_key_reference
    postgres_container="${RUN_ID}-postgres-rehearsal"
    database_user=$(read_env_value RADAR_POSTGRES_USER "$EVIDENCE_DIR/rehearsal.env") || database_user=radar
    database_name=$(read_env_value RADAR_POSTGRES_DB "$EVIDENCE_DIR/rehearsal.env") || database_name=radar
    signing_key_reference='env:RADAR_EVIDENCE_HASH_KEY'
    "$DOCKER_BIN" exec -i -e PGPASSFILE=/run/secrets/radar-database.pgpass "$postgres_container" \
        psql -v ON_ERROR_STOP=1 -U "$database_user" -d "$database_name" \
        -v signing_key_reference="$signing_key_reference" <<'SQL'
INSERT INTO evaluation_evidence_signing_keys (id, key_reference, status, state_epoch)
SELECT gen_random_uuid(), :'signing_key_reference', 'active', 1
WHERE NOT EXISTS (
    SELECT 1 FROM evaluation_evidence_signing_keys WHERE status = 'active'
);
SQL
}

verify_runtime_isolation() {
    local environment_file="$EVIDENCE_DIR/rehearsal.env"
    local key image
    local -a command=(python3 "$ISOLATION_TOOL" runtime --docker-bin "$DOCKER_BIN" --run-id "$RUN_ID" \
        --output "$EVIDENCE_DIR/private/runtime-isolation.json")
    for key in RADAR_CONTROL_PLANE_IMAGE RADAR_WORKER_IMAGE RADAR_POSTGRES_IMAGE RADAR_REDIS_IMAGE \
        RADAR_MINIO_IMAGE RADAR_MINIO_MC_IMAGE RADAR_CLAMAV_IMAGE; do
        image=$(read_env_value "$key" "$environment_file") || return 1
        command+=(--image "$image")
    done
    "${command[@]}"
}

stop_loopback_forwarder() {
    local forwarder_pid_file="$EVIDENCE_DIR/private/loopback-forwarder.pid"
    local pid=$LOOPBACK_FORWARDER_PID
    if [[ -z "$pid" && -f "$forwarder_pid_file" ]]; then
        pid=$(sed -n '1p' "$forwarder_pid_file")
    fi
    LOOPBACK_FORWARDER_PID=
    rm -f -- "$forwarder_pid_file" 2>/dev/null || true
    [[ -n "$pid" ]] || return 0
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
}

wait_for_candidate_health() {
    local deadline=$((SECONDS + HEALTH_TIMEOUT_SECONDS))
    while (( SECONDS <= deadline )); do
        if curl --fail --silent --show-error --max-time 5 "$BROWSER_ORIGIN/health" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    fail "candidate health did not become ready within the bounded timeout"
}

phase_3() {
    if [[ -n "$TEST_DRIVER" ]]; then run_phase_driver "radar-browser-workflows"; return $?; fi
    local environment_file="$EVIDENCE_DIR/rehearsal.env"
    local administrator_email=${RADAR_LOCAL_SETUP_ADMIN_EMAIL:-radar-admin@staging.local}
    local administrator_password fixture_password synthetic_key
    administrator_password=$(read_env_value RADAR_ADMIN_PASSWORD "$environment_file") || return 1
    synthetic_key=$(read_env_value RADAR_SYNTHETIC_UPSTREAM_API_KEY "$environment_file") || return 1
    fixture_password=${RADAR_LOCAL_FIXTURE_PASSWORD:-}
    [[ -n "$fixture_password" ]] || return 1
    compose_command up -d --no-build --pull never --wait --wait-timeout "$HEALTH_TIMEOUT_SECONDS" || return $?
    verify_runtime_isolation || return $?
    initialize_evidence_signing_key || return $?
    start_loopback_forwarder || return $?

    env RADAR_SETUP_ADMINISTRATOR_PASSWORD="$administrator_password" \
        RADAR_FIXTURE_PASSWORD="$fixture_password" RADAR_SYNTHETIC_UPSTREAM_KEY="$synthetic_key" \
        python3 "$FIXTURE_TOOL" --origin "$BROWSER_ORIGIN" --setup-administrator-email "$administrator_email" \
        --run-identifier "$RUN_ID" --manifest "$EVIDENCE_DIR/private/fixture-manifest.json" \
        --worker-bindings "$EVIDENCE_DIR/bindings.json" --worker-environment "$environment_file" \
        --worker-registration-output "$EVIDENCE_DIR/private/worker-registration.json" || return $?

    env RADAR_FIXTURE_PASSWORD="$fixture_password" python3 "$CLOSURE_TOOL" prepare-runtime \
        --origin "$BROWSER_ORIGIN" --run-id "$RUN_ID" --env-file "$environment_file" \
        --administrator-email "$administrator_email" --output "$EVIDENCE_DIR/private/runtime.env" || return $?

    local administrator_token user_token
    administrator_token=$(read_env_value RADAR_QUALITY_STAGING_ADMIN_TOKEN "$EVIDENCE_DIR/private/runtime.env") || return 1
    user_token=$(read_env_value RADAR_QUALITY_STAGING_USER_TOKEN "$EVIDENCE_DIR/private/runtime.env") || return 1

    env RADAR_QUALITY_STAGING_E2E=1 RADAR_QUALITY_STAGING_URL="$BROWSER_ORIGIN" \
        RADAR_QUALITY_FIXTURE_MANIFEST="$EVIDENCE_DIR/private/fixture-manifest.json" \
        RADAR_QUALITY_STAGING_ADMIN_TOKEN="$administrator_token" \
        RADAR_QUALITY_STAGING_USER_TOKEN="$user_token" \
        "$QUALITY_TOOL" || return $?
    env RADAR_E2E_BASE_URL="$BROWSER_ORIGIN" RADAR_E2E_ADMIN_EMAIL="$administrator_email" \
        RADAR_E2E_ADMIN_PASSWORD="$administrator_password" \
        RADAR_E2E_USER_EMAIL="radar-quality-${RUN_ID}@example.invalid" \
        RADAR_E2E_USER_PASSWORD="$fixture_password" \
        RADAR_E2E_FIXTURE_MANIFEST="$EVIDENCE_DIR/private/fixture-manifest.json" \
        pnpm --dir "$ROOT_DIR/frontend" run test:e2e
}

verify_migration_replay() {
    python3 - "$EVIDENCE_DIR/private/gate-2-migration/summary.json" "$EVIDENCE_DIR/private/gate-4-migration/summary.json" <<'PY'
import json
import pathlib
import sys

left = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
right = json.loads(pathlib.Path(sys.argv[2]).read_text(encoding="utf-8"))
required_fields = (
    "migration_224_checksum", "migration_225_checksum", "migration_count",
    "baseline_schema_migrations", "actual_schema_migrations",
    "expected_schema_migrations", "migration_ledger_ok",
    "candidate_pending_migrations", "legacy_entries", "checksum_mismatches",
    "baseline_ledger_sha256", "candidate_ledger_sha256",
    "expected_candidate_ledger_sha256", "expected_runtime_ledger_sha256",
    "runtime_ledger_sha256",
)
for field in required_fields:
    if field not in left or field not in right:
        raise SystemExit(f"migration replay missing {field}")
    if left.get(field) is None or left.get(field) != right.get(field):
        raise SystemExit(f"migration replay {field} mismatch")
if right.get("migration_ledger_ok") is not True:
    raise SystemExit("migration replay did not close the v0.2.0 ledger")
if right.get("candidate_pending_migrations") != [] or right.get("checksum_mismatches") != []:
    raise SystemExit("migration replay contains pending or mismatched files")
if right.get("candidate_ledger_sha256") != right.get("expected_candidate_ledger_sha256"):
    raise SystemExit("candidate ledger fingerprint does not match expected ledger")
if right.get("runtime_ledger_sha256") != right.get("expected_runtime_ledger_sha256"):
    raise SystemExit("runtime ledger fingerprint does not match expected ledger")
if right.get("actual_schema_migrations") != right.get("migration_count"):
    raise SystemExit("actual schema migration count does not match runtime count")
if not right.get("rollback_database_clone_used"):
    raise SystemExit("rollback did not use a database clone")
PY
}

quality_smoke() {
    local runtime_file="$EVIDENCE_DIR/private/runtime.env"
    local administrator_token user_token
    administrator_token=$(read_env_value RADAR_QUALITY_STAGING_ADMIN_TOKEN "$runtime_file") || return 1
    user_token=$(read_env_value RADAR_QUALITY_STAGING_USER_TOKEN "$runtime_file") || return 1
    env RADAR_QUALITY_STAGING_E2E=1 RADAR_QUALITY_STAGING_URL="$BROWSER_ORIGIN" \
        RADAR_QUALITY_FIXTURE_MANIFEST="$EVIDENCE_DIR/private/fixture-manifest.json" \
        RADAR_QUALITY_STAGING_ADMIN_TOKEN="$administrator_token" \
        RADAR_QUALITY_STAGING_USER_TOKEN="$user_token" \
        "$QUALITY_TOOL"
}

playwright_smoke() {
    local runtime_file="$EVIDENCE_DIR/private/runtime.env"
    local administrator_email administrator_password user_email user_password
    administrator_email=$(read_env_value RADAR_E2E_ADMIN_EMAIL "$runtime_file") || return 1
    administrator_password=$(read_env_value RADAR_E2E_ADMIN_PASSWORD "$runtime_file") || return 1
    user_email=$(read_env_value RADAR_E2E_USER_EMAIL "$runtime_file") || return 1
    user_password=$(read_env_value RADAR_E2E_USER_PASSWORD "$runtime_file") || return 1
    env RADAR_E2E_BASE_URL="$BROWSER_ORIGIN" RADAR_E2E_ADMIN_EMAIL="$administrator_email" \
        RADAR_E2E_ADMIN_PASSWORD="$administrator_password" RADAR_E2E_USER_EMAIL="$user_email" \
        RADAR_E2E_USER_PASSWORD="$user_password" \
        RADAR_E2E_FIXTURE_MANIFEST="$EVIDENCE_DIR/private/fixture-manifest.json" \
        pnpm --dir "$ROOT_DIR/frontend" exec playwright test --grep @smoke
}

run_primary_compose_rollback() {
    local environment_file="$EVIDENCE_DIR/rehearsal.env"
    local database_password_file pgpass_file postgres_image rollback_control_image
    database_password_file=$(read_env_value RADAR_DATABASE_PASSWORD_FILE "$environment_file") || return 1
    pgpass_file=$(read_env_value RADAR_DATABASE_PGPASS_FILE "$environment_file") || return 1
    postgres_image=$(read_env_value RADAR_POSTGRES_IMAGE "$environment_file") || return 1
    rollback_control_image=$(read_env_value RADAR_ROLLBACK_CONTROL_PLANE_IMAGE "$environment_file") || return 1
    local output_dir="$EVIDENCE_DIR/private/gate-4-primary-rollback"
    mkdir -p "$output_dir"
    chmod 700 "$output_dir"
    local -a command=(python3 "$COMPOSE_ROLLBACK_TOOL" --run-id "$RUN_ID" --docker-bin "$DOCKER_BIN" \
        --environment-file "$environment_file" --postgres-image "$postgres_image" \
        --rollback-control-plane-image "$rollback_control_image" \
        "--database-password-file" "$database_password_file" "--pgpass-file" "$pgpass_file" \
        --output "$output_dir/summary.json" --timeout-seconds "$HEALTH_TIMEOUT_SECONDS")
    if [[ "$RETAIN_DEBUG" == 1 ]]; then command+=(--retain-volume); fi
    "${command[@]}"
}

phase_4() {
    if [[ -n "$TEST_DRIVER" ]]; then
        run_phase_driver "candidate-restart-smoke" || return $?
        run_phase_driver "primary-compose-clone-rollback" || return $?
        run_phase_driver "candidate-primary-restore" || return $?
        run_phase_driver "fresh-migration-replay" "$GATE4_PROJECT" "$GATE4_PROJECT" || return $?
        verify_migration_replay
        return $?
    fi
    compose_command restart sub2api-staging || return $?
    wait_for_candidate_health || return $?
    playwright_smoke || return $?
    quality_smoke || return $?
    compose_command stop radar-runner radar-grader radar-statistics sub2api-staging || return $?
    run_primary_compose_rollback || return $?
    compose_command start sub2api-staging || return $?
    wait_for_candidate_health || return $?
    compose_command start radar-runner radar-grader radar-statistics || return $?
    quality_smoke || return $?
    run_migration_rehearsal gate4 "$EVIDENCE_DIR/private/gate-4-migration" || return $?
    verify_migration_replay
}

phase_5() {
    if [[ -n "$TEST_DRIVER" ]]; then
        run_phase_driver "evidence-closure-input" || return $?
        return 97
    fi
    [[ -f "$EVIDENCE_DIR/gate-1-code.json" && -f "$EVIDENCE_DIR/gate-2-migration.json" \
        && -f "$EVIDENCE_DIR/gate-3-radar.json" && -f "$EVIDENCE_DIR/gate-4-restart-rollback.json" ]]
}

write_failed_closure() {
    local gate=$1
    local code=$2
    local command=(python3 "$CLOSURE_TOOL" write-failure --evidence-dir "$EVIDENCE_DIR" \
        --run-id "$RUN_ID" --failed-gate "$gate" --exit-code "$code")
    if [[ -f "$EVIDENCE_DIR/bindings.json" ]]; then
        command+=(--bindings "$EVIDENCE_DIR/bindings.json")
    fi
    "${command[@]}" >/dev/null 2>&1 || true
}

run_gate() {
    local gate_name=$1
    local phase_function=$2
    local log_file="$EVIDENCE_DIR/private/${gate_name}.log"
    local started_at finished_at code status writer_code redactor_code writer_redactor_code
    local -a pipeline_status
    started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    : >"$log_file"
    chmod 600 "$log_file"
    "$phase_function" 2>&1 | python3 "$CLOSURE_TOOL" redact-stream >"$log_file"
    pipeline_status=("${PIPESTATUS[@]}")
    code=${pipeline_status[0]:-1}
    redactor_code=${pipeline_status[1]:-1}
    finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    if [[ $redactor_code -ne 0 ]]; then
        : >"$log_file"
        chmod 600 "$log_file"
        [[ $code -ne 0 ]] || code=$redactor_code
    fi
    status=passed
    [[ $code -eq 0 ]] || status=failed
    if [[ -f "$EVIDENCE_DIR/bindings.json" ]]; then
        python3 "$CLOSURE_TOOL" write-gate --evidence-dir "$EVIDENCE_DIR" \
            --bindings "$EVIDENCE_DIR/bindings.json" --gate "$gate_name" --status "$status" \
            --exit-code "$code" --started-at "$started_at" --finished-at "$finished_at" \
            2>&1 | python3 "$CLOSURE_TOOL" redact-stream >>"$log_file"
        pipeline_status=("${PIPESTATUS[@]}")
        writer_code=${pipeline_status[0]:-1}
        writer_redactor_code=${pipeline_status[1]:-1}
        if [[ $writer_code -ne 0 && $code -eq 0 ]]; then code=$writer_code; fi
        if [[ $writer_redactor_code -ne 0 && $code -eq 0 ]]; then code=$writer_redactor_code; fi
    elif [[ $code -eq 0 ]]; then
        code=1
    fi
    if [[ $code -ne 0 ]]; then
        write_failed_closure "$gate_name" "$code"
    fi
    return "$code"
}

verified_compose_resource_ids() {
    local kind=$1
    local listed id label
    local failed=0
    if [[ "$kind" == container ]]; then
        listed=$("$DOCKER_BIN" ps -aq --filter "label=com.docker.compose.project=$RUN_ID" 2>/dev/null) || return 1
        while IFS= read -r id; do
            [[ -n "$id" ]] || continue
            if ! label=$("$DOCKER_BIN" container inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$id" 2>/dev/null); then
                failed=1
                continue
            fi
            if [[ "$label" == "$RUN_ID" ]]; then printf '%s\n' "$id"; else failed=1; fi
        done <<<"$listed"
    else
        listed=$("$DOCKER_BIN" "$kind" ls -q --filter "label=com.docker.compose.project=$RUN_ID" 2>/dev/null) || return 1
        while IFS= read -r id; do
            [[ -n "$id" ]] || continue
            if ! label=$("$DOCKER_BIN" "$kind" inspect --format '{{ index .Labels "com.docker.compose.project" }}' "$id" 2>/dev/null); then
                failed=1
                continue
            fi
            if [[ "$label" == "$RUN_ID" ]]; then printf '%s\n' "$id"; else failed=1; fi
        done <<<"$listed"
    fi
    return "$failed"
}

verified_migration_resource_ids() {
    local kind=$1
    local project=$2
    local listed id label
    local failed=0
    if [[ "$kind" == container ]]; then
        listed=$("$DOCKER_BIN" ps -aq --filter "label=radar.rehearsal.project=$project" 2>/dev/null) || return 1
        while IFS= read -r id; do
            [[ -n "$id" ]] || continue
            if ! label=$("$DOCKER_BIN" container inspect --format '{{ index .Config.Labels "radar.rehearsal.project" }}' "$id" 2>/dev/null); then
                failed=1
                continue
            fi
            if [[ "$label" == "$project" ]]; then printf '%s\n' "$id"; else failed=1; fi
        done <<<"$listed"
    else
        listed=$("$DOCKER_BIN" "$kind" ls -q --filter "label=radar.rehearsal.project=$project" 2>/dev/null) || return 1
        while IFS= read -r id; do
            [[ -n "$id" ]] || continue
            if ! label=$("$DOCKER_BIN" "$kind" inspect --format '{{ index .Labels "radar.rehearsal.project" }}' "$id" 2>/dev/null); then
                failed=1
                continue
            fi
            if [[ "$label" == "$project" ]]; then printf '%s\n' "$id"; else failed=1; fi
        done <<<"$listed"
    fi
    return "$failed"
}

write_retention_record() {
    local retention_file="$EVIDENCE_DIR/private/retention.json"
    python3 - "$retention_file" "$RETAIN_SECONDS" "$RUN_ID" "$EVIDENCE_DIR" "$0" \
        "$GATE2_PROJECT" "$GATE4_PROJECT" <<'PY'
import json
import os
import pathlib
import shlex
import sys
import tempfile
from datetime import datetime, timedelta, timezone

path, seconds_text, run_id, evidence_dir, script, gate2_project, gate4_project = sys.argv[1:]
seconds = int(seconds_text)
deadline = datetime.now(timezone.utc) + timedelta(seconds=seconds)
command = shlex.join([
    "env", f"RADAR_LOCAL_RUN_ID={run_id}", f"RADAR_LOCAL_EVIDENCE_DIR={evidence_dir}",
    script, "--cleanup-retained",
])
document = {
    "schema_version": "radar-local-retention-v1",
    "deadline": deadline.isoformat().replace("+00:00", "Z"),
    "deadline_seconds": seconds,
    "cleanup_command": command,
    "migration_projects": [gate2_project, gate4_project],
}
target = pathlib.Path(path)
descriptor, temporary_name = tempfile.mkstemp(prefix=f".{target.name}.", dir=target.parent)
temporary = pathlib.Path(temporary_name)
try:
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        json.dump(document, stream, sort_keys=True, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(temporary, 0o600)
    os.replace(temporary, target)
    os.chmod(target, 0o600)
finally:
    temporary.unlink(missing_ok=True)
PY
}

retention_record_valid() {
    local retention_file="$EVIDENCE_DIR/private/retention.json"
    [[ -f "$retention_file" && -r "$retention_file" ]] || return 1
    local mode
    if mode=$(stat -c '%a' "$retention_file" 2>/dev/null); then
        :
    elif mode=$(stat -f '%Lp' "$retention_file" 2>/dev/null); then
        :
    else
        return 1
    fi
    [[ "$mode" == 600 ]] || return 1
    python3 - "$retention_file" "$RETAIN_SECONDS" "$RUN_ID" "$EVIDENCE_DIR" "$0" \
        "$GATE2_PROJECT" "$GATE4_PROJECT" <<'PY'
import json
import pathlib
import shlex
import sys
from datetime import datetime, timezone

path, seconds_text, run_id, evidence_dir, script, gate2_project, gate4_project = sys.argv[1:]
try:
    document = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
except (OSError, json.JSONDecodeError) as error:
    raise SystemExit(f"retention authorization record is invalid: {error}")
if not isinstance(document, dict) or set(document) != {
    "schema_version", "deadline", "deadline_seconds", "cleanup_command", "migration_projects"
}:
    raise SystemExit("retention authorization record has an invalid schema")
if document.get("schema_version") != "radar-local-retention-v1":
    raise SystemExit("retention authorization record has an invalid version")
seconds = document.get("deadline_seconds")
if type(seconds) is not int or seconds <= 0 or seconds > 86400 or seconds != int(seconds_text):
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
expected_command = shlex.join([
    "env", f"RADAR_LOCAL_RUN_ID={run_id}", f"RADAR_LOCAL_EVIDENCE_DIR={evidence_dir}",
    script, "--cleanup-retained",
])
if document.get("cleanup_command") != expected_command:
    raise SystemExit("retention authorization cleanup command is invalid")
if document.get("migration_projects") != [gate2_project, gate4_project]:
    raise SystemExit("retention authorization migration projects are invalid")
PY
}

cleanup_private_material() {
    local path
    local failed=0
    for path in "$EVIDENCE_DIR/rehearsal.env" "$EVIDENCE_DIR/private/runtime.env" \
        "$EVIDENCE_DIR/private/fixture-manifest.json" "$EVIDENCE_DIR/private/postgres-password" \
        "$EVIDENCE_DIR/private/database-password" "$EVIDENCE_DIR/private/database.pgpass"; do
        if [[ -e "$path" ]] && ! rm -f -- "$path"; then failed=1; fi
    done
    if [[ -d "$FRONTEND_RESULTS_DIR/.auth" ]] && ! rm -rf -- "$FRONTEND_RESULTS_DIR/.auth"; then failed=1; fi
    if [[ -d "$FRONTEND_RESULTS_DIR/artifacts" ]] && ! rm -rf -- "$FRONTEND_RESULTS_DIR/artifacts"; then failed=1; fi
    if [[ -f "$FRONTEND_RESULTS_DIR/playwright.json" ]] && ! rm -f -- "$FRONTEND_RESULTS_DIR/playwright.json"; then failed=1; fi
    if [[ -f "$FRONTEND_RESULTS_DIR/playwright.xml" ]] && ! rm -f -- "$FRONTEND_RESULTS_DIR/playwright.xml"; then failed=1; fi
    return "$failed"
}

write_cleanup_failure() {
    local path="$EVIDENCE_DIR/private/cleanup-failure.json"
    python3 - "$path" "$@" <<'PY'
import json
import os
import pathlib
import sys
import tempfile
from datetime import datetime, timezone

target = pathlib.Path(sys.argv[1])
document = {
    "schema_version": "radar-local-cleanup-failure-v1",
    "recorded_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
    "failed_categories": list(dict.fromkeys(sys.argv[2:])),
}
descriptor, temporary_name = tempfile.mkstemp(prefix=f".{target.name}.", dir=target.parent)
temporary = pathlib.Path(temporary_name)
try:
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        json.dump(document, stream, sort_keys=True, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(temporary, 0o600)
    os.replace(temporary, target)
    os.chmod(target, 0o600)
finally:
    temporary.unlink(missing_ok=True)
PY
}

cleanup_resources() {
    local ids id project
    local category_failed=0
    local retain_at_cleanup=0
    local -a failed_categories=()
    if ! stop_loopback_forwarder; then failed_categories+=("forwarder"); fi
    validate_run_id || failed_categories+=("validation")
    if [[ -e "$EVIDENCE_DIR/private/cleanup-failure.json" ]] && \
        ! rm -f -- "$EVIDENCE_DIR/private/cleanup-failure.json"; then
        failed_categories+=("private")
    fi
    if [[ "$DOCKER_CLEANUP_ENABLED" == 1 ]]; then
        if ! command -v "$DOCKER_BIN" >/dev/null 2>&1; then
            failed_categories+=("docker")
        else
            category_failed=0
            if ! ids=$(verified_compose_resource_ids container); then category_failed=1; fi
            while IFS= read -r id; do
                [[ -n "$id" ]] || continue
                if ! "$DOCKER_BIN" rm -f "$id" >/dev/null 2>&1; then category_failed=1; fi
            done <<<"$ids"
            for project in "$GATE2_PROJECT" "$GATE4_PROJECT"; do
                [[ -n "$project" ]] || continue
                if ! ids=$(verified_migration_resource_ids container "$project"); then category_failed=1; fi
                while IFS= read -r id; do
                    [[ -n "$id" ]] || continue
                    if ! "$DOCKER_BIN" rm -f "$id" >/dev/null 2>&1; then category_failed=1; fi
                done <<<"$ids"
            done
            [[ $category_failed -eq 0 ]] || failed_categories+=("container")

            if [[ "$RETAIN_DEBUG" == 1 && "$RETENTION_AUTHORIZED" == 1 ]] && \
                retention_record_valid >/dev/null 2>&1; then
                retain_at_cleanup=1
            fi
            if [[ "$retain_at_cleanup" != 1 ]]; then
                category_failed=0
                if ! ids=$(verified_compose_resource_ids volume); then category_failed=1; fi
                while IFS= read -r id; do
                    [[ -n "$id" ]] || continue
                    if ! "$DOCKER_BIN" volume rm "$id" >/dev/null 2>&1; then category_failed=1; fi
                done <<<"$ids"
                for project in "$GATE2_PROJECT" "$GATE4_PROJECT"; do
                    [[ -n "$project" ]] || continue
                    if ! ids=$(verified_migration_resource_ids volume "$project"); then category_failed=1; fi
                    while IFS= read -r id; do
                        [[ -n "$id" ]] || continue
                        if ! "$DOCKER_BIN" volume rm "$id" >/dev/null 2>&1; then category_failed=1; fi
                    done <<<"$ids"
                done
                [[ $category_failed -eq 0 ]] || failed_categories+=("volume")
            fi

            category_failed=0
            if ! ids=$(verified_compose_resource_ids network); then category_failed=1; fi
            while IFS= read -r id; do
                [[ -n "$id" ]] || continue
                if ! "$DOCKER_BIN" network rm "$id" >/dev/null 2>&1; then category_failed=1; fi
            done <<<"$ids"
            for project in "$GATE2_PROJECT" "$GATE4_PROJECT"; do
                [[ -n "$project" ]] || continue
                if ! ids=$(verified_migration_resource_ids network "$project"); then category_failed=1; fi
                while IFS= read -r id; do
                    [[ -n "$id" ]] || continue
                    if ! "$DOCKER_BIN" network rm "$id" >/dev/null 2>&1; then category_failed=1; fi
                done <<<"$ids"
            done
            [[ $category_failed -eq 0 ]] || failed_categories+=("network")
        fi
    fi
    if ! cleanup_private_material; then failed_categories+=("private"); fi
    if [[ ${#failed_categories[@]} -ne 0 ]]; then
        write_cleanup_failure "${failed_categories[@]}" || true
        return 1
    fi
    return 0
}

on_exit() {
    local main_code=$?
    local cleanup_code=0
    local final_code=$main_code
    trap - EXIT INT TERM
    cleanup_resources || cleanup_code=$?
    if [[ $cleanup_code -ne 0 && $main_code -eq 0 ]]; then
        final_code=$cleanup_code
        write_failed_closure cleanup "$cleanup_code"
    fi
    exit "$final_code"
}

on_interrupt() {
    trap - INT TERM
    exit 130
}

on_terminate() {
    trap - INT TERM
    exit 143
}

if [[ "$DRY_RUN" == 1 ]]; then
    [[ "$CLEANUP_ONLY" == 0 ]] || { printf 'FAIL: cleanup mode cannot be a dry-run\n' >&2; exit 2; }
    render_dry_run
    exit $?
fi

[[ "$DRY_RUN" == 0 ]] || { printf 'FAIL: RADAR_LOCAL_PRERELEASE_DRY_RUN must be 0 or 1\n' >&2; exit 2; }
[[ "$RETAIN_DEBUG" == 0 || "$RETAIN_DEBUG" == 1 ]] || { printf 'FAIL: RADAR_LOCAL_RETAIN_DEBUG must be 0 or 1\n' >&2; exit 2; }
if [[ ! "$RETAIN_SECONDS" =~ ^[1-9][0-9]{0,4}$ ]] || (( 10#$RETAIN_SECONDS > 86400 )); then
    printf 'FAIL: debug retention must be bounded\n' >&2
    exit 2
fi
if [[ ! "$HEALTH_TIMEOUT_SECONDS" =~ ^[1-9][0-9]{0,3}$ ]] || (( 10#$HEALTH_TIMEOUT_SECONDS > 3600 )); then
    printf 'FAIL: RADAR_LOCAL_HEALTH_TIMEOUT_SECONDS must be bounded\n' >&2
    exit 2
fi
validate_run_id || exit 2
validate_evidence_path || exit 2
mkdir -p "$EVIDENCE_DIR/private" "$EVIDENCE_DIR/public"
chmod 700 "$EVIDENCE_DIR/private"
trap on_exit EXIT
trap on_interrupt INT
trap on_terminate TERM
validate_migration_projects || exit 2

if [[ "$CLEANUP_ONLY" == 1 ]]; then
    DOCKER_CLEANUP_ENABLED=1
    exit 0
fi

validate_loopback_origin || exit 2
validate_static_interfaces || exit 2
DOCKER_CLEANUP_ENABLED=1
if [[ "$RETAIN_DEBUG" == 1 ]]; then
    if ! write_retention_record || ! retention_record_valid; then
        write_failed_closure retention-authorization 1
        exit 1
    fi
    RETENTION_AUTHORIZED=1
fi

run_gate immutable-inputs-and-code phase_1 || exit $?
run_gate migration-225 phase_2 || exit $?
run_gate radar-browser-workflows phase_3 || exit $?
run_gate restart-and-rollback phase_4 || exit $?
run_gate evidence-closure-input phase_5 || exit $?

python3 "$CLOSURE_TOOL" audit --evidence-dir "$EVIDENCE_DIR" --bindings "$EVIDENCE_DIR/bindings.json" 2>&1 | \
    python3 "$CLOSURE_TOOL" redact-stream >>"$EVIDENCE_DIR/private/evidence-closure-input.log"
closure_pipeline_status=("${PIPESTATUS[@]}")
closure_code=${closure_pipeline_status[0]:-1}
closure_redactor_code=${closure_pipeline_status[1]:-1}
if [[ $closure_code -eq 0 && $closure_redactor_code -ne 0 ]]; then closure_code=$closure_redactor_code; fi
if [[ $closure_code -ne 0 ]]; then
    write_failed_closure evidence-closure-input "$closure_code"
    exit "$closure_code"
fi
printf 'local-isolated-prerelease-passed\n'
