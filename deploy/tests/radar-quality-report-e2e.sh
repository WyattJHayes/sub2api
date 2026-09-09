#!/usr/bin/env bash

set -euo pipefail

STAGING_URL=${RADAR_QUALITY_STAGING_URL:-}
FIXTURE_MANIFEST=${RADAR_QUALITY_FIXTURE_MANIFEST:-}
MAX_ATTEMPTS=${RADAR_QUALITY_STAGING_MAX_ATTEMPTS:-60}
POLL_SECONDS=${RADAR_QUALITY_STAGING_POLL_SECONDS:-5}

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

require_value() {
    local name=$1
    [[ -n ${!name:-} ]] || fail "$name is required"
}

[[ ${RADAR_QUALITY_STAGING_E2E:-} == "1" ]] || \
    fail "set RADAR_QUALITY_STAGING_E2E=1 to run the isolated staging check"
require_value RADAR_QUALITY_FIXTURE_MANIFEST
[[ -f "$FIXTURE_MANIFEST" ]] || fail "fixture manifest must be a regular file"

# Permissions and schema are checked before target validation or any network tool runs.
ROUTE_SNAPSHOT_PATH=$(python3 - "$FIXTURE_MANIFEST" <<'PY'
import json
import os
import re
import stat
import sys

path = sys.argv[1]
if stat.S_IMODE(os.stat(path).st_mode) != 0o600:
    raise SystemExit("fixture manifest must have mode 0600")
try:
    with open(path, encoding="utf-8") as stream:
        document = json.load(stream)
except (OSError, json.JSONDecodeError) as error:
    raise SystemExit("fixture manifest must contain valid JSON") from error
if not isinstance(document, dict):
    raise SystemExit("fixture manifest must be a JSON object")
forbidden_fragments = (
    "credential", "password", "token", "apikey", "prompt", "completion", "trace",
    "account", "channel", "rawartifact", "probespechash", "observation",
)
def contains_forbidden_key(value):
    if isinstance(value, dict):
        for key, nested in value.items():
            normalized = re.sub(r"[^a-z0-9]+", "", str(key).casefold())
            if any(fragment in normalized for fragment in forbidden_fragments):
                return True
            if contains_forbidden_key(nested):
                return True
    elif isinstance(value, list):
        return any(contains_forbidden_key(nested) for nested in value)
    return False
if contains_forbidden_key(document):
    raise SystemExit("fixture manifest contains forbidden evidence fields")
expected_top_level = {
    "schema_version", "run_identifier", "fixture_user_email",
    "setup_administrator_email", "route_snapshot_path", "scenarios",
}
if set(document) != expected_top_level:
    raise SystemExit("fixture manifest must use the exact top-level schema")
if document.get("schema_version") != "radar-local-quality-fixture-v1":
    raise SystemExit("fixture manifest has an unsupported schema version")
for field in ("run_identifier", "fixture_user_email", "setup_administrator_email"):
    if not isinstance(document.get(field), str) or not document[field]:
        raise SystemExit(f"fixture manifest is missing {field}")
route_snapshot_path = document.get("route_snapshot_path")
if not isinstance(route_snapshot_path, str) or re.fullmatch(
    r"/api/v1/admin/groups/[1-9][0-9]*/composite-routes", route_snapshot_path
) is None:
    raise SystemExit("fixture manifest route snapshot path is invalid")
expected = {
    "healthy": {
        "alias": "radar-quality-healthy",
        "overall_conclusion": "no_significant_anomaly",
        "adulteration_risk": "no_significant_anomaly",
        "degradation_risk": "no_significant_anomaly",
        "source_state": "inferred",
    },
    "watered": {
        "alias": "radar-quality-watered",
        "overall_conclusion": "high_risk",
        "adulteration_risk": "high_risk",
        "degradation_risk": "no_significant_anomaly",
        "source_state": "inferred",
    },
    "degraded": {
        "alias": "radar-quality-degraded",
        "overall_conclusion": "high_risk",
        "adulteration_risk": "no_significant_anomaly",
        "degradation_risk": "high_risk",
        "source_state": "inferred",
    },
    "insufficient": {
        "alias": "radar-quality-insufficient",
        "overall_conclusion": "insufficient_coverage",
        "adulteration_risk": "insufficient_coverage",
        "degradation_risk": "insufficient_coverage",
        "source_state": "insufficient_evidence",
    },
}
scenarios = document.get("scenarios")
if not isinstance(scenarios, dict) or set(scenarios) != set(expected):
    raise SystemExit("fixture manifest must contain four exact scenarios")
for name, conclusion in expected.items():
    entry = scenarios.get(name)
    if not isinstance(entry, dict) or set(entry) != {"model_alias", "run_id", "expected"}:
        raise SystemExit(f"fixture manifest scenario {name} is malformed")
    if entry.get("model_alias") != conclusion["alias"]:
        raise SystemExit(f"fixture manifest scenario {name} has an invalid alias")
    if not isinstance(entry.get("run_id"), str) or not entry["run_id"]:
        raise SystemExit(f"fixture manifest scenario {name} has an invalid run ID")
    wanted = {key: value for key, value in conclusion.items() if key != "alias"}
    if entry.get("expected") != wanted:
        raise SystemExit(f"fixture manifest scenario {name} has invalid expected conclusions")
print(route_snapshot_path)
PY
)

