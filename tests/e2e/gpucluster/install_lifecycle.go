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

package gpucluster

import (
	"context"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	e2eoperands "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operands"
	e2eoperator "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operator"
	e2eworkload "github.com/NVIDIA/gpu-operator/tests/e2e/framework/workload"
)

const (
	failureDiagnosticsTimeout = time.Minute

	migDeviceClassName     = "mig.nvidia.com"
	vfioGPUDeviceClassName = "vfio.gpu.nvidia.com"
)

var draDeviceClassNames = []string{
	e2eoperands.GPUDeviceClassName,
	migDeviceClassName,
	vfioGPUDeviceClassName,
}

var gpuOperatorCRDNames = []string{
	"clusterpolicies.nvidia.com",
	"gpuclusters.nvidia.com",
	"nvidiadrivers.nvidia.com",
}

var _ = e2eoperator.Describe("gpucluster", "gpucluster-install", ginkgo.Label("single-gpu", "disruptive"), ginkgo.Ordered, ginkgo.Serial, func() {
	ginkgo.BeforeAll(func(ctx context.Context) {
		ginkgo.By("Checking that the cluster is ready for a fresh GPUCluster install")
		clients := e2eoperator.NewClients()
		expectNoClusterPolicyOrGPUCluster(ctx, clients)
		ginkgo.DeferOnFailure(logDRAFailureDiagnostics, clients, ginkgo.NodeTimeout(failureDiagnosticsTimeout))
	})
	install := e2eoperator.NewInstall(
		"clusterPolicy.deployCR=false",
		"gpuCluster.deployCR=true",
		"driver.nvidiaDriverCRD.enabled=true",
		"driver.nvidiaDriverCRD.deployDefaultCR=true",
	)
	var operandRestartCountsAtBringUp map[operandContainer]int32

	ginkgo.It("brings up the GPUCluster and the operands", func(ctx context.Context) {
		e2eoperator.WaitForGPUClusterReady(ctx, install.Clients)
		e2eoperands.WaitForOperandsReady(ctx, install.Clients, operandSelectors)

		ginkgo.By("Recording the restart counts of the operand containers")
		restartCounts, err := operandRestartCounts(ctx, install.Clients)
		framework.ExpectNoError(err, "failed to record the operand restart counts")
		gomega.Expect(restartCounts).NotTo(gomega.BeEmpty(), "found no operand containers to record")
		ginkgo.GinkgoWriter.Printf("Recorded %d containers for the later restart check\n", len(restartCounts))
		operandRestartCountsAtBringUp = restartCounts
	})

	ginkgo.It("does not deploy the device plugin or GFD", func(ctx context.Context) {
		ginkgo.By("Checking that neither the device plugin nor GFD is deployed")
		for _, app := range []string{"nvidia-device-plugin-daemonset", "gpu-feature-discovery"} {
			daemonSets, err := install.DaemonSets.List(ctx, e2eoperator.TestContext.OperatorNamespace, e2eoperands.AppSelector(app))
			framework.ExpectNoError(err, "failed to list %s daemonsets", app)
			gomega.Expect(daemonSetNames(daemonSets)).To(gomega.BeEmpty(),
				"%s daemonsets are deployed, but a GPUCluster install should not deploy them", app)

			pods, err := install.Pods.List(ctx, e2eoperator.TestContext.OperatorNamespace, e2eoperands.AppSelector(app))
			framework.ExpectNoError(err, "failed to list %s pods", app)
			gomega.Expect(podStatusSummaries(pods)).To(gomega.BeEmpty(),
				"%s pods exist, but a GPUCluster install should not run them", app)
		}
		ginkgo.GinkgoWriter.Printf("Found no device plugin or GFD daemonsets or pods\n")
	})

	ginkgo.It("creates the DRA DeviceClasses", func(ctx context.Context) {
		for _, deviceClassName := range draDeviceClassNames {
			e2eoperands.WaitForDeviceClass(ctx, install.Clients, deviceClassName)
		}
	})

	ginkgo.It("publishes the GPU in a ResourceSlice", func(ctx context.Context) {
		e2eoperands.WaitForGPUResourceSlices(ctx, install.Clients)
	})

	ginkgo.It("runs vectorAdd with a ResourceClaim", func(ctx context.Context) {
		e2eworkload.RunGPUClaimWorkload(ctx, install.Clients)
	})

	ginkgo.It("does not give a GPU to a pod without a ResourceClaim", func(ctx context.Context) {
		e2eworkload.RunVectorAddWithoutResourceClaim(ctx, install.Clients)
	})

	ginkgo.It("does not restart or replace the operands", func(ctx context.Context) {
		gomega.Expect(operandRestartCountsAtBringUp).NotTo(gomega.BeEmpty(), "the bring-up spec did not record the operand restart counts")

		expectOperandsStable(ctx, install.Clients, operandRestartCountsAtBringUp)
		e2eoperands.WaitForOperandsReady(ctx, install.Clients, operandSelectors)
	})

	ginkgo.It("deletes the GPUCluster when the chart is uninstalled", func(ctx context.Context) {
		install.Uninstall(ctx)
		e2eoperator.WaitForGPUClusterDeleted(ctx, install.Clients)
	})

	ginkgo.It("removes the DRA DeviceClasses", func(ctx context.Context) {
		for _, deviceClassName := range draDeviceClassNames {
			e2eoperands.WaitForDeviceClassDeleted(ctx, install.Clients, deviceClassName)
		}
	})

	ginkgo.It("deletes the operator namespace", func(ctx context.Context) {
		waitForOperatorNamespaceDeleted(ctx, install.Clients)
	})

	ginkgo.It("removes the ResourceSlices of the DRA driver", func(ctx context.Context) {
		e2eoperands.WaitForGPUResourceSlicesDeleted(ctx, install.Clients)
	})

	ginkgo.It("keeps the GPU Operator CRDs", func(ctx context.Context) {
		ginkgo.By("Checking the GPU Operator CRDs after the uninstall")
		crdClient := install.APIExtensions.ApiextensionsV1().CustomResourceDefinitions()
		for _, crdName := range gpuOperatorCRDNames {
			crd, err := crdClient.Get(ctx, crdName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get CRD %s", crdName)
			gomega.Expect(crd.DeletionTimestamp).To(gomega.BeNil(), "CRD %s is being deleted", crdName)
		}
		ginkgo.GinkgoWriter.Printf("All %d GPU Operator CRDs are still installed\n", len(gpuOperatorCRDNames))
	})
})
