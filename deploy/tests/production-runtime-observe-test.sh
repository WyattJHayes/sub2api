#!/usr/bin/env bash
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="$SCRIPT_DIR/../production_runtime_observe.sh"

healthy_output=$(bash "$SCRIPT" --test-mode --mem-total-kib 2097152 --mem-available-kib 1572864 --swap-used-kib 65536)
printf '%s\n' "$healthy_output" | grep -q '^status=ok$'

set +e
warning_output=$(bash "$SCRIPT" --test-mode --mem-total-kib 2097152 --mem-available-kib 524288 --swap-used-kib 716800 2>&1)
warning_status=$?
set -e

test "$warning_status" -eq 1
printf '%s\n' "$warning_output" | grep -q '^status=warning$'
printf '%s\n' "$warning_output" | grep -q 'warning=mem_available_below_threshold'
if printf '%s\n' "$warning_output" | grep -Eiq 'password|token|cookie|redis://|postgres://'; then
	printf '%s\n' 'observation output contains sensitive-looking material' >&2
	exit 1
fi