require_value RADAR_QUALITY_STAGING_URL
[[ "$MAX_ATTEMPTS" =~ ^[1-9][0-9]*$ ]] || fail "RADAR_QUALITY_STAGING_MAX_ATTEMPTS must be a positive integer"
[[ "$POLL_SECONDS" =~ ^[1-9][0-9]*$ ]] || fail "RADAR_QUALITY_STAGING_POLL_SECONDS must be a positive integer"

python3 - "$STAGING_URL" <<'PY'
from sys import argv
from urllib.parse import urlparse

url = urlparse(argv[1])
if url.scheme != "http" or url.hostname not in {"127.0.0.1", "localhost", "::1"}:
    raise SystemExit("RADAR_QUALITY_STAGING_URL must use an HTTP loopback origin")
if url.username or url.password or url.port is None:
    raise SystemExit("RADAR_QUALITY_STAGING_URL must include only a host and explicit port")
if url.path not in {"", "/"} or url.params or url.query or url.fragment:
    raise SystemExit("RADAR_QUALITY_STAGING_URL must be an origin without a path, query, or fragment")
PY

if [[ ${RADAR_QUALITY_STAGING_DRY_RUN:-0} == "1" ]]; then
    printf 'Radar quality report staging dry-run passed.\n'
    exit 0
fi

require_value RADAR_QUALITY_STAGING_ADMIN_TOKEN
require_value RADAR_QUALITY_STAGING_USER_TOKEN
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v python3 >/dev/null 2>&1 || fail "python3 is required"

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/radar-quality-report-e2e.XXXXXX")
cleanup() {
    rm -rf -- "$WORK_DIR"
}
trap cleanup EXIT

api_get() {
    local token=$1
    local path=$2
    local output=$3
    local status
    status=$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
        -H "Authorization: Bearer $token" -H 'Accept: application/json' \
        -o "$output" -w '%{http_code}' "$STAGING_URL$path") || fail "request failed"
    [[ "$status" == "200" ]] || fail "API request returned HTTP $status"
}

canonical_json() {
    python3 - "$1" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    document = json.load(stream)
print(json.dumps(document, ensure_ascii=True, sort_keys=True, separators=(",", ":")))
PY
}

route_before="$WORK_DIR/routes-before.json"
route_after="$WORK_DIR/routes-after.json"
health="$WORK_DIR/health.json"
scenarios=(healthy watered degraded insufficient)
aliases=(radar-quality-healthy radar-quality-watered radar-quality-degraded radar-quality-insufficient)

# This verifier only reads public Radar evidence and route snapshots.
api_get "$RADAR_QUALITY_STAGING_ADMIN_TOKEN" "$ROUTE_SNAPSHOT_PATH" "$route_before"

