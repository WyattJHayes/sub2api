#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

# Keep the previous filename as a compatibility entrypoint while v0.1.176 uses
# an accurately named command in release and isolation instructions.
exec bash "$ROOT_DIR/deploy/radar/rehearse-v01171-migrations.sh" "$@"
