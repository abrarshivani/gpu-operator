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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/conditions"
	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	"github.com/NVIDIA/gpu-operator/tests/e2e/helpers"
)

const (
	gpuClusterReconcileTimeout = 2 * time.Minute

	gpuClusterName = "gpu-cluster"

	gpuDriverName                       = "gpu.nvidia.com"
	gpuDeviceClassName                  = "gpu.nvidia.com"
	migDeviceClassName                  = "mig.nvidia.com"
	vfioGPUDeviceClassName              = "vfio.gpu.nvidia.com"
	computeDomainDaemonDeviceClassName  = "compute-domain-daemon.nvidia.com"
	computeDomainChannelDeviceClassName = "compute-domain-default-channel.nvidia.com"
	gpuDeviceTypeAttribute              = "type"
	gpuDeviceUUIDAttribute              = "uuid"
	gpuClaimRequestName                 = "gpu"
	draKubeletPluginApp                 = "nvidia-dra-driver-kubelet-plugin"
	draKubeletPluginGPUsContainer       = "gpus"
	draKubeletPluginComputeContainer    = "compute-domains"
	draControllerApp                    = "nvidia-dra-driver-controller"
	draControllerContainer              = "compute-domain"
	draDCGMApp                          = "nvidia-dcgm-dra"
	draDCGMExporterApp                  = "nvidia-dcgm-exporter-dra"
	draDCGMExporterContainer            = "nvidia-dcgm-exporter-dra"
	dcgmRemoteHostEngineEnvName         = "DCGM_REMOTE_HOSTENGINE_INFO"
	featureGatesEnvName                 = "FEATURE_GATES"
	draValidatorSelector                = "app.kubernetes.io/component=nvidia-dra-validator"
	devicePluginApp                     = "nvidia-device-plugin-daemonset"
	gpuFeatureDiscoveryApp              = "gpu-feature-discovery"
	resourceSlicesPolicyName            = "resourceslices-policy-nvidia-dra-driver"
	longRunningCUDAWorkloadImage        = "nvcr.io/nvidia/k8s/cuda-sample:nbody-cuda11.7.1-ubuntu18.04"
	longRunningCUDAWorkloadContainer    = "cuda-nbody"
	claimWorkloadContainer              = "cuda-vector-add"
	// The cuda-sample images (vectorAddImage, longRunningCUDAWorkloadImage) ship their sample as this
	// entrypoint, which is not on PATH, so a command that replaces the entrypoint must call it here.
	cudaSampleBinaryPath = "/cuda-samples/sample"
)

// gpuClusterChartValues returns the --set values that install a GPUCluster and the default NVIDIADriver
// instead of a ClusterPolicy, followed by extraChartValues.
func gpuClusterChartValues(extraChartValues ...string) []string {
	return append([]string{
		"clusterPolicy.deployCR=false",
		"gpuCluster.deployCR=true",
		"driver.nvidiaDriverCRD.enabled=true",
		"driver.nvidiaDriverCRD.deployDefaultCR=true",
	}, extraChartValues...)
}

// draOperandSelectors match the pods of the operands a default GPUCluster install runs. The
// compute-domain controller runs because the chart enables compute domains by default.
var draOperandSelectors = []string{
	helpers.DriverComponentSelector,
	appSelector(draKubeletPluginApp),
	appSelector(draControllerApp),
	appSelector(draDCGMExporterApp),
	draValidatorSelector,
}

// draDeviceClassNames are the DeviceClasses every GPUCluster creates, whether or not compute domains are enabled.
var draDeviceClassNames = []string{gpuDeviceClassName, migDeviceClassName, vfioGPUDeviceClassName}

var computeDomainDeviceClassNames = []string{computeDomainDaemonDeviceClassName, computeDomainChannelDeviceClassName}

