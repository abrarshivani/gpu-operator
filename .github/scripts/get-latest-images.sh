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

readonly COMPONENT_SOURCES='{
    "toolkit": {
        "imageRepository": "ghcr.io/nvidia/container-toolkit",
        "githubRepository": "NVIDIA/nvidia-container-toolkit",
        "githubBranch": "main"
    },
    "device-plugin": {
        "imageRepository": "ghcr.io/nvidia/k8s-device-plugin",
        "githubRepository": "NVIDIA/k8s-device-plugin",
        "githubBranch": "main"
    },
    "mig-manager": {
        "imageRepository": "ghcr.io/nvidia/k8s-mig-manager",
        "githubRepository": "NVIDIA/mig-parted",
        "githubBranch": "main"
    },
    "driver-manager": {
        "imageRepository": "ghcr.io/nvidia/k8s-driver-manager",
        "githubRepository": "NVIDIA/k8s-driver-manager",
        "githubBranch": "main"
    },
    "vgpu-device-manager": {
        "imageRepository": "ghcr.io/nvidia/vgpu-device-manager",
        "githubRepository": "NVIDIA/vgpu-device-manager",
        "githubBranch": "main"
    },
    "kata-sandbox-device-plugin": {
        "imageRepository": "ghcr.io/nvidia/nvidia-sandbox-device-plugin",
        "githubRepository": "NVIDIA/sandbox-device-plugin",
        "githubBranch": "main"
    },
    "kubevirt-gpu-device-plugin": {
        "imageRepository": "ghcr.io/nvidia/kubevirt-gpu-device-plugin",
        "githubRepository": "NVIDIA/kubevirt-gpu-device-plugin",
        "githubBranch": "master"
    },
    "cc-manager": {
        "imageRepository": "ghcr.io/nvidia/k8s-cc-manager",
        "githubRepository": "NVIDIA/k8s-cc-manager",
        "githubBranch": "main",
        "tagSuffix": "-distroless"
    }
}'
VALID_COMPONENTS=$(jq -r 'keys_unsorted | join(" ")' <<< "${COMPONENT_SOURCES}")
readonly VALID_COMPONENTS
readonly MAX_COMMITS_TO_CHECK=30
readonly OUTDATED_IMAGE_AGE_HOURS=24

usage() {
    echo "Usage: $0 [--outdated-images-file FILE] <component> [<component>...]" >&2
    echo "Valid components: ${VALID_COMPONENTS}" >&2
    exit 1
}

outdated_images_file=""
if [[ "${1:-}" == "--outdated-images-file" ]]; then
    [[ $# -ge 2 ]] || usage
    outdated_images_file=$2
    shift 2
fi

if [[ $# -eq 0 ]]; then
    usage
fi

if ! command -v regctl &> /dev/null; then
    echo "Error: regctl not found. Please install regctl first." >&2
    exit 1
fi

get_latest_available_image() {
    local component=$1
    local fallback_note_file=$2
    local outdated_image_file=$3

    local component_source
    if ! component_source=$(jq -e --arg component "${component}" '.[$component]' <<< "${COMPONENT_SOURCES}"); then
        echo "Error: Unknown component '${component}'" >&2
        echo "Valid components: ${VALID_COMPONENTS}" >&2
        return 1
    fi

    local image_repo github_repo github_branch tag_suffix
    image_repo=$(jq -er '.imageRepository' <<< "${component_source}")
    github_repo=$(jq -er '.githubRepository' <<< "${component_source}")
    github_branch=$(jq -er '.githubBranch' <<< "${component_source}")
    tag_suffix=$(jq -r '.tagSuffix // ""' <<< "${component_source}")

    echo "Fetching the last ${MAX_COMMITS_TO_CHECK} commits from ${github_repo}@${github_branch}..." >&2

    # NOTE: We use 8-char truncated SHAs as image tags. This must match the
    # tag convention used by each component's CI pipeline when publishing images.
    local github_api_url="https://api.github.com/repos/${github_repo}/commits?sha=${github_branch}&per_page=${MAX_COMMITS_TO_CHECK}"

    local curl_options=(-sSL -H "Accept: application/vnd.github.v3+json")
    if [[ -n "${GITHUB_TOKEN:-}" ]]; then
        curl_options+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
    fi

    local recent_commits
    if ! recent_commits=$(curl "${curl_options[@]}" "${github_api_url}" | \
        jq -er '.[] | "\(.sha[0:8]) \(.commit.committer.date)"'); then
        echo "Error: Failed to fetch recent commits from ${github_repo}" >&2
        return 1
    fi

    local commits_without_image=()
    local commit commit_date
    while read -r commit commit_date; do
        local full_image="${image_repo}:${commit}${tag_suffix}"
        if ! regctl manifest head "${full_image}" &> /dev/null; then
            echo "No image for commit ${commit} (${commit_date}): ${full_image}" >&2
            commits_without_image+=("${commit}")
            continue
        fi

        echo "Using ${component} image: ${full_image}" >&2
        if [[ ${#commits_without_image[@]} -gt 0 ]]; then
            local fallback_note="${full_image} from ${commit_date}; the ${#commits_without_image[@]} newer commit(s) ${commits_without_image[*]} have no image"
            echo "${fallback_note}" > "${fallback_note_file}"
            local commit_age_seconds=$(( $(date -u +%s) - $(date -u -d "${commit_date}" +%s) ))
            if (( commit_age_seconds > OUTDATED_IMAGE_AGE_HOURS * 3600 )); then
                echo "${fallback_note}" > "${outdated_image_file}"
            fi
        fi
        echo "${full_image}"
        return 0
    done <<< "${recent_commits}"

    echo "Error: None of the last ${MAX_COMMITS_TO_CHECK} commits of ${github_repo}@${github_branch} has an image in ${image_repo}" >&2
    return 1
}

components=("$@")
image_lookup_dir=$(mktemp -d)
trap 'rm -rf "${image_lookup_dir}"' EXIT

lookup_pids=()
for index in "${!components[@]}"; do
    component=${components[index]}
    get_latest_available_image "${component}" \
        "${image_lookup_dir}/${index}.fallback" \
        "${image_lookup_dir}/${index}.outdated" \
        > "${image_lookup_dir}/${index}.image" \
        2> >(sed -u "s/^/[${component}] /" >&2) &
    lookup_pids+=($!)
done

failed_components=()
for index in "${!components[@]}"; do
    if ! wait "${lookup_pids[index]}"; then
        failed_components+=("${components[index]}")
    fi
done

if [[ ${#failed_components[@]} -gt 0 ]]; then
    echo "::error::Image lookup failed for ${failed_components[*]}" >&2
    exit 1
fi

if [[ -n "${outdated_images_file}" ]]; then
    : > "${outdated_images_file}"
fi

for index in "${!components[@]}"; do
    component=${components[index]}
    if [[ -f "${image_lookup_dir}/${index}.fallback" ]]; then
        echo "::warning::${component} uses $(< "${image_lookup_dir}/${index}.fallback")" >&2
    fi
    if [[ -n "${outdated_images_file}" && -f "${image_lookup_dir}/${index}.outdated" ]]; then
        echo "${component}: $(< "${image_lookup_dir}/${index}.outdated")" >> "${outdated_images_file}"
    fi
    echo "${component}=$(< "${image_lookup_dir}/${index}.image")"
done
