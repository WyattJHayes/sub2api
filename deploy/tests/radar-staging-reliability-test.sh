#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.radar-staging.yml"
RELIABILITY_COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.radar-reliability.yml"
WORKER_COMPOSE_FILE="$ROOT_DIR/radar-worker/deploy/docker-compose.staging.yml"
WORKER_RELIABILITY_COMPOSE_FILE="$ROOT_DIR/radar-worker/deploy/docker-compose.reliability.yml"
ACCEPTANCE_SCRIPT="$ROOT_DIR/deploy/radar/reliability-acceptance.py"
LIVE_E2E_SCRIPT="$ROOT_DIR/deploy/tests/radar-live-e2e.sh"
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/radar-reliability-test.XXXXXX")
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

require_command docker
require_command python3

mkdir -p "$TEST_ROOT/evidence" "$TEST_ROOT/reports"

run_live_preflight_contract() {
    [[ -x "$LIVE_E2E_SCRIPT" ]] || fail "live E2E script is missing or not executable"

    local contract_root="$TEST_ROOT/live-preflight"
    local fake_bin="$contract_root/bin"
    local env_file="$contract_root/radar.env"
    local docker_log="$contract_root/docker.log"
    mkdir -p "$fake_bin"
    : >"$docker_log"
    : >"$env_file"
    chmod 600 "$env_file"

    cat >"$fake_bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
log=${RADAR_LIVE_DOCKER_LOG:?RADAR_LIVE_DOCKER_LOG is required}
printf '%s\n' "$*" >>"$log"
[[ ${1:-} == compose ]] || exit 1
case " $* " in
    *\ config\ --quiet\ *) exit 0 ;;
    *) exit 1 ;;