// expectNoGPUStackCustomResources checks the precondition of the install specs. A missing CRD
// counts as no custom resources, because the first install of the run creates the CRDs.
func expectNoGPUStackCustomResources(ctx context.Context, clients clusterClients) {
	ginkgo.GinkgoHelper()
	clusterPolicies, err := clients.clusterPolicies.List(ctx)
	if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		framework.ExpectNoError(err, "failed to list ClusterPolicies")
		gomega.Expect(clusterPolicies.Items).To(gomega.BeEmpty(), "a ClusterPolicy already exists")
	}
	gpuClusters, err := clients.gpuClusters.List(ctx)
	if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		framework.ExpectNoError(err, "failed to list GPUClusters")
		gomega.Expect(gpuClusters.Items).To(gomega.BeEmpty(), "a GPUCluster already exists")
	}
}

// waitForGPUClusterReady waits for the GPUCluster to report ready with a true Ready condition and no true Error condition.
func waitForGPUClusterReady(ctx context.Context, clients clusterClients, timeout time.Duration) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, timeout, fmt.Sprintf("waiting for GPUCluster/%s to be ready", gpuClusterName), func(g gomega.Gomega) {
		gpuCluster, err := clients.gpuClusters.Get(ctx, gpuClusterName)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(gpuCluster.Status.State).To(gomega.Equal(nvidiav1alpha1.Ready))
		g.Expect(meta.IsStatusConditionTrue(gpuCluster.Status.Conditions, conditions.Ready)).To(gomega.BeTrue(), "Ready condition is not true")
		g.Expect(meta.IsStatusConditionTrue(gpuCluster.Status.Conditions, conditions.Error)).To(gomega.BeFalse(), "Error condition is true")
	})
}

func waitForDRAOperandsReady(ctx context.Context, clients clusterClients) {
	ginkgo.GinkgoHelper()
	for _, selector := range draOperandSelectors {
		waitForPodsReady(ctx, clients, selector)
	}
}

// expectDevicePluginAndGFDNotDeployed checks that nothing of the device plugin stack runs next to the GPUCluster.
func expectDevicePluginAndGFDNotDeployed(ctx context.Context, clients clusterClients) {
	ginkgo.GinkgoHelper()
	for _, app := range []string{devicePluginApp, gpuFeatureDiscoveryApp} {
		daemonSets, err := clients.daemonSets.List(ctx, tcfg.namespace, appSelector(app))
		framework.ExpectNoError(err, "failed to list daemonsets %s", app)
		gomega.Expect(daemonSets).To(gomega.BeEmpty(), "daemonset %s is deployed", app)
		pods, err := clients.pods.List(ctx, tcfg.namespace, appSelector(app))
		framework.ExpectNoError(err, "failed to list pods %s", app)
		gomega.Expect(pods).To(gomega.BeEmpty(), "pods of %s are running", app)
	}
}

func waitForDeviceClasses(ctx context.Context, clients clusterClients, deviceClassNames ...string) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for DeviceClasses %v", deviceClassNames), func(g gomega.Gomega) {
		for _, deviceClassName := range deviceClassNames {
			_, err := clients.kube.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "DeviceClass %s", deviceClassName)
		}
	})
}

// gpuResourceSliceDevices returns the devices that the DRA driver publishes for nodeName.
func gpuResourceSliceDevices(ctx context.Context, clients clusterClients, nodeName string) ([]resourcev1.Device, error) {
	resourceSlices, err := clients.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list ResourceSlices: %w", err)
	}
	var devices []resourcev1.Device
	for _, resourceSlice := range resourceSlices.Items {
		if resourceSlice.Spec.Driver == gpuDriverName && resourceSlice.Spec.NodeName != nil && *resourceSlice.Spec.NodeName == nodeName {
			devices = append(devices, resourceSlice.Spec.Devices...)
		}
	}
	return devices, nil
}

func deviceStringAttribute(device resourcev1.Device, attributeName resourcev1.QualifiedName) string {
	attribute, found := device.Attributes[attributeName]
	if !found || attribute.StringValue == nil {
		return ""
	}
	return *attribute.StringValue
}

