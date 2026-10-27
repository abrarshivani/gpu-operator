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
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	"github.com/NVIDIA/gpu-operator/tests/e2e/helpers"
)

const (
	sharedGPUClaimName    = gpuWorkloadName + "-shared-gpu"
	firstSharingPodName   = gpuWorkloadName + "-sharing-first"
	secondSharingPodName  = gpuWorkloadName + "-sharing-second"
	waitingPodName        = gpuWorkloadName + "-waiting"
	waitingPodClaimName   = waitingPodName + "-gpu"
	sharingPodRunDuration = 15 * time.Minute
	claimStabilityPeriod  = time.Minute
	claimPodReportTimeout = 2 * time.Minute

	dcgmWorkloadPodName       = gpuWorkloadName + "-dcgm"
	dcgmWorkloadRunDuration   = 3 * time.Minute
	dcgmExporterScrapeTimeout = 2 * time.Minute
	standaloneDCGMHostEngine  = "nvidia-dcgm-dra:5555"
	standaloneDCGMPort        = 5555
	dcgmExporterMetricsPort   = "9400"
	gpuUtilizationMetricName  = "DCGM_FI_DEV_GPU_UTIL"
	maxGPUUtilizationPercent  = 100
)

var gpuUUIDPattern = regexp.MustCompile(`UUID: (GPU-[0-9a-fA-F-]+)`)