esac
SH
    chmod 700 "$fake_bin/docker"

    local base_env=(
        "RADAR_LIVE_E2E=1"
        "RADAR_LIVE_DRY_RUN=1"
        "RADAR_LIVE_ENV_FILE=$env_file"
        "RADAR_LIVE_ADMIN_API_KEY=live-admin-key-4f93e0a5b7c1d8e2f6a9c4b0d7e1f5a3"
        "RADAR_LIVE_EVALUATION_API_KEY=live-evaluation-key-6d2f9a4c8b1e7f3a5d0c6b2e9f4a8d1"
        "RADAR_RUNNER_WORKER_TOKEN=live-runner-token-9c4e1a7b2d8f5a0c6e3b1d9f7a4c8e2"
        "RADAR_GRADER_WORKER_TOKEN=live-grader-token-1a7e4c9d2b6f8e0c5d3b9a4f7e2c6d1"
        "RADAR_STATISTICS_WORKER_TOKEN=live-statistics-token-8b2d6f0a4c9e1d7f3a5b6c8e2d4f9a1"
        "RADAR_LOADGEN_WORKER_TOKEN=live-loadgen-token-3d8f1a6c9e2b4f7d0a5c8e1b6d9f2a4"
        "RADAR_CHAOS_CONTROLLER_TOKEN=live-chaos-token-5f0c9e2a7d1b4f8c6e3a0d9b2f7c1e4"
        "RADAR_RECOVERY_VERIFIER_TOKEN=live-recovery-token-7a1d4f9c2e6b0d8f3c5a9e1b4d7f2c6"
        "RADAR_CONTROL_PLANE_IMAGE=registry.example.invalid/sub2api/radar-control-plane@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        "RADAR_WORKER_IMAGE=registry.example.invalid/sub2api/radar-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
        "RADAR_LIVE_CONTROL_PLANE_IMAGE_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        "RADAR_LIVE_WORKER_IMAGE_DIGEST=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
        "RADAR_LIVE_DOCKER_LOG=$docker_log"
        "PATH=$fake_bin:$PATH"
        "TMPDIR=${TMPDIR:-/tmp}"
    )

    local output="$contract_root/unset.out"
    if env -i "PATH=$fake_bin:$PATH" "RADAR_LIVE_DOCKER_LOG=$docker_log" \
        "$LIVE_E2E_SCRIPT" >"$output" 2>&1; then
        fail "live E2E accepted an invocation without RADAR_LIVE_E2E=1"
    fi
    grep -Fq 'set RADAR_LIVE_E2E=1' "$output" || \
        fail "live E2E opt-in failure did not identify RADAR_LIVE_E2E"
    [[ ! -s "$docker_log" ]] || fail "live E2E invoked Docker before opt-in"

    local marker_name marker_value
    for marker_name in \
        RADAR_LIVE_EVALUATION_API_KEY RADAR_LIVE_ADMIN_API_KEY \
        RADAR_RUNNER_WORKER_TOKEN RADAR_GRADER_WORKER_TOKEN RADAR_STATISTICS_WORKER_TOKEN \
        RADAR_LOADGEN_WORKER_TOKEN RADAR_CHAOS_CONTROLLER_TOKEN RADAR_RECOVERY_VERIFIER_TOKEN; do
        case "$marker_name" in
            RADAR_LIVE_EVALUATION_API_KEY) marker_value='synthetic-live-evaluation-key-12345678901234567890' ;;
            RADAR_LIVE_ADMIN_API_KEY) marker_value='placeholder-live-admin-key-12345678901234567890' ;;
            *) marker_value='synthetic-live-worker-token-12345678901234567890' ;;
        esac
        : >"$docker_log"
        output="$contract_root/${marker_name}.out"
        if env -i "${base_env[@]}" "$marker_name=$marker_value" \
            "$LIVE_E2E_SCRIPT" >"$output" 2>&1; then
            fail "live E2E accepted synthetic or placeholder credential: $marker_name"
        fi
        grep -Fq 'synthetic or placeholder credential' "$output" || \
            fail "live E2E did not identify the rejected credential: $marker_name"
        [[ ! -s "$docker_log" ]] || fail "live E2E invoked Docker after rejecting $marker_name"
    done

    : >"$docker_log"
    output="$contract_root/repeated.out"
    if env -i "${base_env[@]}" \
        RADAR_LIVE_EVALUATION_API_KEY=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
        "$LIVE_E2E_SCRIPT" >"$output" 2>&1; then
        fail "live E2E accepted a repeated-character credential"
    fi
    grep -Fq 'repeated-character credential' "$output" || \
        fail "live E2E did not reject a low-entropy credential"
    [[ ! -s "$docker_log" ]] || fail "live E2E invoked Docker after rejecting a low-entropy credential"

    : >"$docker_log"
    output="$contract_root/missing-control-digest.out"
    if env -i "${base_env[@]}" RADAR_LIVE_CONTROL_PLANE_IMAGE_DIGEST= \
        "$LIVE_E2E_SCRIPT" >"$output" 2>&1; then
        fail "live E2E accepted a missing control-plane digest"
    fi
    grep -Fq 'RADAR_LIVE_CONTROL_PLANE_IMAGE_DIGEST must be a lowercase sha256 image digest' "$output" || \
        fail "live E2E did not identify a missing control-plane digest"
    [[ ! -s "$docker_log" ]] || fail "live E2E invoked Docker after rejecting a missing control-plane digest"

    : >"$docker_log"
    output="$contract_root/mismatched-worker-digest.out"
    if env -i "${base_env[@]}" \
        RADAR_LIVE_WORKER_IMAGE_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
        "$LIVE_E2E_SCRIPT" >"$output" 2>&1; then
        fail "live E2E accepted a mismatched Worker image digest"
    fi
    grep -Fq 'RADAR_WORKER_IMAGE must end with RADAR_LIVE_WORKER_IMAGE_DIGEST' "$output" || \
        fail "live E2E did not identify a mismatched Worker digest"
    [[ ! -s "$docker_log" ]] || fail "live E2E invoked Docker after rejecting a mismatched Worker digest"

    : >"$docker_log"
    output="$contract_root/valid.out"
    env -i "${base_env[@]}" "$LIVE_E2E_SCRIPT" >"$output" 2>&1 || {
        sed -n '1,80p' "$output" >&2
        fail "live E2E dry-run rejected dedicated contract credentials"
    }
    grep -Fq 'Radar live E2E dry-run passed.' "$output" || \
        fail "live E2E dry-run did not emit its success marker"
    [[ "$(wc -l <"$docker_log" | tr -d ' ')" == "1" ]] || \
        fail "live E2E dry-run performed more than one Docker operation"
    grep -Fq 'compose' "$docker_log" || fail "live E2E dry-run did not render Compose"
    grep -Fq 'config --quiet' "$docker_log" || \
        fail "live E2E dry-run attempted an operation beyond Compose config"

    cat >"$fake_bin/stat" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
    -f)
        # GNU stat accepts -f and exits successfully, but reports filesystem
        # data instead of file permissions. The live check must fall back to
        # the GNU permission form when this happens.
        printf '4096\n'
        ;;
    -c)
        [[ ${2:-} == '%a' ]] || exit 1
        printf '600\n'
        ;;
    *)
        exec /usr/bin/stat "$@"
        ;;
