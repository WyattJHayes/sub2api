#!/usr/bin/env bash
set -eu

MIN_AVAILABLE_KIB=1048576
MAX_SWAP_USED_KIB=131072
TEST_MODE=0
MEM_TOTAL_KIB=""
MEM_AVAILABLE_KIB=""
SWAP_USED_KIB=""

usage() {
	cat <<'EOF'
Read-only Sub2API runtime observation.

Usage:
  production_runtime_observe.sh [options]

Options:
  --help                         Show this help and exit.
  --test-mode                    Use injected values and skip Docker/journal reads.
  --mem-total-kib VALUE          Inject MemTotal for test mode.
  --mem-available-kib VALUE      Inject MemAvailable for test mode.
  --swap-used-kib VALUE          Inject used Swap for test mode.

The script never starts, stops, restarts, removes, reconfigures, or deploys a
service. A warning exits with status 1 so release automation cannot ignore it.
EOF
}

die() {
	printf 'error=%s\n' "$1" >&2
	exit 2
}

require_uint() {
	case "$2" in
		''|*[!0-9]*) die "$1 must be a non-negative integer" ;;
	esac
}

count_lines() {
	awk 'NF {count++} END {print count + 0}'
}

count_pattern() {
	awk -v pattern="$1" 'BEGIN {IGNORECASE = 1} $0 ~ pattern {count++} END {print count + 0}'
}

read_meminfo_kib() {
	[ -r /proc/meminfo ] || return 0
	awk -v key="$1" '$1 == key ":" {print $2; exit}' /proc/meminfo
}

read_swap_used_kib() {
	if command -v swapon >/dev/null 2>&1; then
		used_bytes=$(swapon --show=USED --bytes --noheadings --raw 2>/dev/null | awk '{sum += $1} END {print sum + 0}')
		printf '%s\n' "$((used_bytes / 1024))"
		return
	fi

	swap_total=$(read_meminfo_kib SwapTotal)
	swap_free=$(read_meminfo_kib SwapFree)
	if [ -z "$swap_total" ] || [ -z "$swap_free" ]; then
		printf '%s\n' 0
		return
	fi
	printf '%s\n' "$((swap_total - swap_free))"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--help)
			usage
			exit 0
			;;
		--test-mode)
			TEST_MODE=1
			shift
			;;
		--mem-total-kib|--mem-available-kib|--swap-used-kib)
			option="$1"
			shift
			[ "$#" -gt 0 ] || die "$option requires a value"
			case "$option" in
				--mem-total-kib) MEM_TOTAL_KIB="$1" ;;
				--mem-available-kib) MEM_AVAILABLE_KIB="$1" ;;
				--swap-used-kib) SWAP_USED_KIB="$1" ;;
			esac
			shift
			;;
		*)
			die "unknown option: $1"
			;;
	esac
done

if [ "$TEST_MODE" -eq 1 ]; then
	[ -n "$MEM_TOTAL_KIB" ] || die "--test-mode requires --mem-total-kib"
	[ -n "$MEM_AVAILABLE_KIB" ] || die "--test-mode requires --mem-available-kib"
	[ -n "$SWAP_USED_KIB" ] || die "--test-mode requires --swap-used-kib"
	MEM_SOURCE=fixture
else
	MEM_SOURCE=proc
	[ -n "$MEM_TOTAL_KIB" ] || MEM_TOTAL_KIB=$(read_meminfo_kib MemTotal)
	[ -n "$MEM_AVAILABLE_KIB" ] || MEM_AVAILABLE_KIB=$(read_meminfo_kib MemAvailable)
	[ -n "$SWAP_USED_KIB" ] || SWAP_USED_KIB=$(read_swap_used_kib)
fi

mem_source_warning=0
if [ -z "$MEM_TOTAL_KIB" ] || [ -z "$MEM_AVAILABLE_KIB" ]; then
	if [ "$TEST_MODE" -eq 1 ]; then
		die "test mode requires numeric memory values"
	fi
	MEM_SOURCE=unavailable
	MEM_TOTAL_KIB=${MEM_TOTAL_KIB:-0}
	MEM_AVAILABLE_KIB=${MEM_AVAILABLE_KIB:-0}
	mem_source_warning=1
fi