// waitForGPUResourceSlices waits until the DRA driver publishes at least one full GPU for every GPU node.
func waitForGPUResourceSlices(ctx context.Context, clients clusterClients) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, operandReadyTimeout, "waiting for the DRA driver to publish the GPUs of every GPU node", func(g gomega.Gomega) {
		gpuNodes, err := clients.nodes.ListGPUNodes(ctx)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(gpuNodes).NotTo(gomega.BeEmpty(), "no GPU nodes found")
		for _, node := range gpuNodes {
			devices, err := gpuResourceSliceDevices(ctx, clients, node.Name)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(devices).To(gomega.ContainElement(gomega.Satisfy(func(device resourcev1.Device) bool {
				return deviceStringAttribute(device, gpuDeviceTypeAttribute) == "gpu"
			})), "node %s publishes no GPU", node.Name)
		}
	})
}

func newGPUResourceClaim(name string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gpuWorkloadNamespace},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{
					Name: gpuClaimRequestName,
					Exactly: &resourcev1.ExactDeviceRequest{
						DeviceClassName: gpuDeviceClassName,
						AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
						Count:           1,
					},
				}},
			},
		},
	}
}

// createGPUResourceClaim creates a ResourceClaim for one GPU and registers its deletion.
func createGPUResourceClaim(ctx context.Context, clients clusterClients, name string) {
	ginkgo.GinkgoHelper()
	claims := clients.kube.ResourceV1().ResourceClaims(gpuWorkloadNamespace)
	_, err := claims.Create(ctx, newGPUResourceClaim(name), metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create ResourceClaim %s/%s", gpuWorkloadNamespace, name)
	ginkgo.DeferCleanup(deleteResourceClaim, clients, name)
}

func deleteResourceClaim(ctx context.Context, clients clusterClients, name string) {
	ginkgo.GinkgoHelper()
	claims := clients.kube.ResourceV1().ResourceClaims(gpuWorkloadNamespace)
	err := claims.Delete(ctx, name, metav1.DeleteOptions{})
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete ResourceClaim %s/%s", gpuWorkloadNamespace, name)
	waitForNotFound(ctx, podDeletionTimeout, fmt.Sprintf("waiting for ResourceClaim %s/%s to be deleted", gpuWorkloadNamespace, name), func(ctx context.Context) error {
		_, err := claims.Get(ctx, name, metav1.GetOptions{})
		return err
	})
}

// newClaimPod returns a pod whose only container gets the GPU of ResourceClaim claimName. A nil
// command keeps the image entrypoint.
func newClaimPod(name, claimName, containerName, image string, command []string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gpuWorkloadNamespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:              gpuClaimRequestName,
				ResourceClaimName: new(claimName),
			}},
			Containers: []corev1.Container{{
				Name:    containerName,
				Image:   image,
				Command: command,
				Resources: corev1.ResourceRequirements{
					Claims: []corev1.ResourceClaim{{Name: gpuClaimRequestName}},
				},
			}},
		},
	}
}

// createWorkloadPod creates pod and registers its deletion.
func createWorkloadPod(ctx context.Context, clients clusterClients, pod *corev1.Pod) {
	ginkgo.GinkgoHelper()
	_, err := clients.kube.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create pod %s/%s", pod.Namespace, pod.Name)
	ginkgo.DeferCleanup(deleteWorkloadPod, clients, pod.Name)
}

func deleteWorkloadPod(ctx context.Context, clients clusterClients, name string) {
	ginkgo.GinkgoHelper()
	err := clients.kube.CoreV1().Pods(gpuWorkloadNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete pod %s/%s", gpuWorkloadNamespace, name)
	waitForPodNotFound(ctx, clients, gpuWorkloadNamespace, name, podDeletionTimeout)
}

// waitForPodPhase waits until the pod reaches expectedPhase, failing early if it reaches another terminal phase.
func waitForPodPhase(ctx context.Context, clients clusterClients, name string, expectedPhase corev1.PodPhase, timeout time.Duration) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, timeout, fmt.Sprintf("waiting for pod %s/%s to reach phase %s", gpuWorkloadNamespace, name, expectedPhase), func(g gomega.Gomega) {
		pod, err := clients.kube.CoreV1().Pods(gpuWorkloadNamespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		if expectedPhase != pod.Status.Phase && (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) {
			gomega.StopTrying(fmt.Sprintf("pod %s/%s ended in phase %s", gpuWorkloadNamespace, name, pod.Status.Phase)).Now()
		}
		g.Expect(pod.Status.Phase).To(gomega.Equal(expectedPhase))
	})
}