esac
SH
    chmod 700 "$fake_bin/stat"
    : >"$docker_log"
    output="$contract_root/gnu-stat.out"
    env -i "${base_env[@]}" "$LIVE_E2E_SCRIPT" >"$output" 2>&1 || {
        sed -n '1,80p' "$output" >&2
        fail "live E2E rejected a valid mode-600 file when stat -f returned filesystem data"
    }
    grep -Fq 'Radar live E2E dry-run passed.' "$output" || \
        fail "live E2E GNU stat compatibility check did not emit its success marker"
}

run_live_preflight_contract

compose_env=(
    "RADAR_RELEASE_VERSION=staging-test"
    "RADAR_RELEASE_COMMIT=0123456789abcdef0123456789abcdef01234567"
    "RADAR_RELEASE_DATE=2026-07-30T00:00:00Z"
    "RADAR_POSTGRES_PASSWORD=staging-postgres-password"
    "RADAR_JWT_SECRET=staging-jwt-secret-with-more-than-32-bytes"
    "RADAR_ADMIN_PASSWORD=staging-admin-password"
    "RADAR_CONTEXT_SIGNING_KEY=staging-context-signing-key-with-32-bytes"
    "RADAR_EVIDENCE_HASH_KEY=staging-evidence-hash-key-with-32-bytes"
    "RADAR_API_WRITER_INSTANCE_ID=00000000-0000-4000-8000-000000000001"
    "RADAR_SYNTHETIC_UPSTREAM_API_KEY=staging-synthetic-upstream-key"
    "RADAR_RUNNER_WORKER_TOKEN=staging-runner-worker-token-more-than-32-bytes"
    "RADAR_GRADER_WORKER_TOKEN=staging-grader-worker-token-more-than-32-bytes"
    "RADAR_STATISTICS_WORKER_TOKEN=staging-statistics-worker-token-more-than-32-bytes"
    "RADAR_MINIO_ROOT_USER=radar-artifact-access"
    "RADAR_MINIO_ROOT_PASSWORD=staging-minio-secret-more-than-32-bytes"
    "RADAR_LOADGEN_WORKER_TOKEN=staging-loadgen-worker-token-more-than-32-bytes"
    "RADAR_LOADGEN_EVALUATION_API_KEY=sk-staging-loadgen-evaluation"
    "RADAR_LOAD_PLAN_ID=00000000-0000-4000-8000-000000000010"
    "RADAR_LOAD_RUN_ID=00000000-0000-4000-8000-000000000011"
    "RADAR_LOADGEN_IMAGE_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    "RADAR_CHAOS_CONTROLLER_TOKEN=staging-chaos-controller-token-more-than-32-bytes"
    "RADAR_CHAOS_AUTO_ROLLBACK_SECONDS=15"
    "RADAR_FAULT_EXPERIMENT_ID=00000000-0000-4000-8000-000000000012"
    "RADAR_CHAOS_TARGET_WORKER_ID=00000000-0000-4000-8000-000000000013"
    "RADAR_RECOVERY_VERIFIER_TOKEN=staging-recovery-verifier-token-more-than-32-bytes"
    "RADAR_RECOVERY_EVIDENCE_ID=00000000-0000-4000-8000-000000000014"
    "RADAR_RELIABILITY_EVIDENCE_DIR=$TEST_ROOT/evidence"
    "RADAR_RELIABILITY_REPORT_DIR=$TEST_ROOT/reports"
    "RADAR_RELIABILITY_UID=10001"
    "RADAR_RELIABILITY_GID=10001"
)

artifact_variable_names=(
    RADAR_MINIO_ROOT_USER
    RADAR_MINIO_ROOT_PASSWORD
)

