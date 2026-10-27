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
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/NVIDIA/gpu-operator/internal/consts"
	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
)

const (
	operandStabilityPeriod         = 5 * time.Minute
	draResourceSliceCleanupTimeout = 5 * time.Minute

	computeDomainDriverName    = "compute-domain.nvidia.com"
	claimlessWorkloadContainer = "cuda-vector-add"
	passthroughFeatureGate     = "PassthroughSupport=true"

	// vectorAddBinaryPath is the entrypoint of vectorAddImage.
	vectorAddBinaryPath     = "/cuda-samples/sample"
	noGPUDeviceNodesMessage = "no GPU device nodes visible"
	// The CUDA error varies with what the container sees (no device, no driver libraries), so only
	// the prefix vectorAdd prints for its first device call is matched.
	vectorAddAllocationFailure = "Failed to allocate device vector A (error code"
)

// gpuOperatorCRDNames are the CRDs the chart ships in its crds directory, which helm never deletes.
var gpuOperatorCRDNames = []string{
	"clusterpolicies.nvidia.com",
	"gpuclusters.nvidia.com",
	"nvidiadrivers.nvidia.com",
	"computedomains.resource.nvidia.com",
	"computedomaincliques.resource.nvidia.com",
}

var passthroughHostMountPaths = []string{"/lib/modules/", "/sys/", "/proc/"}

var _ = ginkgo.Describe(e2eTestPrefix+"-gpucluster-install", ginkgo.Label("gpucluster", "single-gpu", "disruptive"), ginkgo.Ordered, ginkgo.Serial, func() {
	ginkgo.BeforeAll(func(ctx context.Context) {
		expectNoGPUStackCustomResources(ctx, newClusterClients())
	})
	install := newOperatorInstall(gpuClusterChartValues()...)

	ginkgo.It("brings up the GPUCluster and the DRA operands", func(ctx context.Context) {
		waitForGPUClusterReady(ctx, install.clients, operandReadyTimeout)
		waitForDRAOperandsReady(ctx, install.clients)
	})

	ginkgo.It("runs the DCGM exporter on its embedded hostengine", func(ctx context.Context) {
		clients := install.clients
		exporterDaemonSet, err := clients.daemonSets.GetByLabel(ctx, tcfg.namespace, "app", draDCGMExporterApp)
		framework.ExpectNoError(err, "failed to get daemonset %s", draDCGMExporterApp)
		for _, container := range exporterDaemonSet.Spec.Template.Spec.Containers {
			gomega.Expect(container.Env).NotTo(gomega.ContainElement(gomega.HaveField("Name", dcgmRemoteHostEngineEnvName)),
				"container %s points to a standalone hostengine", container.Name)
		}

		standaloneDCGMDaemonSets, err := clients.daemonSets.List(ctx, tcfg.namespace, appSelector(draDCGMApp))
		framework.ExpectNoError(err, "failed to list daemonsets %s", draDCGMApp)
		gomega.Expect(standaloneDCGMDaemonSets).To(gomega.BeEmpty(), "standalone DCGM is deployed although dcgm.enabled defaults to false")
	})

	ginkgo.It("does not deploy the device plugin or GFD", func(ctx context.Context) {
		expectDevicePluginAndGFDNotDeployed(ctx, install.clients)
	})

	ginkgo.It("creates the DRA DeviceClasses", func(ctx context.Context) {
		waitForDeviceClasses(ctx, install.clients, draDeviceClassNames...)
	})

	ginkgo.It("publishes the GPU in a ResourceSlice", func(ctx context.Context) {
		waitForGPUResourceSlices(ctx, install.clients)
	})

	ginkgo.It("runs vectorAdd with a ResourceClaim", func(ctx context.Context) {
		runGPUClaimWorkload(ctx, install.clients)
	})

	ginkgo.It("does not give a GPU to a pod without a ResourceClaim", func(ctx context.Context) {
		clients := install.clients
		claimlessPodName := gpuWorkloadName + "-no-claim"
		createWorkloadPod(ctx, clients, newClaimlessVectorAddPod(claimlessPodName))

		waitForPodPhase(ctx, clients, claimlessPodName, corev1.PodFailed, gpuWorkloadTimeout)
		podLogs := workloadPodLogs(ctx, clients, claimlessPodName, claimlessWorkloadContainer)
		gomega.Expect(podLogs).To(gomega.ContainSubstring(noGPUDeviceNodesMessage), "a GPU device node is visible without a ResourceClaim")
		gomega.Expect(podLogs).To(gomega.ContainSubstring(vectorAddAllocationFailure), "vectorAdd did not fail on its first CUDA call")

		deleteWorkloadPod(ctx, clients, claimlessPodName)
	})

	ginkgo.It("does not restart or replace the DRA operands", func(ctx context.Context) {
		clients := install.clients
		waitForDRAOperandsReady(ctx, clients)
		recordedRestartCounts, err := draOperandRestartCounts(ctx, clients)
		framework.ExpectNoError(err, "failed to record the DRA operand restart counts")
		gomega.Expect(recordedRestartCounts).NotTo(gomega.BeEmpty(), "no DRA operand containers found")

		ginkgo.By(fmt.Sprintf("checking for %s that no DRA operand container restarts and no pod is replaced", operandStabilityPeriod))
		gomega.Consistently(ctx, func(g gomega.Gomega) {
			currentRestartCounts, err := draOperandRestartCounts(ctx, clients)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(currentRestartCounts).To(gomega.Equal(recordedRestartCounts))
		}).WithTimeout(operandStabilityPeriod).WithPolling(pollInterval).Should(gomega.Succeed())
	})

	gpuClusterUninstallSpecs(install)
})

