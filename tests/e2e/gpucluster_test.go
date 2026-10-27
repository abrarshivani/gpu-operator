/**
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
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
**/

package e2e

import (
	"context"

	"github.com/onsi/ginkgo/v2"
)

// The specs whose only precondition is a ready GPUCluster share one install. Each group of specs
// leaves the GPUCluster ready for the next one.
var _ = ginkgo.Describe(e2eTestPrefix+"-gpucluster", ginkgo.Label("gpucluster", "single-gpu"), ginkgo.Ordered, ginkgo.Serial, func() {
	install := newOperatorInstall(gpuClusterChartValues()...)

	ginkgo.It("brings up the DRA operands", func(ctx context.Context) {
		waitForGPUClusterReady(ctx, install.clients, operandReadyTimeout)
		waitForDRAOperandsReady(ctx, install.clients)
		waitForGPUResourceSlices(ctx, install.clients)
	})

	sharedResourceClaimSpecs(install)
	standaloneDCGMSpecs(install)
	gpuClusterChangeSpecs(install)
	singletonAndClusterPolicyConflictSpecs(install)
	nodeRebootSpecs(install)
	nvidiaDriverWithGPUClusterSpecs(install)
})