profile_variable_names=(
    RADAR_LOADGEN_WORKER_TOKEN
    RADAR_LOADGEN_EVALUATION_API_KEY
    RADAR_LOAD_PLAN_ID
    RADAR_LOAD_RUN_ID
    RADAR_LOADGEN_IMAGE_DIGEST
    RADAR_CHAOS_CONTROLLER_TOKEN
    RADAR_CHAOS_AUTO_ROLLBACK_SECONDS
    RADAR_FAULT_EXPERIMENT_ID
    RADAR_CHAOS_TARGET_WORKER_ID
    RADAR_RECOVERY_VERIFIER_TOKEN
    RADAR_RECOVERY_EVIDENCE_ID
    RADAR_RELIABILITY_EVIDENCE_DIR
    RADAR_RELIABILITY_REPORT_DIR
    RADAR_RELIABILITY_UID
    RADAR_RELIABILITY_GID
)

base_compose_env=()
for assignment in "${compose_env[@]}"; do
    include=true
    for variable in "${profile_variable_names[@]}"; do
        if [[ "$assignment" == "$variable="* ]]; then
            include=false
            break
        fi
    done
    $include && base_compose_env+=("$assignment")
done

# The default stack must remain usable without any reliability or chaos secret.
env "${base_compose_env[@]}" docker compose -f "$COMPOSE_FILE" config --format json \
    >"$TEST_ROOT/base-compose.json"
env "${base_compose_env[@]}" docker compose -f "$WORKER_COMPOSE_FILE" config --quiet

python3 - "$TEST_ROOT/base-compose.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
document = json.loads(path.read_text(encoding="utf-8"))
services = document["services"]
expected = {"minio-staging", "minio-init", "clamav-staging"}
missing = expected.difference(services)
if missing:
    raise SystemExit(f"{path}: missing artifact services: {sorted(missing)}")

control = services["sub2api-staging"]
environment = control.get("environment", {})
required_environment = {
    "RADAR_ARTIFACT_STORAGE_ENABLED": "true",
    "RADAR_ARTIFACT_STORAGE_ENDPOINT": "http://minio-staging:9000",
    "RADAR_ARTIFACT_STORAGE_BUCKET": "radar-artifacts",
    "RADAR_ARTIFACT_STORAGE_ACCESS_KEY_ID": "radar-artifact-access",
    "RADAR_ARTIFACT_STORAGE_SECRET_ACCESS_KEY": "staging-minio-secret-more-than-32-bytes",
    "RADAR_ARTIFACT_STORAGE_FORCE_PATH_STYLE": "true",
    "RADAR_ARTIFACT_STORAGE_SCAN_MODE": "clamav",
    "RADAR_ARTIFACT_STORAGE_CLAMAV_ADDRESS": "clamav-staging:3310",
}
for key, expected_value in required_environment.items():
    if str(environment.get(key, "")).lower() != expected_value.lower():
        raise SystemExit(f"{path}: control plane {key} is not bound to the artifact stack")

depends_on = control.get("depends_on", {})
expected_dependencies = {
    "minio-init": "service_completed_successfully",
    "clamav-staging": "service_healthy",
}
for service_name, condition in expected_dependencies.items():
    actual = depends_on.get(service_name, {}).get("condition")
    if actual != condition:
        raise SystemExit(
            f"{path}: control plane dependency {service_name} uses {actual!r}, expected {condition!r}"
        )

for service_name in expected:
    service = services[service_name]
    if service.get("ports"):
        raise SystemExit(f"{path}: {service_name} publishes a host port")
    if "control_plane" not in service.get("networks", {}):
        raise SystemExit(f"{path}: {service_name} is outside the control-plane network")
    if service_name != "minio-init" and not service.get("healthcheck"):
        raise SystemExit(f"{path}: {service_name} has no health check")

clamav = services["clamav-staging"]
required_clamav_capabilities = {"CHOWN", "FOWNER", "DAC_OVERRIDE", "SETGID", "SETUID"}
actual_clamav_capabilities = set(clamav.get("cap_add", []))
if "ALL" not in clamav.get("cap_drop", []):
    raise SystemExit(f"{path}: ClamAV must drop the default capability set")
if actual_clamav_capabilities != required_clamav_capabilities:
    raise SystemExit(
        f"{path}: ClamAV capabilities are {sorted(actual_clamav_capabilities)}, "
        f"expected {sorted(required_clamav_capabilities)}"
    )

if services["minio-init"].get("depends_on", {}).get("minio-staging", {}).get("condition") != "service_healthy":
    raise SystemExit(f"{path}: bucket initialization does not wait for healthy MinIO")
