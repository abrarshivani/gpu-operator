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
    echo "Usage: $0 CHART_DIRECTORY VALUES_OVERRIDES_FILE" >&2
    exit 1
fi

CHART_DIRECTORY="$1"
VALUES_OVERRIDES_FILE="$2"
CHART_VALUES_FILE="${CHART_DIRECTORY}/values.yaml"

OVERRIDE_PATH_LIST=$(yq '.. | select(tag == "!!map" and has("image")) | path | join(".")' "${VALUES_OVERRIDES_FILE}")
if [[ -z "${OVERRIDE_PATH_LIST}" ]]; then
    echo "Error: ${VALUES_OVERRIDES_FILE} has no image overrides" >&2
    exit 1
fi
mapfile -t OVERRIDE_PATHS <<< "${OVERRIDE_PATH_LIST}"

MISSING_PATH_COUNT=0
for OVERRIDE_PATH in "${OVERRIDE_PATHS[@]}"; do
    if ! VALUES_PATH=".${OVERRIDE_PATH}" yq -e 'eval(strenv(VALUES_PATH)) | has("image")' "${CHART_VALUES_FILE}" &> /dev/null; then
        echo "Error: ${OVERRIDE_PATH}.image is not a value in ${CHART_VALUES_FILE}" >&2
        MISSING_PATH_COUNT=$((MISSING_PATH_COUNT + 1))
    fi
done
if [[ ${MISSING_PATH_COUNT} -gt 0 ]]; then
    exit 1
fi

VALUES_OVERRIDES_FILE="${VALUES_OVERRIDES_FILE}" yq -i '. *= load(strenv(VALUES_OVERRIDES_FILE))' "${CHART_VALUES_FILE}"

RENDERED_CLUSTER_POLICY_SPEC=$(helm template gpu-operator "${CHART_DIRECTORY}" | yq 'select(.kind == "ClusterPolicy") | .spec')

IMAGE_REFERENCE_EXPRESSION='eval(strenv(VALUES_PATH)) | (.repository // "") + "/" + (.image // "") + ":" + (.version // "")'
MISMATCHED_PATH_COUNT=0
for OVERRIDE_PATH in "${OVERRIDE_PATHS[@]}"; do
    EXPECTED_IMAGE_REFERENCE=$(VALUES_PATH=".${OVERRIDE_PATH}" yq "${IMAGE_REFERENCE_EXPRESSION}" "${VALUES_OVERRIDES_FILE}")
    RENDERED_IMAGE_REFERENCE=$(VALUES_PATH=".${OVERRIDE_PATH}" yq "${IMAGE_REFERENCE_EXPRESSION}" <<< "${RENDERED_CLUSTER_POLICY_SPEC}")
    if [[ "${RENDERED_IMAGE_REFERENCE}" != "${EXPECTED_IMAGE_REFERENCE}" ]]; then
        echo "Error: ${OVERRIDE_PATH} renders as '${RENDERED_IMAGE_REFERENCE}' in the ClusterPolicy, expected '${EXPECTED_IMAGE_REFERENCE}'" >&2
        MISMATCHED_PATH_COUNT=$((MISMATCHED_PATH_COUNT + 1))
    fi
done
if [[ ${MISMATCHED_PATH_COUNT} -gt 0 ]]; then
    exit 1
fi

echo "Applied ${#OVERRIDE_PATHS[@]} image overrides to ${CHART_VALUES_FILE}"