func sharedResourceClaimSpecs(install *operatorInstall) {
	ginkgo.Context("with a ResourceClaim shared by two pods", func() {
		ginkgo.BeforeAll(func() {
			ginkgo.DeferCleanup(func(ctx context.Context) {
				for _, podName := range []string{firstSharingPodName, secondSharingPodName, waitingPodName} {
					deleteWorkloadPod(ctx, install.clients, podName)
				}
				for _, claimName := range []string{sharedGPUClaimName, waitingPodClaimName} {
					deleteResourceClaim(ctx, install.clients, claimName)
				}
			})
		})

		ginkgo.It("runs both sharing pods on the same GPU", func(ctx context.Context) {
			clients := install.clients
			createSharedClaimTestClaim(ctx, clients, sharedGPUClaimName)
			for _, podName := range []string{firstSharingPodName, secondSharingPodName} {
				createSharedClaimTestPod(ctx, clients, newLongRunningCUDAPod(podName, sharedGPUClaimName, sharingPodRunDuration))
			}
			for _, podName := range []string{firstSharingPodName, secondSharingPodName} {
				waitForPodPhase(ctx, clients, podName, corev1.PodRunning, gpuWorkloadTimeout)
			}

			expectClaimAllocatedByGPUDriver(ctx, clients, sharedGPUClaimName)
			sharedClaim, err := clients.kube.ResourceV1().ResourceClaims(gpuWorkloadNamespace).Get(ctx, sharedGPUClaimName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get ResourceClaim %s/%s", gpuWorkloadNamespace, sharedGPUClaimName)
			gomega.Expect(sharedClaim.Status.ReservedFor).To(gomega.ConsistOf(
				gomega.HaveField("Name", firstSharingPodName),
				gomega.HaveField("Name", secondSharingPodName),
			), "ResourceClaim %s/%s is not reserved for both sharing pods", gpuWorkloadNamespace, sharedGPUClaimName)

			firstPodGPUUUID := waitForPodGPUUUID(ctx, clients, firstSharingPodName)
			secondPodGPUUUID := waitForPodGPUUUID(ctx, clients, secondSharingPodName)
			gomega.Expect(secondPodGPUUUID).To(gomega.Equal(firstPodGPUUUID), "the sharing pods see different GPUs")

			firstSharingPod, err := clients.kube.CoreV1().Pods(gpuWorkloadNamespace).Get(ctx, firstSharingPodName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get pod %s/%s", gpuWorkloadNamespace, firstSharingPodName)
			gomega.Expect(allocatedGPUUUID(ctx, clients, sharedClaim, firstSharingPod.Spec.NodeName)).To(gomega.Equal(firstPodGPUUUID),
				"the sharing pods do not see the GPU allocated to ResourceClaim %s/%s", gpuWorkloadNamespace, sharedGPUClaimName)
		})

		ginkgo.It("keeps a pod with its own ResourceClaim pending while the shared claim holds the GPU", func(ctx context.Context) {
			clients := install.clients
			createSharedClaimTestClaim(ctx, clients, waitingPodClaimName)
			createSharedClaimTestPod(ctx, clients, newClaimPod(waitingPodName, waitingPodClaimName, claimWorkloadContainer, vectorAddImage, nil))

			waitFor(ctx, claimPodReportTimeout, fmt.Sprintf("waiting for the scheduler to report pod %s/%s unschedulable", gpuWorkloadNamespace, waitingPodName), func(g gomega.Gomega) {
				waitingPod, err := clients.kube.CoreV1().Pods(gpuWorkloadNamespace).Get(ctx, waitingPodName, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(waitingPod.Status.Conditions).To(gomega.ContainElement(gomega.And(
					gomega.HaveField("Type", corev1.PodScheduled),
					gomega.HaveField("Status", corev1.ConditionFalse),
					gomega.HaveField("Reason", corev1.PodReasonUnschedulable),
				)))
			})
			expectForClaimStabilityPeriod(ctx, fmt.Sprintf("checking that pod %s/%s stays pending", gpuWorkloadNamespace, waitingPodName), func(g gomega.Gomega) {
				expectPodPendingWithUnallocatedClaim(ctx, g, clients, waitingPodName, waitingPodClaimName)
			})
		})

		ginkgo.It("keeps the second sharing pod running after the first one is deleted", func(ctx context.Context) {
			clients := install.clients
			deleteWorkloadPod(ctx, clients, firstSharingPodName)

			expectForClaimStabilityPeriod(ctx, fmt.Sprintf("checking that pod %s/%s keeps running", gpuWorkloadNamespace, secondSharingPodName), func(g gomega.Gomega) {
				secondSharingPod, err := clients.kube.CoreV1().Pods(gpuWorkloadNamespace).Get(ctx, secondSharingPodName, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(secondSharingPod.Status.Phase).To(gomega.Equal(corev1.PodRunning), "pod %s/%s", gpuWorkloadNamespace, secondSharingPodName)
				expectPodPendingWithUnallocatedClaim(ctx, g, clients, waitingPodName, waitingPodClaimName)
			})
		})

		ginkgo.It("runs the pending pod once the sharing pods and the shared claim are deleted", func(ctx context.Context) {
			clients := install.clients
			deleteWorkloadPod(ctx, clients, secondSharingPodName)
			deleteResourceClaim(ctx, clients, sharedGPUClaimName)

			waitForPodPhase(ctx, clients, waitingPodName, corev1.PodSucceeded, gpuWorkloadTimeout)
			expectClaimAllocatedByGPUDriver(ctx, clients, waitingPodClaimName)
			waitingPodLogs := workloadPodLogs(ctx, clients, waitingPodName, claimWorkloadContainer)
			gomega.Expect(waitingPodLogs).To(gomega.ContainSubstring("Test PASSED"), "vectorAdd did not report success")
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
		})
	})
}

// createSharedClaimTestClaim and createSharedClaimTestPod do not register a deletion, because the
// objects outlive the spec that creates them; the Context deletes them after its last spec.
func createSharedClaimTestClaim(ctx context.Context, clients clusterClients, name string) {
	ginkgo.GinkgoHelper()
	_, err := clients.kube.ResourceV1().ResourceClaims(gpuWorkloadNamespace).Create(ctx, newGPUResourceClaim(name), metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create ResourceClaim %s/%s", gpuWorkloadNamespace, name)
}

func createSharedClaimTestPod(ctx context.Context, clients clusterClients, pod *corev1.Pod) {
	ginkgo.GinkgoHelper()
	_, err := clients.kube.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create pod %s/%s", pod.Namespace, pod.Name)
}

// waitForPodGPUUUID returns the GPU UUID that the nvidia-smi -L of a long-running CUDA pod printed.
func waitForPodGPUUUID(ctx context.Context, clients clusterClients, podName string) string {
	ginkgo.GinkgoHelper()
	var gpuUUID string
	waitFor(ctx, claimPodReportTimeout, fmt.Sprintf("waiting for pod %s/%s to print its GPU UUID", gpuWorkloadNamespace, podName), func(g gomega.Gomega) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: gpuWorkloadNamespace}}
		podLogs, err := clients.pods.Logs(ctx, pod, &corev1.PodLogOptions{Container: longRunningCUDAWorkloadContainer})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		uuidMatch := gpuUUIDPattern.FindStringSubmatch(podLogs)
		g.Expect(uuidMatch).NotTo(gomega.BeNil(), "no GPU UUID in the logs:\n%s", podLogs)
		gpuUUID = uuidMatch[1]
	})
	return gpuUUID
}

// allocatedGPUUUID returns the uuid attribute that the ResourceSlice of nodeName publishes for
// the device allocated to claim.
func allocatedGPUUUID(ctx context.Context, clients clusterClients, claim *resourcev1.ResourceClaim, nodeName string) string {
	ginkgo.GinkgoHelper()
	allocatedDeviceName := claim.Status.Allocation.Devices.Results[0].Device
	devices, err := gpuResourceSliceDevices(ctx, clients, nodeName)
	framework.ExpectNoError(err, "failed to list the devices of node %s", nodeName)
	deviceIndex := slices.IndexFunc(devices, func(device resourcev1.Device) bool { return device.Name == allocatedDeviceName })
	gomega.Expect(deviceIndex).NotTo(gomega.Equal(-1), "node %s publishes no device %s", nodeName, allocatedDeviceName)
	return deviceStringAttribute(devices[deviceIndex], gpuDeviceUUIDAttribute)
}

func expectPodPendingWithUnallocatedClaim(ctx context.Context, g gomega.Gomega, clients clusterClients, podName, claimName string) {
	pod, err := clients.kube.CoreV1().Pods(gpuWorkloadNamespace).Get(ctx, podName, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(pod.Status.Phase).To(gomega.Equal(corev1.PodPending), "pod %s/%s", gpuWorkloadNamespace, podName)
	claim, err := clients.kube.ResourceV1().ResourceClaims(gpuWorkloadNamespace).Get(ctx, claimName, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(claim.Status.Allocation).To(gomega.BeNil(), "ResourceClaim %s/%s is allocated", gpuWorkloadNamespace, claimName)
}

func expectForClaimStabilityPeriod(ctx context.Context, description string, check func(g gomega.Gomega)) {
	ginkgo.GinkgoHelper()
	ginkgo.By(description)
	gomega.Consistently(ctx, check).WithTimeout(claimStabilityPeriod).WithPolling(pollInterval).Should(gomega.Succeed())
}

func standaloneDCGMSpecs(install *operatorInstall) {
	ginkgo.Context("with standalone DCGM", func() {
		ginkgo.BeforeAll(func(ctx context.Context) {
			gpuCluster, err := install.clients.gpuClusters.Get(ctx, gpuClusterName)
			framework.ExpectNoError(err, "failed to get GPUCluster/%s", gpuClusterName)
			ginkgo.DeferCleanup(restoreDCGMSpec, install.clients, gpuCluster.Spec.DCGM.DeepCopy())
		})

		ginkgo.It("points the DCGM exporter at the standalone DCGM hostengine", func(ctx context.Context) {
			clients := install.clients
			enableStandaloneDCGM := func(gpuCluster *nvidiav1alpha1.GPUCluster) {
				if gpuCluster.Spec.DCGM == nil {
					gpuCluster.Spec.DCGM = &nvidiav1.DCGMSpec{}
				}
				gpuCluster.Spec.DCGM.Enabled = new(true)
			}
			framework.ExpectNoError(clients.gpuClusters.Modify(ctx, gpuClusterName, enableStandaloneDCGM), "failed to enable standalone DCGM on GPUCluster/%s", gpuClusterName)

			waitForPodsReady(ctx, clients, appSelector(draDCGMApp))
			waitFor(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for Service %s to expose port %d", draDCGMApp, standaloneDCGMPort), func(g gomega.Gomega) {
				dcgmService, err := clients.kube.CoreV1().Services(tcfg.namespace).Get(ctx, draDCGMApp, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(dcgmService.Spec.Ports).To(gomega.ContainElement(gomega.HaveField("Port", int32(standaloneDCGMPort))))
			})
			waitForDCGMExporterRemoteHostEngine(ctx, clients, standaloneDCGMHostEngine)
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
		})

		ginkgo.It("exports the GPU utilization of a claim workload with its pod and namespace", func(ctx context.Context) {
			clients := install.clients
			claimName := dcgmWorkloadPodName + "-gpu"
			createGPUResourceClaim(ctx, clients, claimName)
			createWorkloadPod(ctx, clients, newLongRunningCUDAPod(dcgmWorkloadPodName, claimName, dcgmWorkloadRunDuration))
			waitForPodPhase(ctx, clients, dcgmWorkloadPodName, corev1.PodRunning, gpuWorkloadTimeout)
			workloadPod, err := clients.kube.CoreV1().Pods(gpuWorkloadNamespace).Get(ctx, dcgmWorkloadPodName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get pod %s/%s", gpuWorkloadNamespace, dcgmWorkloadPodName)
			nodeName := workloadPod.Spec.NodeName

			// dcgm-exporter names the labels that map a GPU to the pod using it "pod" and "namespace".
			isWorkloadBusySample := func(sample gpuMetricSample) bool {
				return sample.labels["pod"] == workloadPod.Name && sample.labels["namespace"] == workloadPod.Namespace &&
					sample.value > 0 && sample.value <= maxGPUUtilizationPercent
			}
			waitFor(ctx, dcgmWorkloadRunDuration, fmt.Sprintf("waiting for the DCGM exporter on node %s to report the GPU utilization of pod %s/%s", nodeName, workloadPod.Namespace, workloadPod.Name), func(g gomega.Gomega) {
				gpuUtilizationSamples, err := scrapeGPUUtilizationSamples(ctx, clients, nodeName)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(gpuUtilizationSamples).To(gomega.ContainElement(gomega.Satisfy(isWorkloadBusySample)))
			})

			waitForPodPhase(ctx, clients, dcgmWorkloadPodName, corev1.PodSucceeded, gpuWorkloadTimeout)
			expectClaimAllocatedByGPUDriver(ctx, clients, claimName)
			waitFor(ctx, dcgmExporterScrapeTimeout, fmt.Sprintf("waiting for the DCGM exporter on node %s to keep serving %s", nodeName, gpuUtilizationMetricName), func(g gomega.Gomega) {
				gpuUtilizationSamples, err := scrapeGPUUtilizationSamples(ctx, clients, nodeName)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(gpuUtilizationSamples).NotTo(gomega.BeEmpty())
			})
		})
	})
}

func restoreDCGMSpec(ctx context.Context, clients clusterClients, originalDCGMSpec *nvidiav1.DCGMSpec) {
	ginkgo.GinkgoHelper()
	err := clients.gpuClusters.Modify(ctx, gpuClusterName, func(gpuCluster *nvidiav1alpha1.GPUCluster) {
		gpuCluster.Spec.DCGM = originalDCGMSpec
	})
	framework.ExpectNoError(err, "failed to restore the DCGM spec of GPUCluster/%s", gpuClusterName)

	expectedRemoteHostEngine := ""
	if originalDCGMSpec != nil && ptr.Deref(originalDCGMSpec.Enabled, false) {
		expectedRemoteHostEngine = standaloneDCGMHostEngine
	} else {
		waitForPodsDeleted(ctx, clients, appSelector(draDCGMApp))
	}
	waitForDCGMExporterRemoteHostEngine(ctx, clients, expectedRemoteHostEngine)
	waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
}

// waitForDCGMExporterRemoteHostEngine waits until the DCGM exporter daemonset and every one of
// its pods set DCGM_REMOTE_HOSTENGINE_INFO to expectedRemoteHostEngine, or leave it unset when it
// is empty, and every pod is ready.
func waitForDCGMExporterRemoteHostEngine(ctx context.Context, clients clusterClients, expectedRemoteHostEngine string) {
	ginkgo.GinkgoHelper()
	description := fmt.Sprintf("waiting for the DCGM exporter pods to use the remote hostengine %q", expectedRemoteHostEngine)
	if expectedRemoteHostEngine == "" {
		description = "waiting for the DCGM exporter pods to use their embedded hostengine"
	}
	waitFor(ctx, operandReadyTimeout, description, func(g gomega.Gomega) {
		exporterDaemonSet, err := clients.daemonSets.Get(ctx, tcfg.namespace, draDCGMExporterApp)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(containerEnvValue(exporterDaemonSet.Spec.Template.Spec.Containers, draDCGMExporterContainer, dcgmRemoteHostEngineEnvName)).
			To(gomega.Equal(expectedRemoteHostEngine), "daemonset %s", exporterDaemonSet.Name)

		exporterPods, err := clients.pods.List(ctx, tcfg.namespace, appSelector(draDCGMExporterApp))
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(exporterPods).NotTo(gomega.BeEmpty(), "no DCGM exporter pods")
		for _, exporterPod := range exporterPods {
			g.Expect(containerEnvValue(exporterPod.Spec.Containers, draDCGMExporterContainer, dcgmRemoteHostEngineEnvName)).
				To(gomega.Equal(expectedRemoteHostEngine), "pod %s", exporterPod.Name)
			g.Expect(exporterPod.DeletionTimestamp).To(gomega.BeNil(), "pod %s is terminating", exporterPod.Name)
			g.Expect(helpers.IsPodReady(&exporterPod)).To(gomega.BeTrue(), "pod %s is not ready", exporterPod.Name)
		}
	})
}

// containerEnvValue returns the literal value of env variable envName in container containerName,
// or "" when it is not set.
func containerEnvValue(containers []corev1.Container, containerName, envName string) string {
	for _, container := range containers {
		if container.Name != containerName {
			continue
		}
		for _, envVar := range container.Env {
			if envVar.Name == envName {
				return envVar.Value
			}
		}
	}
	return ""
}

type gpuMetricSample struct {
	labels map[string]string
	value  float64
}

// scrapeGPUUtilizationSamples reads the DCGM_FI_DEV_GPU_UTIL samples from the DCGM exporter pod on
// nodeName through the API server pod proxy, so the test needs no route to the pod network.
func scrapeGPUUtilizationSamples(ctx context.Context, clients clusterClients, nodeName string) ([]gpuMetricSample, error) {
	exporterPods, err := clients.pods.List(ctx, tcfg.namespace, appSelector(draDCGMExporterApp))
	if err != nil {
		return nil, err
	}
	exporterPodIndex := slices.IndexFunc(exporterPods, func(pod corev1.Pod) bool {
		return pod.Spec.NodeName == nodeName && pod.DeletionTimestamp == nil
	})
	if exporterPodIndex == -1 {
		return nil, fmt.Errorf("no DCGM exporter pod runs on node %s", nodeName)
	}
	exporterPodName := exporterPods[exporterPodIndex].Name

	metricsText, err := clients.kube.CoreV1().Pods(tcfg.namespace).ProxyGet("http", exporterPodName, dcgmExporterMetricsPort, "/metrics", nil).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to scrape DCGM exporter pod %s: %w", exporterPodName, err)
	}
	metricsParser := expfmt.NewTextParser(model.UTF8Validation)
	metricFamilies, err := metricsParser.TextToMetricFamilies(bytes.NewReader(metricsText))
	if err != nil {
		return nil, fmt.Errorf("failed to parse the metrics of DCGM exporter pod %s: %w", exporterPodName, err)
	}

	var gpuUtilizationSamples []gpuMetricSample
	for _, metric := range metricFamilies[gpuUtilizationMetricName].GetMetric() {
		sampleLabels := make(map[string]string, len(metric.GetLabel()))
		for _, label := range metric.GetLabel() {
			sampleLabels[label.GetName()] = label.GetValue()
		}
		gpuUtilizationSamples = append(gpuUtilizationSamples, gpuMetricSample{labels: sampleLabels, value: metric.GetGauge().GetValue()})
	}
	return gpuUtilizationSamples, nil
}
