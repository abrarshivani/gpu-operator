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

if [[ $# -ne 2 ]]; then
    echo "Usage: $0 CHART_REPOSITORY MAX_AGE_HOURS" >&2
    exit 1
fi

CHART_REPOSITORY="$1"
MAX_AGE_HOURS="$2"

if ! command -v regctl &> /dev/null; then
    echo "Error: regctl not found. Please install regctl first." >&2
    exit 1
fi

MAIN_CHART_VERSION_PATTERN='^0\.0\.0-main\.([0-9]{14})\.g([0-9a-f]{8})$'
CHART_TAGS=$(regctl tag ls "${CHART_REPOSITORY}")
CHART_VERSION=$(grep -E "${MAIN_CHART_VERSION_PATTERN}" <<< "${CHART_TAGS}" | sort | tail -n 1 || true)

if [[ -z "${CHART_VERSION}" ]]; then
    echo "Error: ${CHART_REPOSITORY} has no main chart" >&2
    exit 1
fi

[[ "${CHART_VERSION}" =~ ${MAIN_CHART_VERSION_PATTERN} ]]
PUBLISHED_TIMESTAMP="${BASH_REMATCH[1]}"
OPERATOR_VERSION="${BASH_REMATCH[2]}"

OLDEST_ACCEPTED_TIMESTAMP=$(date -u -d "${MAX_AGE_HOURS} hours ago" +%Y%m%d%H%M%S)
if (( PUBLISHED_TIMESTAMP < OLDEST_ACCEPTED_TIMESTAMP )); then
    echo "Error: newest main chart ${CHART_VERSION} is older than ${MAX_AGE_HOURS} hours; the scheduled publish did not run or failed" >&2
    exit 1
fi

echo "chart_version=${CHART_VERSION}"
echo "operator_version=${OPERATOR_VERSION}"
