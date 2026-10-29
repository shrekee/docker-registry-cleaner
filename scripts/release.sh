#!/usr/bin/env bash
# Run from the trusted automation checkout, not from the historical tag.
set -euo pipefail
exec python3 "$(dirname "$0")/release.py" "$@"
