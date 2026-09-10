#!/usr/bin/env bash
# Focused command-generation checks; never starts Docker or builds an image.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/../builders/local-docker.sh"

docker() { printf '<%s>' "$@"; printf '\n'; }
unset SCION_GHA_CACHE_SCOPE_PREFIX NPM_CONFIG_FILE PIP_CONFIG_FILE

build_command() {
  builder_build --image-name "$1" --context-dir /tmp/context \
    --dockerfile /tmp/context/Dockerfile --tags "example/$1:test" \
    --platforms linux/amd64 --push true
}

command="$(build_command core-base)"
[[ "${command}" != *'<--cache-from>'* && "${command}" != *'<--cache-to>'* ]]

export SCION_GHA_CACHE_SCOPE_PREFIX=onprem
for image in core-base scion-base; do
  command="$(build_command "${image}")"
  [[ "${command}" == *"<--cache-from><type=gha,version=2,scope=onprem-${image}>"* ]]
  [[ "${command}" == *"<--cache-to><type=gha,version=2,scope=onprem-${image},mode=max>"* ]]
  [[ "${command}" == *'<--platform><linux/amd64>'* ]]
  [[ "${command}" == *'<--push>'* ]]
done

# Exercise the entire graph in dry-run mode, including parent dependencies.
output="$(bash "${script_dir}/../build-images.sh" --builder local-docker \
  --registry ghcr.io/teyfix --target all --tag build-cache-test \
  --platform linux/amd64 --push --dry-run)"
[[ "${output}" == *'scope=onprem-core-base'* ]]
[[ "${output}" == *'scope=onprem-scion-base'* ]]
[[ "${output}" == *'BASE_IMAGE=ghcr.io/teyfix/core-base:'* ]]
[[ "${output}" == *'BASE_IMAGE=ghcr.io/teyfix/scion-base:'* ]]
printf 'PASS: optional per-image cache, amd64 push, and full graph dry run\n'