reports_ready=0
for ((attempt = 1; attempt <= MAX_ATTEMPTS; attempt++)); do
    reports_ready=1
    for index in "${!scenarios[@]}"; do
        detail="$WORK_DIR/detail-${scenarios[$index]}.json"
        status=$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
            -H "Authorization: Bearer $RADAR_QUALITY_STAGING_USER_TOKEN" -H 'Accept: application/json' \
            -o "$detail" -w '%{http_code}' \
            "$STAGING_URL/api/v1/radar/models/${aliases[$index]}/quality-report") || status=000
        if [[ "$status" != "200" ]]; then
            reports_ready=0
        fi
    done
    [[ "$reports_ready" == "1" ]] && break
    (( attempt == MAX_ATTEMPTS )) && fail "quality reports were not published for all fixture scenarios"
    sleep "$POLL_SECONDS"
done

api_get "$RADAR_QUALITY_STAGING_USER_TOKEN" "/api/v1/radar/health" "$health"
api_get "$RADAR_QUALITY_STAGING_ADMIN_TOKEN" "$ROUTE_SNAPSHOT_PATH" "$route_after"

[[ "$(canonical_json "$route_before")" == "$(canonical_json "$route_after")" ]] || \
    fail "gateway route configuration changed during quality verification"

python3 - "$FIXTURE_MANIFEST" "$health" \
    "$WORK_DIR/detail-healthy.json" "$WORK_DIR/detail-watered.json" \
    "$WORK_DIR/detail-degraded.json" "$WORK_DIR/detail-insufficient.json" <<'PY'
import json
import re
import sys

manifest_path, health_path, *detail_paths = sys.argv[1:]
scenario_names = ("healthy", "watered", "degraded", "insufficient")
required_dimensions = {
    "knowledge_freshness", "model_fingerprint", "reasoning_stability", "structure_compliance",
    "parameter_fidelity", "instruction_hierarchy", "protocol_schema", "stream_completeness",
}
forbidden_fragments = (
    "credential", "password", "token", "apikey", "prompt", "completion", "trace",
    "account", "channel", "rawartifact", "probespechash", "observation",
)
def contains_forbidden_key(value):
    if isinstance(value, dict):
        for key, nested in value.items():
            normalized = re.sub(r"[^a-z0-9]+", "", str(key).casefold())
            if any(fragment in normalized for fragment in forbidden_fragments):
                return True
            if contains_forbidden_key(nested):
                return True
    elif isinstance(value, list):
        return any(contains_forbidden_key(nested) for nested in value)
    return False
with open(manifest_path, encoding="utf-8") as stream:
    manifest = json.load(stream)
with open(health_path, encoding="utf-8") as stream:
    health_document = json.load(stream)
health = health_document.get("data")
if not isinstance(health, list):
    raise SystemExit("health response is missing a data list")
health_aliases = {item.get("model_alias") for item in health if isinstance(item, dict)}
expected_aliases = {entry["model_alias"] for entry in manifest["scenarios"].values()}
if not expected_aliases.issubset(health_aliases):
    raise SystemExit("health list does not expose all fixture aliases")
for scenario, path in zip(scenario_names, detail_paths, strict=True):
    with open(path, encoding="utf-8") as stream:
        document = json.load(stream)
    detail = document.get("data")
    if not isinstance(detail, dict):
        raise SystemExit("quality report is missing an object data field")
    if contains_forbidden_key(detail):
        raise SystemExit("quality report exposed a forbidden field")
    entry = manifest["scenarios"][scenario]
    expected = entry["expected"]
    if detail.get("model_alias") != entry["model_alias"]:
        raise SystemExit("quality report alias does not match fixture manifest")
    for field in ("overall_conclusion", "adulteration_risk", "degradation_risk"):
        if detail.get(field) != expected[field]:
            raise SystemExit("quality report does not match fixture manifest")
    source = detail.get("source_attribution")
    if not isinstance(source, dict) or source.get("state") != expected["source_state"]:
        raise SystemExit("quality report does not match fixture manifest")
    dimensions = detail.get("dimension_results")
    if not isinstance(dimensions, list) or {item.get("key") for item in dimensions if isinstance(item, dict)} != required_dimensions:
        raise SystemExit("quality report must expose exactly eight dimensions")
PY

printf 'Radar quality report staging E2E passed for four fixture scenarios.\n'