minio_init_environment = services["minio-init"].get("environment", {})
if minio_init_environment.get("MC_CONFIG_DIR") != "/tmp/.mc":
    raise SystemExit(f"{path}: bucket initialization does not use its writable tmpfs for mc config")

volumes = document.get("volumes", {})
if "radar_staging_artifacts" not in volumes:
    raise SystemExit(f"{path}: artifact object storage has no durable volume")
PY

for variable in "${artifact_variable_names[@]}"; do
    filtered_env=()
    for assignment in "${base_compose_env[@]}"; do
        [[ "$assignment" == "$variable="* ]] || filtered_env+=("$assignment")
    done
    if env -u "$variable" "${filtered_env[@]}" docker compose -f "$COMPOSE_FILE" \
        config --quiet >/dev/null 2>&1; then
        fail "artifact stack accepted missing required variable: $variable"
    fi
done

env "${compose_env[@]}" docker compose -f "$COMPOSE_FILE" -f "$RELIABILITY_COMPOSE_FILE" \
    --profile reliability --profile chaos config --format json \
    >"$TEST_ROOT/compose.json"
env "${compose_env[@]}" docker compose -f "$WORKER_COMPOSE_FILE" \
    -f "$WORKER_RELIABILITY_COMPOSE_FILE" --profile reliability --profile chaos \
    config --format json \
    >"$TEST_ROOT/worker-compose.json"

python3 - "$TEST_ROOT/compose.json" "$TEST_ROOT/worker-compose.json" <<'PY'
import json
import pathlib
import sys

expected = {"radar-loadgen", "radar-chaos-controller", "radar-recovery-verifier"}

for path_arg in sys.argv[1:]:
    path = pathlib.Path(path_arg)
    document = json.loads(path.read_text(encoding="utf-8"))
    services = document["services"]
    missing = expected.difference(services)
    if missing:
        raise SystemExit(f"{path}: missing reliability services: {sorted(missing)}")

    networks = document["networks"]
    reliability_network = networks.get("radar_reliability_internal", {})
    if reliability_network.get("internal") is not True:
        raise SystemExit(f"{path}: reliability network must be internal")

    for name in expected:
        service = services[name]
        profiles = set(service.get("profiles", []))
        required_profile = "chaos" if name == "radar-chaos-controller" else "reliability"
        if required_profile not in profiles:
            raise SystemExit(f"{path}: {name} is missing profile {required_profile}")
        if service.get("read_only") is not True:
            raise SystemExit(f"{path}: {name} root filesystem is writable")
        if service.get("user") != "10001:10001":
            raise SystemExit(f"{path}: {name} must run as the configured non-root user")
        if "ALL" not in service.get("cap_drop", []):
            raise SystemExit(f"{path}: {name} does not drop all capabilities")
        if "no-new-privileges:true" not in service.get("security_opt", []):
            raise SystemExit(f"{path}: {name} allows privilege escalation")
        if "radar_reliability_internal" not in service.get("networks", {}):
            raise SystemExit(f"{path}: {name} is outside the reliability network")
        mounts = service.get("volumes", [])
        for mount in mounts:
            source = mount.get("source", "") if isinstance(mount, dict) else str(mount)
            if "docker.sock" in source:
                raise SystemExit(f"{path}: {name} mounts the Docker socket")

    chaos = services["radar-chaos-controller"]
    if chaos.get("volumes"):
        raise SystemExit(f"{path}: chaos controller must not receive host mounts")
PY

for variable in "${profile_variable_names[@]}"; do
    filtered_env=()
    for assignment in "${compose_env[@]}"; do
        [[ "$assignment" == "$variable="* ]] || filtered_env+=("$assignment")
    done
    if env -u "$variable" "${filtered_env[@]}" docker compose -f "$COMPOSE_FILE" \
        -f "$RELIABILITY_COMPOSE_FILE" --profile reliability --profile chaos \
        config --quiet >/dev/null 2>&1; then
        fail "profile accepted missing required variable: $variable"
    fi
done

[[ -x "$ACCEPTANCE_SCRIPT" ]] || fail "acceptance script is missing or not executable"