var _ = ginkgo.Describe(e2eTestPrefix+"-gpucluster-compute-domains", ginkgo.Label("gpucluster", "single-gpu"), ginkgo.Ordered, ginkgo.Serial, func() {
	ginkgo.BeforeAll(func(ctx context.Context) {
		expectNoGPUStackCustomResources(ctx, newClusterClients())
	})
	install := newOperatorInstall(gpuClusterChartValues("draDriver.computeDomains.enabled=true")...)

	ginkgo.It("brings up the GPUCluster", func(ctx context.Context) {
		waitForGPUClusterReady(ctx, install.clients, operandReadyTimeout)
	})

	ginkgo.It("runs the compute-domain controller", func(ctx context.Context) {
		waitForPodsReady(ctx, install.clients, appSelector(draControllerApp))
	})

	ginkgo.It("runs the compute-domains container in every kubelet plugin pod", func(ctx context.Context) {
		clients := install.clients
		waitFor(ctx, operandReadyTimeout, fmt.Sprintf("waiting for container %s to be ready in every kubelet plugin pod", draKubeletPluginComputeContainer), func(g gomega.Gomega) {
			kubeletPluginPods, err := clients.pods.List(ctx, tcfg.namespace, appSelector(draKubeletPluginApp))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(kubeletPluginPods).NotTo(gomega.BeEmpty(), "no kubelet plugin pods found")
			for _, pod := range kubeletPluginPods {
				g.Expect(pod.Status.ContainerStatuses).To(gomega.ContainElement(gomega.And(
					gomega.HaveField("Name", draKubeletPluginComputeContainer),
					gomega.HaveField("Ready", true),
				)), "pod %s", pod.Name)
			}
		})
	})

	ginkgo.It("creates the compute-domain DeviceClasses", func(ctx context.Context) {
		waitForDeviceClasses(ctx, install.clients, computeDomainDeviceClassNames...)
	})

	ginkgo.It("runs vectorAdd with a ResourceClaim", func(ctx context.Context) {
		waitForGPUResourceSlices(ctx, install.clients)
		runGPUClaimWorkload(ctx, install.clients)
	})

	gpuClusterUninstallSpecs(install)
})

