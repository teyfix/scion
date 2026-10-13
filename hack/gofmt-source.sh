#!/usr/bin/env bash
# Format source without touching isolated writers or project-owned runtime/cache trees.
set -euo pipefail
cd "$(dirname "$0")/.."
find . \( -path './.worktrees' -o -path './.cache' -o -path './.state' \) -prune -o \
  -type f -name '*.go' -print0 | xargs -0 gofmt "$@"