func workloadPodLogs(ctx context.Context, clients clusterClients, name, containerName string) string {
	ginkgo.GinkgoHelper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gpuWorkloadNamespace}}
	podLogs, err := clients.pods.Logs(ctx, pod, &corev1.PodLogOptions{Container: containerName})
	framework.ExpectNoError(err, "failed to read the logs of pod %s/%s", gpuWorkloadNamespace, name)
	return podLogs
}

// expectClaimAllocatedByGPUDriver checks that ResourceClaim claimName is allocated a device of the DRA driver.
func expectClaimAllocatedByGPUDriver(ctx context.Context, clients clusterClients, claimName string) {
	ginkgo.GinkgoHelper()
	claim, err := clients.kube.ResourceV1().ResourceClaims(gpuWorkloadNamespace).Get(ctx, claimName, metav1.GetOptions{})
	framework.ExpectNoError(err, "failed to get ResourceClaim %s/%s", gpuWorkloadNamespace, claimName)
	gomega.Expect(claim.Status.Allocation).NotTo(gomega.BeNil(), "ResourceClaim %s/%s is not allocated", gpuWorkloadNamespace, claimName)
	gomega.Expect(claim.Status.Allocation.Devices.Results).To(gomega.ConsistOf(gomega.HaveField("Driver", gpuDriverName)),
		"ResourceClaim %s/%s is not allocated one device of %s", gpuWorkloadNamespace, claimName, gpuDriverName)
}

// runClaimPod runs command in a pod with a ResourceClaim for one GPU, checks that the claim was
// allocated and the pod succeeded, deletes the pod and the claim, and returns the pod logs. A nil
// command runs vectorAdd.
func runClaimPod(ctx context.Context, clients clusterClients, podName string, command []string) string {
	ginkgo.GinkgoHelper()
	claimName := podName + "-gpu"
	createGPUResourceClaim(ctx, clients, claimName)
	createWorkloadPod(ctx, clients, newClaimPod(podName, claimName, claimWorkloadContainer, vectorAddImage, command))

	waitForPodPhase(ctx, clients, podName, corev1.PodSucceeded, gpuWorkloadTimeout)
	expectClaimAllocatedByGPUDriver(ctx, clients, claimName)
	podLogs := workloadPodLogs(ctx, clients, podName, claimWorkloadContainer)

	deleteWorkloadPod(ctx, clients, podName)
	deleteResourceClaim(ctx, clients, claimName)
	return podLogs
}

// runGPUClaimWorkload runs vectorAdd with a ResourceClaim for one GPU and checks that it passes.
func runGPUClaimWorkload(ctx context.Context, clients clusterClients) {
	ginkgo.GinkgoHelper()
	workloadLogs := runClaimPod(ctx, clients, gpuWorkloadName+"-claim", nil)
	gomega.Expect(workloadLogs).To(gomega.ContainSubstring("Test PASSED"), "vectorAdd did not report success")
}

// longRunningCUDACommand prints the UUID of the claimed GPU, then keeps it busy with nbody for duration.
func longRunningCUDACommand(duration time.Duration) []string {
	return []string{"bash", "-c", fmt.Sprintf(
		`nvidia-smi -L && deadline=$((SECONDS + %d)) && while (( SECONDS < deadline )); do %s -benchmark -numbodies=65536 > /dev/null || exit 1; done`,
		int(duration.Seconds()), cudaSampleBinaryPath)}
}

// newLongRunningCUDAPod returns a pod that keeps the GPU of ResourceClaim claimName busy for duration.
func newLongRunningCUDAPod(name, claimName string, duration time.Duration) *corev1.Pod {
	return newClaimPod(name, claimName, longRunningCUDAWorkloadContainer, longRunningCUDAWorkloadImage, longRunningCUDACommand(duration))
}
