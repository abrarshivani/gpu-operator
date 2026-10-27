#! /bin/bash
# This test case runs the GPUCluster (DRA) specs of the Ginkgo e2e suite on a single-GPU node.

SCRIPTS_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )"/../scripts && pwd )"

# Each GPUCluster container installs the chart and the driver again, which outlasts the default timeout.
GINKGO_TIMEOUT="${GINKGO_TIMEOUT:-5h}" "${SCRIPTS_DIR}"/run-ginkgo.sh gpucluster