cat >"$TEST_ROOT/pass.json" <<'JSON'
{
  "schema_version": "radar-staging-reliability-acceptance-v1",
  "release": {
    "run_id": "00000000-0000-4000-8000-000000000011",
    "load_plan_id": "00000000-0000-4000-8000-000000000010",
    "control_plane_image_digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "worker_image_digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  },
  "fact_manifest": {
    "schema_version": "radar-fact-manifest-v1",
    "run_id": "00000000-0000-4000-8000-000000000011",
    "load_plan_id": "00000000-0000-4000-8000-000000000010",
    "profile_id": "staging-v1",
    "policy_id": "00000000-0000-4000-8000-000000000015",
    "policy_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "release_subject_id": "00000000-0000-4000-8000-000000000016",
    "release_subject_hash": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "snapshot_refs": [
      {
        "snapshot_id": "00000000-0000-4000-8000-000000000022",
        "snapshot_hash": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
        "run_id": "00000000-0000-4000-8000-000000000011",
        "load_plan_id": "00000000-0000-4000-8000-000000000010",
        "profile_id": "staging-v1",
        "slice_key": "model=deepseek-chat|region=staging|concurrency=100",
        "source_watermark": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
      }
    ],
    "recovery_ref": {
      "evidence_id": "00000000-0000-4000-8000-000000000014",
      "evidence_hash": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "run_id": "00000000-0000-4000-8000-000000000011",
      "experiment_id": "00000000-0000-4000-8000-000000000012",
      "source_watermark": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
      "recovery_generation": 1
    },
    "artifact_manifest_hashes": ["6666666666666666666666666666666666666666666666666666666666666666"],
    "manifest_sha256": "7a2c76bf606659e7e955edab7e640f924e90f572fc3b45f057bbebd7bf3066d6",
    "signature": "63cbd37f9d4bc330feec99623a7a54eac166ba5e8698c9668c3df4b09690c756"
  },
  "reliability_snapshots": [
    {
      "snapshot_id": "00000000-0000-4000-8000-000000000022",
      "snapshot_hash": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "run_id": "00000000-0000-4000-8000-000000000011",
      "load_plan_id": "00000000-0000-4000-8000-000000000010",
      "profile_id": "staging-v1",
      "slice_key": "model=deepseek-chat|region=staging|concurrency=100",
      "window_start": "2026-07-30T00:00:00.123456789Z",
      "window_end": "2026-07-30T00:10:00.987654321Z",
      "query_version": "reliability-query-v1",
      "source_hash": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "source_watermark": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "fresh_until": "2026-07-30T00:20:00.000000001Z",
      "metrics": {
        "request_count": 600,
        "success_count": 594,
        "error_count": 4,
        "timeout_count": 2,
        "successful_latency_count": 594,
        "valid_pair_count": 30,
        "upstream_failure_count": 4,
        "gateway_failure_count": 0,
        "client_cancellation_count": 0,
        "error_numerator": 6,
        "error_denominator": 600,
        "p99_latency_ms": 480,
        "histogram_or_sketch_hash": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
        "error_rate": "0.01",
        "cost_amount": "0",
        "ongoing_confirmed_p0_incident": false
      },
      "request_count": 600,
      "success_count": 594,
      "error_count": 4,
      "timeout_count": 2,
      "billing_idempotency_failures": 0,
      "p99_latency_ms": 480,
      "p99_slo_ms": 500,
      "error_rate": "0.010000"
    }
  ],
  "recovery": {
    "evidence_id": "00000000-0000-4000-8000-000000000014",
    "evidence_hash": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
    "recovery_generation": 1,
    "source_watermark": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
    "rpo_seconds": 240,
    "rpo_limit_seconds": 300,
    "rto_seconds": 1500,
    "rto_limit_seconds": 1800,
    "worker_reregistered": true,
    "leases_recovered": true,
    "duplicate_score_count": 0,
    "evidence_hash_consistent": true,
    "ledger_consistent": true,
    "deterministic_acceptance": {
      "valid_pairs": 30,
      "pre_recovery_hash": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "post_recovery_hash": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    }
  },
  "rollback": {
    "recorded_at": "2026-07-30T00:00:00Z",
    "failed_run_ids": ["00000000-0000-4000-8000-000000000011"],
    "active_lease_ids": ["00000000-0000-4000-8000-000000000020"],
    "previous_control_plane_image_digest": "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
    "previous_worker_image_digest": "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
    "control_plane_binary_sha256": "1111111111111111111111111111111111111111111111111111111111111111",
    "worker_binary_sha256": "2222222222222222222222222222222222222222222222222222222222222222",
    "migration_checksums": ["3333333333333333333333333333333333333333333333333333333333333333"],
    "score_hashes": ["4444444444444444444444444444444444444444444444444444444444444444"],
    "aggregate_hashes": ["5555555555555555555555555555555555555555555555555555555555555555"],
    "artifact_manifest_hashes": ["6666666666666666666666666666666666666666666666666666666666666666"],
    "budget_ledger_total_before": "10.00000000",
    "budget_ledger_total_after": "10.00000000",
    "smoke_run_id": "00000000-0000-4000-8000-000000000021"
  }
}
JSON

