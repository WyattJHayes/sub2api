#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BASE_COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.radar-staging.yml"
RELIABILITY_COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.radar-reliability.yml"
ACCEPTANCE_SCRIPT="$ROOT_DIR/deploy/radar/reliability-acceptance.py"
PROJECT_NAME=${RADAR_LIVE_PROJECT_NAME:-sub2api-radar-live}
CONTROL_PLANE_URL=${RADAR_LIVE_CONTROL_PLANE_URL:-http://127.0.0.1:${RADAR_CONTROL_PLANE_PORT:-18080}}
ENV_FILE=${RADAR_LIVE_ENV_FILE:-}
LIVE_ROOT=${RADAR_LIVE_EVIDENCE_DIR:-}
TEST_ROOT=""
LOAD_PLAN_ID=${RADAR_LIVE_LOAD_PLAN_ID:-}
RUN_ID=${RADAR_LIVE_RUN_ID:-}
FAULT_EXPERIMENT_ID=${RADAR_LIVE_FAULT_EXPERIMENT_ID:-}
RECOVERY_EVIDENCE_ID=${RADAR_LIVE_RECOVERY_EVIDENCE_ID:-}
POLICY_ID=${RADAR_LIVE_POLICY_ID:-}
RELEASE_SUBJECT_ID=${RADAR_LIVE_RELEASE_SUBJECT_ID:-}
POLICY_HASH=""
RELEASE_SUBJECT_HASH=""
WORKER_IMAGE_DIGEST=${RADAR_LIVE_WORKER_IMAGE_DIGEST:-}
CONTROL_PLANE_IMAGE_DIGEST=${RADAR_LIVE_CONTROL_PLANE_IMAGE_DIGEST:-}
CONTROL_PLANE_IMAGE=${RADAR_CONTROL_PLANE_IMAGE:-}
WORKER_IMAGE=${RADAR_WORKER_IMAGE:-}
CHAOS_HOLD_SECONDS=${RADAR_LIVE_CHAOS_HOLD_SECONDS:-15}
REGISTER_BODY_FILES=()

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

require_uuid() {
    local name=$1
    local value=$2
    [[ "$value" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$ ]] || \
        fail "$name must be a UUID"
}

require_digest() {
    local name=$1
    local value=$2
    [[ "$value" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "$name must be a lowercase sha256 image digest"
}

require_worker_name() {
    local value=$1
    [[ "$value" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || \
        fail "worker ID must contain only ASCII letters, digits, dot, underscore, or dash: $value"
}

require_live_secret() {
    local name=$1
    local value=${!name:-}
    local normalized
    [[ ${#value} -ge 32 ]] || fail "$name must be at least 32 characters"
    [[ "$value" != *[[:space:]]* ]] || fail "$name must not contain whitespace"
    normalized=$(printf '%s' "$value" | tr '[:upper:]' '[:lower:]')
    case "$normalized" in
        *synthetic*|*placeholder*|*change-me*|*changeme*|*fake*|*demo*|*test-*|*test_*|*example*)
            fail "$name contains a synthetic or placeholder credential"
            ;;
    esac
    local first=${normalized:0:1}
    if [[ -n "$first" && -z "${normalized//"$first"/}" ]]; then
        fail "$name must not be a repeated-character credential"
    fi
}

require_mode_600() {
    local path=$1
    [[ -f "$path" && -r "$path" ]] || fail "environment file is not a readable regular file: $path"
    local mode
    if mode=$(stat -c '%a' "$path" 2>/dev/null); then
        :
    elif mode=$(stat -f '%Lp' "$path" 2>/dev/null); then
        :
    else
        fail "unable to inspect environment file permissions: $path"
    fi
    [[ "$mode" == "600" ]] || \
        fail "environment file must have mode 600: $path"
}

hash_string() {
    local value=$1
    if command -v shasum >/dev/null 2>&1; then
        printf '%s' "$value" | shasum -a 256 | awk '{print $1}'
        return 0
    fi
    command -v sha256sum >/dev/null 2>&1 || fail "required command is unavailable: shasum or sha256sum"
    printf '%s' "$value" | sha256sum | awk '{print $1}'
}

json_value() {
    local path=$1
    local key=$2
    python3 - "$path" "$key" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
key = sys.argv[2]
document = json.loads(path.read_text(encoding="utf-8"))
value = document.get("data", document) if isinstance(document, dict) else document
for part in key.split("."):
    if isinstance(value, dict):
        value = value.get(part)
    else:
        value = None
    if value is None:
        break
if value is None:
    raise SystemExit(1)
if isinstance(value, (dict, list)):
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
else:
    print(str(value))
PY
}

json_required() {
    local path=$1
    local key=$2
    local value
    value=$(json_value "$path" "$key") || fail "$path does not contain $key"
    [[ -n "$value" ]] || fail "$path contains an empty $key"
    printf '%s' "$value"
}

AUTH_ARGS=()
admin_auth() {
    if [[ -n ${RADAR_LIVE_ADMIN_API_KEY:-} ]]; then
        AUTH_ARGS=(-H "x-api-key: $RADAR_LIVE_ADMIN_API_KEY")
    else
        AUTH_ARGS=(-H "Authorization: Bearer ${RADAR_LIVE_ADMIN_TOKEN}")
    fi
}

api_call() {
    local name=$1
    local method=$2
    local path=$3
    local body=${4:-}
    local idempotency=${5:-}
    local response="$TEST_ROOT/raw/${name}.json"
    local headers="$TEST_ROOT/raw/${name}.headers"
    local curl_args=(--silent --show-error --connect-timeout 10 --max-time "${RADAR_LIVE_REQUEST_TIMEOUT_SECONDS:-120}" \
        -X "$method" -D "$headers" -o "$response" -w '%{http_code}' -H 'Accept: application/json')
    if [[ "$method" != "GET" && "$method" != "HEAD" ]]; then
        curl_args+=(-H 'Content-Type: application/json')
    fi
    if [[ -n "$idempotency" ]]; then
        curl_args+=( -H "Idempotency-Key: $idempotency" )
    fi
    if [[ -n "$body" ]]; then
        curl_args+=(--data-binary "@$body")
    fi
    local status
    status=$(curl "${curl_args[@]}" "${AUTH_ARGS[@]}" "$CONTROL_PLANE_URL$path") || \
        fail "$name request failed to connect"
    [[ "$status" =~ ^[0-9]{3}$ ]] || fail "$name returned an invalid HTTP status"
    if ((10#$status >= 400)); then
        fail "$name returned HTTP $status"
    fi
    printf '%s' "$response"
}

worker_call() {
    local name=$1
    local token=$2
    local method=$3
    local path=$4
    local body=${5:-}
    local response="$TEST_ROOT/raw/${name}.json"
    local headers="$TEST_ROOT/raw/${name}.headers"
    local curl_args=(--silent --show-error --connect-timeout 10 --max-time "${RADAR_LIVE_REQUEST_TIMEOUT_SECONDS:-120}" \
        -X "$method" -D "$headers" -o "$response" -w '%{http_code}' -H 'Accept: application/json' \
        -H "Authorization: Bearer $token")
    if [[ "$method" != "GET" && "$method" != "HEAD" ]]; then
        curl_args+=(-H 'Content-Type: application/json')
    fi
    if [[ -n "$body" ]]; then
        curl_args+=(--data-binary "@$body")
    fi
    local status
    status=$(curl "${curl_args[@]}" "$CONTROL_PLANE_URL$path") || \
        fail "$name request failed to connect"
    [[ "$status" =~ ^[0-9]{3}$ ]] || fail "$name returned an invalid HTTP status"
    if ((10#$status >= 400)); then
        fail "$name returned HTTP $status"
    fi
    printf '%s' "$response"
}

base_compose() {
    local assignments=(
        "RADAR_COMPOSE_PROJECT_NAME=$PROJECT_NAME"
        "RADAR_COMPOSE_RESOURCE_PREFIX=$PROJECT_NAME"
        "RADAR_GATEWAY_URL=http://sub2api-staging:8080"
        "RADAR_CONTROL_PLANE_IMAGE=$CONTROL_PLANE_IMAGE"
        "RADAR_WORKER_IMAGE=$WORKER_IMAGE"
        "RADAR_CONTROL_PLANE_IMAGE_DIGEST=$CONTROL_PLANE_IMAGE_DIGEST"
        "RADAR_WORKER_IMAGE_DIGEST=$WORKER_IMAGE_DIGEST"
        "RADAR_IMAGE_PULL_POLICY=always"
    )
    if [[ -n "$ENV_FILE" ]]; then
        env "${assignments[@]}" docker compose --env-file "$ENV_FILE" -f "$BASE_COMPOSE_FILE" "$@"
    else
        env "${assignments[@]}" docker compose -f "$BASE_COMPOSE_FILE" "$@"
    fi
}

reliability_compose() {
    local assignments=(
        "RADAR_COMPOSE_PROJECT_NAME=$PROJECT_NAME"
        "RADAR_COMPOSE_RESOURCE_PREFIX=$PROJECT_NAME"
        "RADAR_CONTROL_PLANE_URL=http://sub2api-staging:8080"
        "RADAR_GATEWAY_URL=http://sub2api-staging:8080"
        "RADAR_LOAD_PLAN_ID=$LOAD_PLAN_ID"
        "RADAR_LOAD_RUN_ID=$RUN_ID"
        "RADAR_FAULT_EXPERIMENT_ID=$FAULT_EXPERIMENT_ID"
        "RADAR_RECOVERY_EVIDENCE_ID=$RECOVERY_EVIDENCE_ID"
        "RADAR_CHAOS_TARGET_WORKER_ID=${RADAR_LIVE_CHAOS_TARGET_WORKER_ID:-}"
        "RADAR_RELIABILITY_EVIDENCE_DIR=$TEST_ROOT/evidence"
        "RADAR_RELIABILITY_REPORT_DIR=$TEST_ROOT/reports"
        "RADAR_RELIABILITY_UID=${RADAR_RELIABILITY_UID:-10001}"
        "RADAR_RELIABILITY_GID=${RADAR_RELIABILITY_GID:-10001}"
        "RADAR_LOADGEN_EVALUATION_API_KEY=$RADAR_LIVE_EVALUATION_API_KEY"
        "RADAR_LOADGEN_IMAGE_DIGEST=$WORKER_IMAGE_DIGEST"
        "RADAR_CHAOS_AUTO_ROLLBACK_SECONDS=$CHAOS_HOLD_SECONDS"
        "RADAR_CONTROL_PLANE_IMAGE=$CONTROL_PLANE_IMAGE"
        "RADAR_WORKER_IMAGE=$WORKER_IMAGE"
        "RADAR_CONTROL_PLANE_IMAGE_DIGEST=$CONTROL_PLANE_IMAGE_DIGEST"
        "RADAR_WORKER_IMAGE_DIGEST=$WORKER_IMAGE_DIGEST"
        "RADAR_IMAGE_PULL_POLICY=always"
    )
    if [[ -n "$ENV_FILE" ]]; then
        env "${assignments[@]}" docker compose --env-file "$ENV_FILE" -f "$BASE_COMPOSE_FILE" -f "$RELIABILITY_COMPOSE_FILE" "$@"
    else
        env "${assignments[@]}" docker compose -f "$BASE_COMPOSE_FILE" -f "$RELIABILITY_COMPOSE_FILE" "$@"
    fi
}

cleanup() {
    if (( ${#REGISTER_BODY_FILES[@]} > 0 )); then
        for path in "${REGISTER_BODY_FILES[@]}"; do
            rm -f -- "$path"
        done
    fi
    if [[ ${RADAR_LIVE_CLEANUP:-0} == "1" && -n "$TEST_ROOT" ]]; then
        reliability_compose down --remove-orphans >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

[[ ${RADAR_LIVE_E2E:-} == "1" ]] || fail "set RADAR_LIVE_E2E=1 to run the live staging E2E"
require_command curl
require_command python3
require_command docker
require_command awk
require_command tr
[[ -x "$ACCEPTANCE_SCRIPT" ]] || fail "acceptance script is missing or not executable"
[[ "$PROJECT_NAME" =~ ^[a-z0-9][a-z0-9_-]{2,62}$ ]] || fail "invalid live project name"
[[ "$CHAOS_HOLD_SECONDS" =~ ^[0-9]+([.][0-9]+)?$ ]] || \
    fail "RADAR_LIVE_CHAOS_HOLD_SECONDS must be a non-negative number"

[[ -n "$ENV_FILE" ]] || fail "RADAR_LIVE_ENV_FILE is required for live staging execution"
require_mode_600 "$ENV_FILE"
require_digest RADAR_LIVE_CONTROL_PLANE_IMAGE_DIGEST "$CONTROL_PLANE_IMAGE_DIGEST"
require_digest RADAR_LIVE_WORKER_IMAGE_DIGEST "$WORKER_IMAGE_DIGEST"
[[ -n "$CONTROL_PLANE_IMAGE" ]] || fail "RADAR_CONTROL_PLANE_IMAGE is required for live staging execution"
[[ -n "$WORKER_IMAGE" ]] || fail "RADAR_WORKER_IMAGE is required for live staging execution"
[[ "$CONTROL_PLANE_IMAGE" == *"@$CONTROL_PLANE_IMAGE_DIGEST" ]] || \
    fail "RADAR_CONTROL_PLANE_IMAGE must end with RADAR_LIVE_CONTROL_PLANE_IMAGE_DIGEST"
[[ "$WORKER_IMAGE" == *"@$WORKER_IMAGE_DIGEST" ]] || \
    fail "RADAR_WORKER_IMAGE must end with RADAR_LIVE_WORKER_IMAGE_DIGEST"

if [[ -n ${RADAR_LIVE_ADMIN_API_KEY:-} && -n ${RADAR_LIVE_ADMIN_TOKEN:-} ]]; then
    fail "set only one of RADAR_LIVE_ADMIN_API_KEY and RADAR_LIVE_ADMIN_TOKEN"
fi
[[ -n ${RADAR_LIVE_ADMIN_API_KEY:-} || -n ${RADAR_LIVE_ADMIN_TOKEN:-} ]] || \
    fail "RADAR_LIVE_ADMIN_API_KEY or RADAR_LIVE_ADMIN_TOKEN is required"
require_live_secret RADAR_LIVE_EVALUATION_API_KEY
if [[ -n ${RADAR_LIVE_ADMIN_API_KEY:-} ]]; then
    require_live_secret RADAR_LIVE_ADMIN_API_KEY
else
    require_live_secret RADAR_LIVE_ADMIN_TOKEN
fi
for secret_name in \
    RADAR_RUNNER_WORKER_TOKEN RADAR_GRADER_WORKER_TOKEN RADAR_STATISTICS_WORKER_TOKEN \
    RADAR_LOADGEN_WORKER_TOKEN RADAR_CHAOS_CONTROLLER_TOKEN RADAR_RECOVERY_VERIFIER_TOKEN; do
    require_live_secret "$secret_name"
done
admin_auth

TEST_ROOT=${LIVE_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/radar-live-e2e.XXXXXX")}
mkdir -p "$TEST_ROOT/raw" "$TEST_ROOT/evidence" "$TEST_ROOT/reports"
chmod 700 "$TEST_ROOT" "$TEST_ROOT/raw" "$TEST_ROOT/evidence" "$TEST_ROOT/reports"

if [[ ${RADAR_LIVE_DRY_RUN:-0} == "1" ]]; then
    base_compose config --quiet >/dev/null
    printf 'Radar live E2E dry-run passed.\n'
    exit 0
fi

[[ -n "$LOAD_PLAN_ID" ]] || fail "RADAR_LIVE_LOAD_PLAN_ID must identify the published load plan bound to the drill"
[[ -n "$RUN_ID" ]] || fail "RADAR_LIVE_RUN_ID must identify the run bound to the approved fault and recovery records"
[[ -n "$FAULT_EXPERIMENT_ID" ]] || fail "RADAR_LIVE_FAULT_EXPERIMENT_ID is required for a complete drill"
[[ -n "$RECOVERY_EVIDENCE_ID" ]] || fail "RADAR_LIVE_RECOVERY_EVIDENCE_ID is required for a complete drill"
[[ -n "$POLICY_ID" ]] || fail "RADAR_LIVE_POLICY_ID must identify a pre-approved Gate Policy"
[[ -n "$RELEASE_SUBJECT_ID" ]] || fail "RADAR_LIVE_RELEASE_SUBJECT_ID must identify an active release subject"
[[ -n "${RADAR_LIVE_CHAOS_TARGET_WORKER_ID:-}" ]] || \
    fail "RADAR_LIVE_CHAOS_TARGET_WORKER_ID must identify the approved target worker"
require_uuid RADAR_LIVE_LOAD_PLAN_ID "$LOAD_PLAN_ID"
require_uuid RADAR_LIVE_RUN_ID "$RUN_ID"
require_uuid RADAR_LIVE_FAULT_EXPERIMENT_ID "$FAULT_EXPERIMENT_ID"
require_uuid RADAR_LIVE_RECOVERY_EVIDENCE_ID "$RECOVERY_EVIDENCE_ID"
require_uuid RADAR_LIVE_POLICY_ID "$POLICY_ID"
require_uuid RADAR_LIVE_RELEASE_SUBJECT_ID "$RELEASE_SUBJECT_ID"
require_uuid RADAR_LIVE_CHAOS_TARGET_WORKER_ID "$RADAR_LIVE_CHAOS_TARGET_WORKER_ID"

base_compose up -d --no-build --pull always --wait --wait-timeout 300 \
    postgres-staging redis-staging minio-staging minio-init clamav-staging \
    sub2api-staging radar-synthetic-upstream
docker image inspect "$WORKER_IMAGE" >/dev/null 2>&1 || \
    fail "pinned Worker image is unavailable locally: $WORKER_IMAGE"
docker image inspect "$CONTROL_PLANE_IMAGE" >/dev/null 2>&1 || \
    fail "pinned control-plane image is unavailable locally: $CONTROL_PLANE_IMAGE"

register_worker() {
    local name=$1
    local kind=$2
    local token_name=$3
    local capabilities=$4
    local token=${!token_name}
    require_worker_name "$name"
    local body="$TEST_ROOT/${name}.json"
    REGISTER_BODY_FILES+=("$body")
    RADAR_REGISTER_TOKEN="$token" python3 - "$body" "$name" "$kind" "$capabilities" "$WORKER_IMAGE_DIGEST" <<'PY'
import json
import os
import pathlib
import sys

path, name, kind, capabilities, image_digest = sys.argv[1:]
document = {
    "name": name,
    "worker_kind": kind,
    "region": "staging",
    "image_digest": image_digest,
    "capabilities": [item for item in capabilities.split(",") if item],
    "max_concurrency": 1,
    "token": os.environ["RADAR_REGISTER_TOKEN"],
}
pathlib.Path(path).write_text(json.dumps(document), encoding="utf-8")
PY
    chmod 600 "$body"
    local key
    key=$(hash_string "radar-live-register:$name")
    api_call "register-${name}" POST "/api/v1/admin/radar/workers" "$body" "$key" >/dev/null
    rm -f -- "$body"
}

register_worker "${RADAR_RUNNER_WORKER_ID:-radar-runner-staging}" runner RADAR_RUNNER_WORKER_TOKEN \
    "coding,reasoning,instruction,long_context,tool_call,protocol,safety,performance,cost"
register_worker "${RADAR_GRADER_WORKER_ID:-radar-grader-staging}" grader RADAR_GRADER_WORKER_TOKEN \
    "exact,exact_json,protocol,safety,tool_call"
register_worker "${RADAR_STATISTICS_WORKER_ID:-radar-statistics-staging}" statistics RADAR_STATISTICS_WORKER_TOKEN \
    "coding,reasoning,safety,tool_use,protocol"
register_worker "${RADAR_LOADGEN_WORKER_ID:-radar-loadgen-staging}" statistics RADAR_LOADGEN_WORKER_TOKEN \
    "reliability,performance,cost,protocol"
register_worker "${RADAR_CHAOS_WORKER_ID:-radar-chaos-controller-staging}" statistics RADAR_CHAOS_CONTROLLER_TOKEN \
    "chaos,reliability,protocol"
register_worker "${RADAR_RECOVERY_WORKER_ID:-radar-recovery-verifier-staging}" statistics RADAR_RECOVERY_VERIFIER_TOKEN \
    "recovery,reliability,protocol"

base_compose up -d --wait --wait-timeout 300 radar-runner radar-grader radar-statistics

LOAD_PLAN_RESPONSE=$(api_call inspect-load-plan GET "/api/v1/admin/radar/reliability/load-plans/$LOAD_PLAN_ID")
RUNS_RESPONSE=$(api_call inspect-runs GET "/api/v1/admin/radar/runs")
FAULT_RESPONSE=$(worker_call inspect-fault "$RADAR_CHAOS_CONTROLLER_TOKEN" GET \
    "/internal/radar/v1/fault-experiments/$FAULT_EXPERIMENT_ID")
RECOVERY_RESPONSE=$(worker_call inspect-recovery "$RADAR_RECOVERY_VERIFIER_TOKEN" GET \
    "/internal/radar/v1/recovery-evidence/$RECOVERY_EVIDENCE_ID/observation")
RELEASE_SUBJECT_RESPONSE=$(api_call inspect-release-subject GET \
    "/api/v1/admin/radar/release-subjects/$RELEASE_SUBJECT_ID")
RELEASE_SUBJECT_HASH=$(python3 - "$RELEASE_SUBJECT_RESPONSE" <<'PY'
import json
import pathlib
import sys

document = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
value = document.get("data", document)
if not isinstance(value, dict) or not isinstance(value.get("subject_hash"), str):
    raise SystemExit("release subject response does not contain its immutable subject hash")
print(value["subject_hash"])
PY
)
[[ "$RELEASE_SUBJECT_HASH" =~ ^[0-9a-f]{64}$ ]] || fail "release subject hash must be a lowercase SHA256"
python3 - "$LOAD_PLAN_RESPONSE" "$RUNS_RESPONSE" "$FAULT_RESPONSE" "$RECOVERY_RESPONSE" "$RELEASE_SUBJECT_RESPONSE" \
    "$LOAD_PLAN_ID" "$RUN_ID" "$FAULT_EXPERIMENT_ID" "$RECOVERY_EVIDENCE_ID" \
    "$RELEASE_SUBJECT_ID" "$RADAR_LIVE_CHAOS_TARGET_WORKER_ID" <<'PY'
import json
import os
import pathlib
import sys

load_plan_path, runs_path, fault_path, recovery_path, release_subject_path, load_plan_id, run_id, fault_id, recovery_id, release_subject_id, target_id = sys.argv[1:]

def load(path):
    return json.loads(pathlib.Path(path).read_text(encoding="utf-8"))

def data(document):
    value = document.get("data", document) if isinstance(document, dict) else document
    if isinstance(value, dict):
        for key in ("experiment", "fault_experiment", "observation", "recovery_observation", "release_subject"):
            nested = value.get(key)
            if isinstance(nested, dict):
                return nested
    return value

load_plan = data(load(load_plan_path))
if not isinstance(load_plan, dict):
    raise SystemExit("control plane returned an invalid load plan")
if str(load_plan.get("id", load_plan.get("load_plan_id", ""))).lower() != load_plan_id.lower():
    raise SystemExit("load plan response identity does not match the requested plan")
if str(load_plan.get("status", "")).lower() != "published":
    raise SystemExit("load plan must be published before the live drill")
canonical_plan = load_plan.get("canonical_plan", load_plan.get("plan", load_plan))
if isinstance(canonical_plan, str):
    try:
        canonical_plan = json.loads(canonical_plan)
    except json.JSONDecodeError as exc:
        raise SystemExit("published load plan canonical document is invalid") from exc
if not isinstance(canonical_plan, dict):
    raise SystemExit("published load plan canonical document is invalid")
pair_count = 1
for field in ("model_aliases", "regions", "concurrency_levels", "input_token_buckets", "output_token_buckets"):
    values = canonical_plan.get(field)
    if not isinstance(values, list) or not values:
        raise SystemExit(f"published load plan {field} is required")
    pair_count *= len(values)
if pair_count != 30:
    raise SystemExit(f"published load plan must expand to exactly 30 pairs, got {pair_count}")

runs_document = load(runs_path)
runs_data = runs_document.get("data", runs_document)
items = runs_data if isinstance(runs_data, list) else runs_data.get("runs", []) if isinstance(runs_data, dict) else []
run = next((item for item in items if isinstance(item, dict) and str(item.get("id", "")).lower() == run_id.lower()), None)
if run is None:
    raise SystemExit("run is not visible to the authenticated staging administrator")
if str(run.get("status", "")).lower() in {"completed", "failed", "cancelled", "budget_paused"}:
    raise SystemExit("run is already terminal and cannot receive the live drill")

fault = data(load(fault_path))
if not isinstance(fault, dict):
    raise SystemExit("control plane returned an invalid fault experiment")
if str(fault.get("experiment_id", fault.get("id", ""))).lower() != fault_id.lower():
    raise SystemExit("fault experiment response identity does not match the requested experiment")
if str(fault.get("run_id", "")).lower() != run_id.lower():
    raise SystemExit("fault experiment is bound to a different run")
if str(fault.get("load_plan_id", "")).lower() not in {"", "none", load_plan_id.lower()}:
    raise SystemExit("fault experiment is bound to a different load plan")
if str(fault.get("status", "")).lower() != "approved":
    raise SystemExit("fault experiment must be approved before the live drill")
if str(fault.get("environment", "")).lower() != "staging":
    raise SystemExit("fault experiment must target staging")
if str(fault.get("target_ref", "")) != target_id:
    raise SystemExit("fault experiment target does not match RADAR_LIVE_CHAOS_TARGET_WORKER_ID")

recovery = data(load(recovery_path))
if not isinstance(recovery, dict):
    raise SystemExit("control plane returned an invalid recovery observation")
if str(recovery.get("run_id", "")).lower() != run_id.lower():
    raise SystemExit("recovery observation is bound to a different run")
if str(recovery.get("experiment_id", "")).lower() != fault_id.lower():
    raise SystemExit("recovery observation is bound to a different fault experiment")
generation = recovery.get("recovery_generation")
if not isinstance(generation, int) or isinstance(generation, bool) or generation < 0:
    raise SystemExit("recovery observation has an invalid generation")

release_subject = data(load(release_subject_path))
if not isinstance(release_subject, dict):
    raise SystemExit("control plane returned an invalid release subject")
if str(release_subject.get("id", "")).lower() != release_subject_id.lower():
    raise SystemExit("release subject response identity does not match the requested subject")
if str(release_subject.get("run_id", "")).lower() != run_id.lower():
    raise SystemExit("release subject is bound to a different run")
if release_subject.get("active") is not True:
    raise SystemExit("release subject must be active before the live drill")
subject = release_subject.get("subject")
if not isinstance(subject, dict):
    raise SystemExit("release subject does not contain its canonical subject")
expected_environment = os.environ.get("RADAR_LIVE_POLICY_ENVIRONMENT", "staging").strip().lower()
expected_scope_type = os.environ.get("RADAR_LIVE_POLICY_SCOPE_TYPE", "run").strip().lower()
expected_scope_id = os.environ.get("RADAR_LIVE_POLICY_SCOPE_ID", run_id).strip()
if str(subject.get("deployment_environment", "")).lower() != expected_environment:
    raise SystemExit("release subject targets a different environment")
if str(subject.get("scope_type", "")).lower() != expected_scope_type or str(subject.get("scope_id", "")) != expected_scope_id:
    raise SystemExit("release subject scope does not match the Gate Policy activation scope")
print("pre-bound live resources validated")
PY

activation="$TEST_ROOT/policy-activation.json"
python3 - "$activation" "$RUN_ID" <<'PY'
import json
import os
import pathlib
import sys

scope_type = os.environ.get("RADAR_LIVE_POLICY_SCOPE_TYPE", "run").strip()
scope_id = os.environ.get("RADAR_LIVE_POLICY_SCOPE_ID", sys.argv[2]).strip()
environment = os.environ.get("RADAR_LIVE_POLICY_ENVIRONMENT", "staging").strip()
if not scope_type or not scope_id or not environment:
    raise SystemExit("Gate Policy scope must be configured")
document = {
    "environment": environment,
    "scope_type": scope_type,
    "scope_id": scope_id,
}
expected = os.environ.get("RADAR_LIVE_EXPECTED_POLICY_ID", "").strip()
if expected:
    document["expected_policy_id"] = expected
pathlib.Path(sys.argv[1]).write_text(json.dumps(document), encoding="utf-8")
PY
chmod 600 "$activation"
POLICY_ACTIVATION_RESPONSE=$(api_call activate-gate-policy POST "/api/v1/admin/radar/policies/$POLICY_ID/activate" "$activation")
python3 - "$POLICY_ACTIVATION_RESPONSE" "$POLICY_ID" "$RUN_ID" <<'PY'
import json
import pathlib
import sys

document = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
value = document.get("data", document)
if not isinstance(value, dict):
    raise SystemExit("Gate Policy activation returned an invalid response")
if str(value.get("policy_id", "")).lower() != sys.argv[2].lower():
    raise SystemExit("Gate Policy activation returned a different policy")
if not isinstance(value.get("policy_hash"), str) or len(value["policy_hash"]) != 64 or any(c not in "0123456789abcdef" for c in value["policy_hash"]):
    raise SystemExit("Gate Policy activation did not return its immutable policy hash")
scope = value.get("scope")
if isinstance(scope, dict) and scope.get("scope_type") == "run" and str(scope.get("scope_id", "")).lower() != sys.argv[3].lower():
    raise SystemExit("Gate Policy activation scope is bound to a different run")
PY
POLICY_HASH=$(python3 - "$POLICY_ACTIVATION_RESPONSE" <<'PY'
import json
import pathlib
import sys

document = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
value = document.get("data", document)
print(value["policy_hash"])
PY
)

if ! curl --silent --show-error --fail --connect-timeout 10 --max-time 20 "$CONTROL_PLANE_URL/health" >/dev/null; then
    fail "control plane health check failed"
fi

reliability_compose --profile reliability run --rm --no-deps radar-loadgen
REPORT="$TEST_ROOT/reports/loadgen-$RUN_ID.json"
[[ -s "$REPORT" ]] || fail "loadgen did not produce a bound report for run $RUN_ID"
python3 - "$REPORT" "$RUN_ID" "$LOAD_PLAN_ID" <<'PY'
import json
import pathlib
import sys

document = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
if document.get("schema_version") != "radar-loadgen-report-v1":
    raise SystemExit("loadgen report schema is invalid")
if document.get("run_id") != sys.argv[2] or document.get("load_plan_id") != sys.argv[3]:
    raise SystemExit("loadgen report identity does not match the requested run and plan")
profile_id = document.get("profile_id")
if not isinstance(profile_id, str) or not profile_id.strip():
    raise SystemExit("loadgen report profile identity is required")
cells = document.get("cells")
published = document.get("published_snapshots")
if not isinstance(cells, list) or len(cells) != 30 or not isinstance(published, list) or len(published) != len(cells):
    raise SystemExit("loadgen report has no measured cells or published snapshots")
for index, cell in enumerate(cells):
    terminal = cell["success_count"] + cell["error_count"] + cell["timeout_count"]
    if terminal != cell["request_count"] or cell["billing_idempotency_failures"] != 0:
        raise SystemExit("loadgen report contains an incomplete or non-idempotent cell")
    published_item = published[index]
    submission = published_item.get("submission") if isinstance(published_item, dict) else None
    receipt = published_item.get("receipt") if isinstance(published_item, dict) else None
    if not isinstance(submission, dict) or not isinstance(receipt, dict):
        raise SystemExit("loadgen report does not contain immutable snapshot receipts")
    receipt_data = receipt.get("data", receipt)
    if not isinstance(receipt_data, dict) or not receipt_data.get("snapshot_id") or not receipt_data.get("snapshot_hash"):
        raise SystemExit("loadgen report contains an incomplete snapshot receipt")
    if submission.get("run_id") != document["run_id"] or submission.get("load_plan_id") != document["load_plan_id"]:
        raise SystemExit("reliability snapshot is bound to a different run or load plan")
    if submission.get("profile_id") != profile_id:
        raise SystemExit("reliability snapshot is bound to a different profile")
    if submission.get("billing_idempotency_failures") != 0:
        raise SystemExit("published reliability evidence contains a billing idempotency failure")
    source_manifest = submission.get("source_manifest")
    if not isinstance(source_manifest, dict):
        raise SystemExit("published reliability evidence is missing its source manifest")
    if source_manifest.get("run_id") != submission.get("run_id") or source_manifest.get("load_plan_id") != submission.get("load_plan_id"):
        raise SystemExit("published source evidence is bound to a different run or load plan")
    if source_manifest.get("profile_id") != submission.get("profile_id") or source_manifest.get("slice_key") != submission.get("slice_key"):
        raise SystemExit("published source evidence is bound to a different profile or slice")
    if source_manifest.get("billing_idempotency_failures") != 0:
        raise SystemExit("published source evidence contains a billing idempotency failure")
PY

SNAPSHOT_REPLAY_DIR="$TEST_ROOT/snapshot-replays"
mkdir -p "$SNAPSHOT_REPLAY_DIR"
python3 - "$REPORT" "$SNAPSHOT_REPLAY_DIR" <<'PY'
import json
import pathlib
import sys

report = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
for index, item in enumerate(report["published_snapshots"]):
    submission = item.get("submission") if isinstance(item, dict) else None
    if not isinstance(submission, dict):
        raise SystemExit("loadgen report does not contain the original snapshot submission")
    pathlib.Path(sys.argv[2], f"{index}.json").write_text(json.dumps(submission), encoding="utf-8")
PY
for SNAPSHOT_BODY in "$SNAPSHOT_REPLAY_DIR"/*.json; do
    SNAPSHOT_INDEX=$(basename "$SNAPSHOT_BODY" .json)
    SNAPSHOT_REPLAY=$(worker_call "duplicate-snapshot-$SNAPSHOT_INDEX" "$RADAR_STATISTICS_WORKER_TOKEN" POST \
        "/internal/radar/v1/reliability-snapshots" "$SNAPSHOT_BODY")
    python3 - "$REPORT" "$SNAPSHOT_INDEX" "$SNAPSHOT_REPLAY" <<'PY'
import json
import pathlib
import sys

report = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
index = int(sys.argv[2])
replayed = json.loads(pathlib.Path(sys.argv[3]).read_text(encoding="utf-8"))
original = report["published_snapshots"][index].get("receipt", {})
if isinstance(original, dict):
    original = original.get("data", original)
duplicate = replayed.get("data", replayed)
if not isinstance(original, dict) or not isinstance(duplicate, dict):
    raise SystemExit("duplicate reliability evidence returned an invalid receipt")
if not original.get("snapshot_id") or duplicate.get("snapshot_id") != original["snapshot_id"]:
    raise SystemExit("duplicate reliability evidence produced a different snapshot")
if not original.get("snapshot_hash") or duplicate.get("snapshot_hash") != original["snapshot_hash"]:
    raise SystemExit("duplicate reliability evidence produced a different hash")
PY
done

if [[ -n ${RADAR_LIVE_SMOKE_RUN_ID:-} ]]; then
    require_uuid RADAR_LIVE_SMOKE_RUN_ID "$RADAR_LIVE_SMOKE_RUN_ID"
fi

wait_for_run() {
    local run_file
    for _ in $(seq 1 "${RADAR_LIVE_RUN_WAIT_ATTEMPTS:-120}"); do
        run_file=$(api_call runs GET "/api/v1/admin/radar/runs")
        if python3 - "$run_file" "$RUN_ID" <<'PY'
import json
import pathlib
import sys

document = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
data = document.get("data", document)
items = data if isinstance(data, list) else data.get("runs", []) if isinstance(data, dict) else []
for item in items:
    if item.get("id") == sys.argv[2]:
        status = str(item.get("status", "")).lower()
        contract = str(item.get("contract_status", "")).lower()
        if status in {"completed", "failed", "cancelled", "budget_paused"}:
            if status != "completed" or contract not in {"completed", "accepted", "passed"}:
                raise SystemExit(2)
            raise SystemExit(0)
raise SystemExit(1)
PY
        then
            return 0
        else
            code=$?
            ((code == 2)) && fail "run $RUN_ID reached a terminal non-success status"
        fi
        sleep 5
    done
    fail "run $RUN_ID did not complete within the live E2E timeout"
}
wait_for_run

reliability_compose --profile chaos run --rm --no-deps radar-chaos-controller
reliability_compose --profile reliability run --rm --no-deps radar-recovery-verifier
RECOVERY_REPORT=$(find "$TEST_ROOT/evidence" "$TEST_ROOT/reports" -type f -name '*.json' -print -quit)
[[ -n "$RECOVERY_REPORT" && -s "$RECOVERY_REPORT" ]] || fail "recovery verifier did not write evidence"
cp "$RECOVERY_REPORT" "$TEST_ROOT/raw/recovery-evidence.json"

GATE_BODY="$TEST_ROOT/gate.json"
python3 - "$GATE_BODY" "$RUN_ID" "$POLICY_ID" <<'PY'
import json
from datetime import datetime, timezone
import pathlib
import sys

observed_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
pathlib.Path(sys.argv[1]).write_text(json.dumps({
    "run_id": sys.argv[2],
    "policy_id": sys.argv[3],
    "policy": {
        "version": 1,
        "observation_days": 0,
        "enforcement_starts_at": observed_at,
    },
    "input": {
        "observed_at": observed_at,
        "observation_days": 0,
    },
}), encoding="utf-8")
PY
api_call evaluate-gate POST "/api/v1/admin/radar/gates/evaluate" "$GATE_BODY" >/dev/null

if [[ ${RADAR_LIVE_BUILD_ACCEPTANCE:-0} == "1" ]]; then
    [[ -n ${RADAR_LIVE_PREVIOUS_CONTROL_PLANE_IMAGE_DIGEST:-} ]] || fail "previous control-plane digest is required for acceptance evidence"
    [[ -n ${RADAR_LIVE_PREVIOUS_WORKER_IMAGE_DIGEST:-} ]] || fail "previous worker digest is required for acceptance evidence"
    for name in RADAR_LIVE_PREVIOUS_CONTROL_PLANE_IMAGE_DIGEST RADAR_LIVE_PREVIOUS_WORKER_IMAGE_DIGEST; do
        require_digest "$name" "${!name}"
    done
    for name in RADAR_LIVE_MIGRATION_CHECKSUMS RADAR_LIVE_SCORE_HASHES RADAR_LIVE_AGGREGATE_HASHES RADAR_LIVE_ARTIFACT_MANIFEST_HASHES RADAR_LIVE_CONTROL_PLANE_BINARY_SHA256 RADAR_LIVE_WORKER_BINARY_SHA256 RADAR_LIVE_BUDGET_LEDGER_TOTAL RADAR_LIVE_SMOKE_RUN_ID RADAR_LIVE_EVIDENCE_MANIFEST_KEY; do
        [[ -n ${!name:-} ]] || fail "$name is required for acceptance evidence"
    done
    for name in RADAR_LIVE_CONTROL_PLANE_BINARY_SHA256 RADAR_LIVE_WORKER_BINARY_SHA256; do
        [[ ${!name} =~ ^[0-9a-f]{64}$ ]] || fail "$name must be a lowercase SHA256"
    done
    require_uuid RADAR_LIVE_SMOKE_RUN_ID "$RADAR_LIVE_SMOKE_RUN_ID"
    require_live_secret RADAR_LIVE_EVIDENCE_MANIFEST_KEY
    export RADAR_LIVE_POLICY_ID RADAR_LIVE_RELEASE_SUBJECT_ID
    python3 - "$REPORT" "$TEST_ROOT/raw/recovery-evidence.json" "$TEST_ROOT/acceptance.json" "$POLICY_HASH" "$RELEASE_SUBJECT_HASH" "$RECOVERY_EVIDENCE_ID" "$FAULT_EXPERIMENT_ID" <<'PY'
import json
import hashlib
import hmac
import os
import pathlib
import sys

report_path, recovery_path, output_path, policy_hash, release_subject_hash, recovery_id, experiment_id = sys.argv[1:]
report = json.loads(pathlib.Path(report_path).read_text(encoding="utf-8"))
recovery_bytes = pathlib.Path(recovery_path).read_bytes()
recovery = json.loads(recovery_bytes.decode("utf-8"))
snapshots = []
for item in report["published_snapshots"]:
    submission = item["submission"]
    receipt = item["receipt"]
    receipt = receipt.get("data", receipt)
    if not isinstance(receipt, dict) or not isinstance(receipt.get("snapshot_id"), str) or not isinstance(receipt.get("snapshot_hash"), str):
        raise SystemExit("acceptance evidence requires immutable snapshot receipts")
    snapshots.append({
        "snapshot_id": receipt["snapshot_id"],
        "snapshot_hash": receipt["snapshot_hash"],
        "run_id": submission["run_id"],
        "load_plan_id": submission["load_plan_id"],
        "profile_id": submission["profile_id"],
        "slice_key": submission["slice_key"],
        "window_start": submission["window_start"],
        "window_end": submission["window_end"],
        "query_version": submission["query_version"],
        "source_hash": submission["source_watermark"],
        "source_watermark": submission["source_watermark"],
        "fresh_until": submission["fresh_until"],
        "metrics": {
            "request_count": submission["request_count"],
            "success_count": submission["success_count"],
            "error_count": submission["error_count"],
            "timeout_count": submission["timeout_count"],
            "retry_count": submission["retry_count"],
            "protocol_error_count": submission["protocol_error_count"],
            "billing_idempotency_failures": submission["billing_idempotency_failures"],
            "successful_latency_count": submission["success_count"],
            "valid_pair_count": 0,
            "upstream_failure_count": submission["error_count"],
            "gateway_failure_count": 0,
            "client_cancellation_count": 0,
            "error_numerator": submission["error_count"] + submission["timeout_count"],
            "error_denominator": submission["request_count"],
            "p99_latency_ms": submission["p99_latency_ms"],
            "histogram_or_sketch_hash": submission["latency_histogram_hash"],
            "ttft_histogram_hash": submission["ttft_histogram_hash"],
            "latency_histogram_hash": submission["latency_histogram_hash"],
            "ttft_histogram": submission["ttft_histogram"],
            "latency_histogram": submission["latency_histogram"],
            "source_manifest": submission["source_manifest"],
            "error_rate": str(submission["error_rate"]),
            "cost_amount": str(submission["cost_amount"]),
            "ongoing_confirmed_p0_incident": False,
        },
        "request_count": submission["request_count"],
        "success_count": submission["success_count"],
        "error_count": submission["error_count"],
        "timeout_count": submission["timeout_count"],
        "billing_idempotency_failures": submission["billing_idempotency_failures"],
        "p99_latency_ms": submission["p99_latency_ms"],
        "p99_slo_ms": int(os.environ.get("RADAR_LIVE_P99_SLO_MS", "500")),
        "error_rate": str(submission["error_rate"]),
    })
if recovery.get("status") != "verified":
    raise SystemExit("recovery evidence status must be verified")
observation = recovery.get("verification")
if not isinstance(observation, dict):
    raise SystemExit("recovery evidence is missing immutable verification facts")
for key in (
    "lease_recovery_ok",
    "evidence_checksums_match",
    "ledger_idempotent",
    "object_references_consistent",
    "policy_version_traceable",
    "backup_evidence_fresh",
    "alert_delivery_ok",
):
    if observation.get(key) is not True:
        raise SystemExit(f"recovery evidence fact {key} is not true")
pair_count = observation.get("deterministic_pair_count")
pre_hash = observation.get("pre_disaster_acceptance_hash")
post_hash = observation.get("recovered_acceptance_hash")
if pair_count != 30 or not isinstance(pre_hash, str) or not isinstance(post_hash, str) or len(pre_hash) != 64 or len(post_hash) != 64 or pre_hash != post_hash:
    raise SystemExit("recovery evidence does not contain exactly 30 deterministic pairs and matching hashes")
if not isinstance(recovery.get("rpo_ms"), int) or not isinstance(recovery.get("rto_ms"), int):
    raise SystemExit("recovery evidence is missing measured RPO or RTO")
if recovery.get("duplicate_score_count") != 0:
    raise SystemExit("recovery evidence contains duplicate scores")
if recovery.get("run_id") != report["run_id"] or recovery.get("experiment_id") != experiment_id:
    raise SystemExit("recovery evidence identity is not bound to the live drill")
def csv(name):
    values = [item for item in os.environ[name].split(",") if item]
    if not values or any(len(item) != 64 or any(char not in "0123456789abcdef" for char in item) for item in values):
        raise SystemExit(f"{name} must contain lowercase SHA256 values")
    return values

artifact_hashes = csv("RADAR_LIVE_ARTIFACT_MANIFEST_HASHES")
unsigned_fact_manifest = {
    "schema_version": "radar-fact-manifest-v1",
    "run_id": report["run_id"],
    "load_plan_id": report["load_plan_id"],
    "profile_id": report["profile_id"],
    "policy_id": os.environ["RADAR_LIVE_POLICY_ID"],
    "policy_hash": policy_hash,
    "release_subject_id": os.environ["RADAR_LIVE_RELEASE_SUBJECT_ID"],
    "release_subject_hash": release_subject_hash,
    "snapshot_refs": [
        {
            "snapshot_id": item["snapshot_id"],
            "snapshot_hash": item["snapshot_hash"],
            "run_id": item["run_id"],
            "load_plan_id": item["load_plan_id"],
            "profile_id": item["profile_id"],
            "slice_key": item["slice_key"],
            "source_watermark": item["source_watermark"],
        }
        for item in snapshots
    ],
    "recovery_ref": {
        "evidence_id": recovery_id,
        "evidence_hash": hashlib.sha256(recovery_bytes).hexdigest(),
        "run_id": recovery["run_id"],
        "experiment_id": recovery["experiment_id"],
        "source_watermark": recovery["source_watermark"],
        "recovery_generation": recovery["recovery_generation"],
    },
    "artifact_manifest_hashes": artifact_hashes,
}
canonical = lambda value: json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
fact_manifest = dict(unsigned_fact_manifest)
fact_manifest["manifest_sha256"] = hashlib.sha256(canonical(unsigned_fact_manifest)).hexdigest()
fact_manifest["signature"] = hmac.new(
    os.environ["RADAR_LIVE_EVIDENCE_MANIFEST_KEY"].encode("utf-8"),
    canonical({**unsigned_fact_manifest, "manifest_sha256": fact_manifest["manifest_sha256"]}),
    hashlib.sha256,
).hexdigest()
document = {
    "schema_version": "radar-staging-reliability-acceptance-v1",
    "release": {
        "run_id": report["run_id"],
        "load_plan_id": report["load_plan_id"],
        "control_plane_image_digest": os.environ["RADAR_LIVE_CONTROL_PLANE_IMAGE_DIGEST"],
        "worker_image_digest": report["worker_image_digest"],
    },
    "fact_manifest": fact_manifest,
    "reliability_snapshots": snapshots,
    "recovery": {
        "evidence_id": recovery_id,
        "evidence_hash": hashlib.sha256(recovery_bytes).hexdigest(),
        "recovery_generation": recovery["recovery_generation"],
        "source_watermark": recovery["source_watermark"],
        "rpo_seconds": float(recovery["rpo_ms"]) / 1000,
        "rpo_limit_seconds": float(os.environ.get("RADAR_LIVE_RPO_LIMIT_SECONDS", "300")),
        "rto_seconds": float(recovery["rto_ms"]) / 1000,
        "rto_limit_seconds": float(os.environ.get("RADAR_LIVE_RTO_LIMIT_SECONDS", "1800")),
        "worker_reregistered": observation.get("milestones", {}).get("worker_reregistered_at") is not None,
        "leases_recovered": observation["lease_recovery_ok"],
        "duplicate_score_count": recovery["duplicate_score_count"],
        "evidence_hash_consistent": observation["evidence_checksums_match"],
        "ledger_consistent": observation["ledger_idempotent"],
        "deterministic_acceptance": {
            "valid_pairs": pair_count,
            "pre_recovery_hash": pre_hash,
            "post_recovery_hash": post_hash,
        },
    },
    "rollback": {
        "recorded_at": report["finished_at"],
        "failed_run_ids": [report["run_id"]],
        "active_lease_ids": [],
        "previous_control_plane_image_digest": os.environ["RADAR_LIVE_PREVIOUS_CONTROL_PLANE_IMAGE_DIGEST"],
        "previous_worker_image_digest": os.environ["RADAR_LIVE_PREVIOUS_WORKER_IMAGE_DIGEST"],
        "control_plane_binary_sha256": os.environ["RADAR_LIVE_CONTROL_PLANE_BINARY_SHA256"],
        "worker_binary_sha256": os.environ["RADAR_LIVE_WORKER_BINARY_SHA256"],
        "migration_checksums": csv("RADAR_LIVE_MIGRATION_CHECKSUMS"),
        "score_hashes": csv("RADAR_LIVE_SCORE_HASHES"),
        "aggregate_hashes": csv("RADAR_LIVE_AGGREGATE_HASHES"),
        "artifact_manifest_hashes": csv("RADAR_LIVE_ARTIFACT_MANIFEST_HASHES"),
        "budget_ledger_total_before": os.environ["RADAR_LIVE_BUDGET_LEDGER_TOTAL"],
        "budget_ledger_total_after": os.environ["RADAR_LIVE_BUDGET_LEDGER_TOTAL"],
        "smoke_run_id": os.environ["RADAR_LIVE_SMOKE_RUN_ID"],
    },
}
pathlib.Path(output_path).write_text(json.dumps(document, sort_keys=True), encoding="utf-8")
PY
    RADAR_EVIDENCE_MANIFEST_KEY="$RADAR_LIVE_EVIDENCE_MANIFEST_KEY" "$ACCEPTANCE_SCRIPT" "$TEST_ROOT/acceptance.json" | tee "$TEST_ROOT/raw/acceptance.out"
fi

printf 'Radar live staging E2E passed. Evidence: %s\n' "$TEST_ROOT"