var _ = ginkgo.Describe(e2eTestPrefix+"-gpucluster-passthrough", ginkgo.Label("gpucluster", "single-gpu"), ginkgo.Ordered, ginkgo.Serial, func() {
	ginkgo.BeforeAll(func(ctx context.Context) {
		expectNoGPUStackCustomResources(ctx, newClusterClients())
	})
	install := newOperatorInstall(gpuClusterChartValues("draDriver.featureGates.PassthroughSupport=true")...)

	ginkgo.It("brings up the GPUCluster", func(ctx context.Context) {
		waitForGPUClusterReady(ctx, install.clients, operandReadyTimeout)
	})

	ginkgo.It("runs the kubelet plugin with the PassthroughSupport feature gate", func(ctx context.Context) {
		clients := install.clients
		kubeletPluginDaemonSet, err := clients.daemonSets.GetByLabel(ctx, tcfg.namespace, "app", draKubeletPluginApp)
		framework.ExpectNoError(err, "failed to get daemonset %s", draKubeletPluginApp)

		var gpusContainer corev1.Container
		gomega.Expect(kubeletPluginDaemonSet.Spec.Template.Spec.Containers).To(
			gomega.ContainElement(gomega.HaveField("Name", draKubeletPluginGPUsContainer), &gpusContainer),
			"daemonset %s has no %s container", draKubeletPluginApp, draKubeletPluginGPUsContainer)

		var featureGatesEnv corev1.EnvVar
		gomega.Expect(gpusContainer.Env).To(gomega.ContainElement(gomega.HaveField("Name", featureGatesEnvName), &featureGatesEnv),
			"container %s has no %s env", draKubeletPluginGPUsContainer, featureGatesEnvName)
		gomega.Expect(strings.Split(featureGatesEnv.Value, ",")).To(gomega.ContainElement(passthroughFeatureGate))

		hostPathByVolumeName := map[string]string{}
		for _, volume := range kubeletPluginDaemonSet.Spec.Template.Spec.Volumes {
			if volume.HostPath != nil {
				hostPathByVolumeName[volume.Name] = volume.HostPath.Path
			}
		}
		hostPathByMountPath := map[string]string{}
		for _, volumeMount := range gpusContainer.VolumeMounts {
			if hostPath, isHostPathVolume := hostPathByVolumeName[volumeMount.Name]; isHostPathVolume {
				hostPathByMountPath[volumeMount.MountPath] = hostPath
			}
		}
		for _, passthroughPath := range passthroughHostMountPaths {
			gomega.Expect(hostPathByMountPath).To(gomega.HaveKeyWithValue(passthroughPath, passthroughPath),
				"container %s does not mount host path %s at %s", draKubeletPluginGPUsContainer, passthroughPath, passthroughPath)
		}

		waitForPodsReady(ctx, clients, appSelector(draKubeletPluginApp))
	})

	ginkgo.It("keeps the GPU bound to the nvidia driver", func(ctx context.Context) {
		clients := install.clients
		waitForGPUResourceSlices(ctx, clients)

		gpuNodes, err := clients.nodes.ListGPUNodes(ctx)
		framework.ExpectNoError(err, "failed to list the GPU nodes")
		for _, node := range gpuNodes {
			devices, err := gpuResourceSliceDevices(ctx, clients, node.Name)
			framework.ExpectNoError(err, "failed to read the ResourceSlices of node %s", node.Name)
			var deviceTypes []string
			for _, device := range devices {
				deviceTypes = append(deviceTypes, deviceStringAttribute(device, gpuDeviceTypeAttribute))
			}
			gomega.Expect(deviceTypes).To(gomega.ContainElement("gpu"), "node %s publishes no GPU bound to the nvidia driver", node.Name)
			gomega.Expect(deviceTypes).NotTo(gomega.ContainElement("vfio"), "node %s publishes a GPU bound to vfio-pci", node.Name)
		}
	})

	ginkgo.It("runs vectorAdd with a ResourceClaim", func(ctx context.Context) {
		runGPUClaimWorkload(ctx, install.clients)
	})

	gpuClusterUninstallSpecs(install)
})