python3 - "$TEST_ROOT/pass.json" "$ROOT_DIR/deploy/radar" <<'PY'
import hashlib
import hmac
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
sys.path.insert(0, sys.argv[2])
from reliability_hash import snapshot_hash

document = json.loads(path.read_text(encoding="utf-8"))
snapshot = document["reliability_snapshots"][0]
snapshot["snapshot_hash"] = snapshot_hash(snapshot)
manifest = document["fact_manifest"]
manifest["snapshot_refs"][0]["snapshot_hash"] = snapshot["snapshot_hash"]
unsigned = dict(manifest)
unsigned.pop("manifest_sha256", None)
unsigned.pop("signature", None)
manifest["manifest_sha256"] = hashlib.sha256(
    json.dumps(unsigned, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
).hexdigest()
manifest["signature"] = hmac.new(
    b"k" * 32,
    json.dumps(
        {**unsigned, "manifest_sha256": manifest["manifest_sha256"]},
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8"),
    hashlib.sha256,
).hexdigest()
path.write_text(json.dumps(document), encoding="utf-8")
PY

RADAR_EVIDENCE_MANIFEST_KEY=$(printf 'k%.0s' {1..32}) "$ACCEPTANCE_SCRIPT" "$TEST_ROOT/pass.json" >"$TEST_ROOT/pass.out"
grep -Fq 'PASS radar staging reliability acceptance' "$TEST_ROOT/pass.out" || \
    fail "passing evidence did not emit the acceptance marker"

python3 - "$TEST_ROOT/pass.json" "$ROOT_DIR/deploy/radar" <<'PY'
import copy
import importlib.util
import json
import pathlib
import sys
from uuid import UUID

evidence_path = pathlib.Path(sys.argv[1])
radar_dir = pathlib.Path(sys.argv[2])
sys.path.insert(0, str(radar_dir))
spec = importlib.util.spec_from_file_location(
    "radar_facts_verifier", radar_dir / "verify-reliability-facts.py"
)
if spec is None or spec.loader is None:
    raise SystemExit("cannot load facts verifier")
verifier = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verifier)
document = json.loads(evidence_path.read_text(encoding="utf-8"))
release = document["release"]
manifest = document["fact_manifest"]
facts = {
    "schema_version": "radar-reliability-facts-v1",
    "run_id": release["run_id"],
    "policy_id": manifest["policy_id"],
    "profile_id": manifest["profile_id"],
    "load_plan_id": release["load_plan_id"],
    "load_plan_sha256": "a" * 64,
    "policy_hash": manifest["policy_hash"],
    "release_subject_id": manifest["release_subject_id"],
    "release_subject_hash": manifest["release_subject_hash"],
    "snapshots": copy.deepcopy(document["reliability_snapshots"]),
    # The API facts endpoint exposes the full recovery row. The acceptance
    # manifest carries a reference subset, so bind that reference to the
    # complete recovery evidence before exercising the facts verifier.
    "recovery": copy.deepcopy(document["recovery"]),
    "artifact_manifest_hashes": list(manifest["artifact_manifest_hashes"]),
}
facts["recovery"].update({
    key: manifest["recovery_ref"][key]
    for key in ("run_id", "experiment_id")
})
verifier.verify_facts(
    facts,
    UUID(release["run_id"]),
    UUID(manifest["policy_id"]),
    manifest["profile_id"],
)
verifier.compare_acceptance_manifest(evidence_path, facts)

tampered = copy.deepcopy(facts)
tampered["snapshots"][0]["snapshot_hash"] = "d" * 64
try:
    verifier.verify_facts(
        tampered,
        UUID(release["run_id"]),
        UUID(manifest["policy_id"]),
        manifest["profile_id"],
    )
except verifier.FactVerificationError:
    pass
else:
    raise SystemExit("facts verifier accepted a tampered snapshot hash")

tampered = copy.deepcopy(facts)
tampered["snapshots"][0]["slice_key"] = "tampered-slice"
try:
    verifier.compare_acceptance_manifest(evidence_path, tampered)
except verifier.FactVerificationError:
    pass
else:
    raise SystemExit("facts verifier accepted a mismatched immutable snapshot field")
PY

assert_rejected() {
    local name=$1
    local expression=$2
    local expected=$3
    python3 - "$TEST_ROOT/pass.json" "$TEST_ROOT/$name.json" "$expression" <<'PY'
import json
import pathlib
import sys

source, target, expression = sys.argv[1:]
document = json.loads(pathlib.Path(source).read_text(encoding="utf-8"))
exec(expression, {"document": document})
pathlib.Path(target).write_text(json.dumps(document), encoding="utf-8")
PY
    if RADAR_EVIDENCE_MANIFEST_KEY=$(printf 'k%.0s' {1..32}) "$ACCEPTANCE_SCRIPT" "$TEST_ROOT/$name.json" >"$TEST_ROOT/$name.out" 2>&1; then
        fail "acceptance script allowed invalid evidence: $name"
    fi
    grep -Fq "$expected" "$TEST_ROOT/$name.out" || \
        fail "invalid evidence $name did not report $expected"
}

assert_rejected p99 \
    'document["reliability_snapshots"][0]["p99_latency_ms"] = 501' \
    'p99_latency_ms: exceeds p99_slo_ms'
assert_rejected denominator \
    'document["reliability_snapshots"][0]["request_count"] = 601' \
    'terminal outcomes do not equal request_count'
assert_rejected error-rate \
    'document["reliability_snapshots"][0]["error_rate"] = "0.001000"' \
    'error_rate: does not match the full request denominator'
assert_rejected billing \
    'document["reliability_snapshots"][0]["billing_idempotency_failures"] = 1' \
    'billing_idempotency_failures: must be zero'
assert_rejected rpo \
    'document["recovery"]["rpo_seconds"] = 301' \
    'rpo_seconds: exceeds rpo_limit_seconds'
assert_rejected rto \
    'document["recovery"]["rto_seconds"] = 1801' \
    'rto_seconds: exceeds rto_limit_seconds'
assert_rejected recovery-hash \
    'document["recovery"]["deterministic_acceptance"]["post_recovery_hash"] = "7777777777777777777777777777777777777777777777777777777777777777"' \
    'deterministic acceptance hash changed after recovery'
assert_rejected rollback \
    'del document["rollback"]["previous_worker_image_digest"]' \
    'rollback.previous_worker_image_digest: is required'
assert_rejected ledger \
    'document["rollback"]["budget_ledger_total_after"] = "10.00000001"' \
    'rollback budget ledger totals changed'
assert_rejected fact-run \
    'document["fact_manifest"]["snapshot_refs"][0]["run_id"] = "00000000-0000-4000-8000-000000000099"' \
    'fact_manifest.snapshot_refs[0].run_id: does not match fact manifest run_id'
assert_rejected fact-plan \
    'document["fact_manifest"]["load_plan_id"] = "00000000-0000-4000-8000-000000000099"' \
    'fact_manifest.load_plan_id: does not match release.load_plan_id'
assert_rejected fact-profile \
    'document["fact_manifest"]["snapshot_refs"][0]["profile_id"] = "profile-tampered"' \
    'fact_manifest.snapshot_refs[0].profile_id: does not match fact manifest profile_id'
assert_rejected fact-snapshot-hash \
    'document["reliability_snapshots"][0]["snapshot_hash"] = "9999999999999999999999999999999999999999999999999999999999999999"' \
    'reliability_snapshots[0].snapshot_hash: does not recompute from immutable fields'
assert_rejected fact-watermark \
    'document["fact_manifest"]["recovery_ref"]["source_watermark"] = "9999999999999999999999999999999999999999999999999999999999999999"' \
    'fact_manifest.recovery_ref.source_watermark: does not match recovery source watermark'
assert_rejected fact-artifact \
    'document["fact_manifest"]["artifact_manifest_hashes"][0] = "9999999999999999999999999999999999999999999999999999999999999999"' \
    'fact_manifest.artifact_manifest_hashes: does not match rollback artifact manifest hashes'

printf 'Radar staging reliability contract checks passed.\n'
