#!/bin/bash

set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
module_directory="$(cd "${script_directory}/.." && pwd -P)"
"${script_directory}/build.sh"
source "${module_directory}/.build/build-env.sh"
go_cache="${cache_directory}/go-build"
module_cache="${cache_directory}/go-mod"

(
  cd "${source_root}"
  GOWORK="${workspace_file}" GOCACHE="${go_cache}" GOMODCACHE="${module_cache}" \
    "${go_binary}" test ./ios ./ios/tunnel ./ios/installationproxy ./ios/zipconduit ./ios/dtx_codec ./ios/instruments ./ios/testmanagerd ./ios/appservice ./ios/openstdio ./ios/nskeyedarchiver
)
(
  cd "${module_directory}"
  GOWORK="${workspace_file}" GOCACHE="${go_cache}" GOMODCACHE="${module_cache}" \
    "${go_binary}" test ./...
  GOWORK="${workspace_file}" GOCACHE="${go_cache}" GOMODCACHE="${module_cache}" \
    "${go_binary}" test -race ./...
  GOWORK="${workspace_file}" GOCACHE="${go_cache}" GOMODCACHE="${module_cache}" \
    "${go_binary}" vet ./...
)

"${candidate_binary}" version
printf 'Lyo Nodus iOS OTA tests passed\n'