require_uint mem_total_kib "$MEM_TOTAL_KIB"
require_uint mem_available_kib "$MEM_AVAILABLE_KIB"
require_uint swap_used_kib "$SWAP_USED_KIB"

docker_source=skipped
container_count=0
unhealthy_count=0
stats_source=skipped
log_source=skipped
health_timeout_count=0
http_5xx_count=0
redis_auth_failed_count=0

if [ "$TEST_MODE" -eq 0 ]; then
	if command -v docker >/dev/null 2>&1 && docker ps -q >/dev/null 2>&1; then
		docker_source=available
		container_ids=$(docker ps -q 2>/dev/null || true)
		container_count=$(printf '%s\n' "$container_ids" | count_lines)
		unhealthy_count=$(docker ps --filter health=unhealthy -q 2>/dev/null | count_lines || true)
		if docker stats --no-stream >/dev/null 2>&1; then
			stats_source=available
		else
			stats_source=unavailable
		fi

		combined_logs=""
		for container_id in $container_ids; do
			container_logs=$(docker logs --since 30m "$container_id" 2>&1 || true)
			combined_logs="${combined_logs}
${container_logs}"
		done
		if command -v journalctl >/dev/null 2>&1; then
			journal_logs=$(journalctl -u docker --since '30 minutes ago' --no-pager 2>/dev/null || true)
			combined_logs="${combined_logs}
${journal_logs}"
		fi
		log_source=available
		health_timeout_count=$(printf '%s\n' "$combined_logs" | count_pattern 'health(check)?.*(timeout|timed out)|(timeout|timed out).*health(check)?')
		http_5xx_count=$(printf '%s\n' "$combined_logs" | count_pattern 'HTTP[^[:cntrl:]]*5[0-9][0-9]|status[=: ]+5[0-9][0-9]')
		redis_auth_failed_count=$(printf '%s\n' "$combined_logs" | count_pattern 'AUTH failed|NOAUTH|WRONGPASS')
	else
		docker_source=unavailable
		stats_source=unavailable
		log_source=unavailable
	fi
fi

warnings=()
if [ "$mem_source_warning" -eq 1 ]; then
	warnings+=(mem_source_unavailable)
fi
if [ "$MEM_AVAILABLE_KIB" -lt "$MIN_AVAILABLE_KIB" ]; then
	warnings+=(mem_available_below_threshold)
fi
if [ "$SWAP_USED_KIB" -gt "$MAX_SWAP_USED_KIB" ]; then
	warnings+=(swap_used_above_threshold)
fi
if [ "$docker_source" = unavailable ]; then
	warnings+=(docker_source_unavailable)
fi
if [ "$stats_source" = unavailable ]; then
	warnings+=(docker_stats_unavailable)
fi
if [ "$unhealthy_count" -gt 0 ]; then
	warnings+=(container_unhealthy)
fi
if [ "$health_timeout_count" -gt 0 ]; then
	warnings+=(docker_healthcheck_timeout)
fi
if [ "$http_5xx_count" -gt 0 ]; then
	warnings+=(http_5xx_observed)
fi
if [ "$redis_auth_failed_count" -gt 0 ]; then
	warnings+=(redis_auth_failed_observed)
fi

printf 'mem_source=%s\n' "$MEM_SOURCE"
printf 'mem_total_kib=%s\n' "$MEM_TOTAL_KIB"
printf 'mem_available_kib=%s\n' "$MEM_AVAILABLE_KIB"
printf 'swap_used_kib=%s\n' "$SWAP_USED_KIB"
printf 'docker_source=%s\n' "$docker_source"
printf 'container_count=%s\n' "$container_count"
printf 'unhealthy_count=%s\n' "$unhealthy_count"
printf 'docker_stats_source=%s\n' "$stats_source"
printf 'log_source=%s\n' "$log_source"
printf 'health_timeout_count=%s\n' "$health_timeout_count"
printf 'http_5xx_count=%s\n' "$http_5xx_count"
printf 'redis_auth_failed_count=%s\n' "$redis_auth_failed_count"

if [ "${#warnings[@]}" -gt 0 ]; then
	for warning in "${warnings[@]}"; do
		printf 'warning=%s\n' "$warning"
	done
	printf '%s\n' 'status=warning'
	exit 1
fi

printf '%s\n' 'status=ok'