// gpuClusterUninstallSpecs uninstalls the chart and checks that nothing the GPUCluster created is
// left behind. The container's DeferCleanup uninstalls again, which is a no-op by then.
func gpuClusterUninstallSpecs(install *operatorInstall) {
	ginkgo.It("deletes the GPUCluster when the chart is uninstalled", func(ctx context.Context) {
		clients := install.clients
		install.uninstall(ctx)
		waitForNotFound(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for GPUCluster/%s to be deleted", gpuClusterName), func(ctx context.Context) error {
			_, err := clients.gpuClusters.Get(ctx, gpuClusterName)
			return err
		})
	})

	ginkgo.It("removes the DeviceClasses and the ResourceSlice admission policy", func(ctx context.Context) {
		clients := install.clients
		for _, deviceClassName := range slices.Concat(draDeviceClassNames, computeDomainDeviceClassNames) {
			waitForNotFound(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for DeviceClass %s to be deleted", deviceClassName), func(ctx context.Context) error {
				_, err := clients.kube.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
				return err
			})
		}

		admissionRegistration := clients.kube.AdmissionregistrationV1()
		waitForNotFound(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for ValidatingAdmissionPolicy %s to be deleted", resourceSlicesPolicyName), func(ctx context.Context) error {
			_, err := admissionRegistration.ValidatingAdmissionPolicies().Get(ctx, resourceSlicesPolicyName, metav1.GetOptions{})
			return err
		})
		waitForNotFound(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for ValidatingAdmissionPolicyBinding %s to be deleted", resourceSlicesPolicyName), func(ctx context.Context) error {
			_, err := admissionRegistration.ValidatingAdmissionPolicyBindings().Get(ctx, resourceSlicesPolicyName, metav1.GetOptions{})
			return err
		})
	})

	ginkgo.It("deletes the operator namespace without pods stuck terminating", func(ctx context.Context) {
		err := framework.WaitForNamespacesDeleted(ctx, install.clients.kube, []string{tcfg.namespace}, framework.DefaultNamespaceDeletionTimeout)
		framework.ExpectNoError(err, "namespace %s was not deleted", tcfg.namespace)
	})

	ginkgo.It("removes the ResourceSlices of the DRA driver", func(ctx context.Context) {
		clients := install.clients
		draDriverNames := []string{gpuDriverName, computeDomainDriverName}
		// The kubelet deletes the ResourceSlices of a driver some time after its plugin unregisters.
		waitFor(ctx, draResourceSliceCleanupTimeout, fmt.Sprintf("waiting for the ResourceSlices of %v to be deleted", draDriverNames), func(g gomega.Gomega) {
			resourceSlices, err := clients.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			for _, resourceSlice := range resourceSlices.Items {
				g.Expect(draDriverNames).NotTo(gomega.ContainElement(resourceSlice.Spec.Driver), "ResourceSlice %s is left", resourceSlice.Name)
			}
		})
	})

	ginkgo.It("keeps the GPU Operator CRDs", func(ctx context.Context) {
		crds := install.clients.apiExtensions.ApiextensionsV1().CustomResourceDefinitions()
		for _, crdName := range gpuOperatorCRDNames {
			crd, err := crds.Get(ctx, crdName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get CRD %s", crdName)
			gomega.Expect(crd.DeletionTimestamp).To(gomega.BeNil(), "CRD %s is being deleted", crdName)
		}
	})
}

// newClaimlessVectorAddPod returns a pod on a GPU node, without a ResourceClaim, that lists the
// /dev/nvidia<N> GPU device nodes it sees and then runs vectorAdd.
func newClaimlessVectorAddPod(name string) *corev1.Pod {
	listGPUDeviceNodesThenRunVectorAdd := fmt.Sprintf(`ls /dev/nvidia[0-9]* 2>/dev/null || echo %q; exec %s`,
		noGPUDeviceNodesMessage, vectorAddBinaryPath)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gpuWorkloadNamespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  map[string]string{consts.GPUPresentLabel: "true"},
			Containers: []corev1.Container{{
				Name:    claimlessWorkloadContainer,
				Image:   vectorAddImage,
				Command: []string{"sh", "-c", listGPUDeviceNodesThenRunVectorAdd},
			}},
		},
	}
}

// draOperandContainer identifies a container by its pod UID as well as names, so a replaced pod
// shows up as a different container even if it reuses the name.
type draOperandContainer struct {
	podName       string
	podUID        types.UID
	containerName string
}

// draOperandRestartCounts returns the restart count of every container of every DRA operand pod.
func draOperandRestartCounts(ctx context.Context, clients clusterClients) (map[draOperandContainer]int32, error) {
	restartCountByContainer := map[draOperandContainer]int32{}
	for _, selector := range draOperandSelectors {
		pods, err := clients.pods.List(ctx, tcfg.namespace, selector)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			for _, containerStatus := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
				container := draOperandContainer{podName: pod.Name, podUID: pod.UID, containerName: containerStatus.Name}
				restartCountByContainer[container] = containerStatus.RestartCount
			}
		}
	}
	return restartCountByContainer, nil
}
