#! /bin/bash
# This test case runs the GPUCluster (DRA) specs of the Ginkgo e2e suite on a single-GPU node.

SCRIPTS_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )"/../scripts && pwd )"

"${SCRIPTS_DIR}"/run-ginkgo.sh gpucluster
