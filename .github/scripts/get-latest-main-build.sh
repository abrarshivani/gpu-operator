#!/usr/bin/env bash
# Copyright NVIDIA CORPORATION
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

if [[ $# -ne 3 ]]; then
    echo "Usage: $0 MAIN_BUILD_REPOSITORY CHART_REPOSITORY MAX_AGE_HOURS" >&2
    exit 1
fi

MAIN_BUILD_REPOSITORY="$1"
CHART_REPOSITORY="$2"
MAX_AGE_HOURS="$3"

if ! command -v regctl &> /dev/null; then
    echo "Error: regctl not found. Please install regctl first." >&2
    exit 1
fi

if ! command -v jq &> /dev/null; then
    echo "Error: jq not found. Please install jq first." >&2
    exit 1
fi

MAIN_VERSION_PATTERN='^0\.0\.0-main\.([0-9]{14})\.g([0-9a-f]{8})$'
MAIN_BUILD_TAGS=$(regctl tag ls "${MAIN_BUILD_REPOSITORY}")
MAIN_VERSION=$(grep -E "${MAIN_VERSION_PATTERN}" <<< "${MAIN_BUILD_TAGS}" | sort | tail -n 1 || true)

if [[ -z "${MAIN_VERSION}" ]]; then
    echo "Error: ${MAIN_BUILD_REPOSITORY} has no main build" >&2
    exit 1
fi

[[ "${MAIN_VERSION}" =~ ${MAIN_VERSION_PATTERN} ]]
PUBLISHED_TIMESTAMP="${BASH_REMATCH[1]}"
OPERATOR_VERSION="${BASH_REMATCH[2]}"

OLDEST_ACCEPTED_TIMESTAMP=$(date -u -d "${MAX_AGE_HOURS} hours ago" +%Y%m%d%H%M%S)
if (( PUBLISHED_TIMESTAMP < OLDEST_ACCEPTED_TIMESTAMP )); then
    echo "Error: newest main build ${MAIN_VERSION} is older than ${MAX_AGE_HOURS} hours; the scheduled publish did not run or failed" >&2
    exit 1
fi

MAIN_BUILD_RECORD=$(regctl artifact get "${MAIN_BUILD_REPOSITORY}:${MAIN_VERSION}")
RECORDED_CHART_DIGEST=$(jq -r '.chart.digest' <<< "${MAIN_BUILD_RECORD}")
CURRENT_CHART_DIGEST=$(regctl image digest "${CHART_REPOSITORY}:${MAIN_VERSION}")
if [[ "${CURRENT_CHART_DIGEST}" != "${RECORDED_CHART_DIGEST}" ]]; then
    echo "Error: ${CHART_REPOSITORY}:${MAIN_VERSION} is ${CURRENT_CHART_DIGEST}, but main build ${MAIN_VERSION} recorded ${RECORDED_CHART_DIGEST}" >&2
    exit 1
fi

echo "chart_version=${MAIN_VERSION}"
echo "operator_version=${OPERATOR_VERSION}"
