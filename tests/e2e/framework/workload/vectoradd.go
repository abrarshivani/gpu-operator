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

package workload

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/gpu-operator/internal/consts"
	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	e2eoperands "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operands"
	e2eoperator "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operator"
	"github.com/NVIDIA/gpu-operator/tests/e2e/helpers"
)

const (
	gpuWorkloadNamePrefix = "gpu-operator-test"

	gpuWorkloadCompletionTimeout = 45 * time.Minute
	gpuWorkloadDeletionTimeout   = 45 * time.Minute

	vectorAddImage          = "nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0-ubuntu22.04"
	vectorAddContainerName  = "cuda-vector-add"
	vectorAddSuccessMessage = "Test PASSED"

	workloadPodCompletionTimeout = 10 * time.Minute
	workloadDeletionTimeout      = 5 * time.Minute
	cleanupTimeoutMargin         = time.Minute
)

// newVectorAddPod creates the common CUDA sample Pod without any GPU allocation mechanism.
func newVectorAddPod(podName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: e2eoperator.WorkloadNamespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  vectorAddContainerName,
				Image: vectorAddImage,
			}},
		},
	}
}

func RunGPUWorkload(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoHelper()

	workloadPodName := fmt.Sprintf("%s-%s", gpuWorkloadNamePrefix, framework.RandomSuffix())
	workloadPod := newVectorAddPod(workloadPodName)
	workloadPod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("1"),
		},
	}

	ginkgo.DeferCleanup(deletePod, clients, workloadPodName, gpuWorkloadDeletionTimeout)

	_, err := clients.Workloads.DeployPod(ctx, e2eoperator.WorkloadNamespace, workloadPod)
	framework.ExpectNoError(err, "failed to create the GPU workload pod %s/%s", e2eoperator.WorkloadNamespace, workloadPodName)

	completionElapsed := verifyVectorAddPod(ctx, clients, workloadPod, corev1.PodSucceeded,
		gpuWorkloadCompletionTimeout, vectorAddSuccessMessage)
	ginkgo.GinkgoWriter.Printf("Pod %s/%s completed successfully (%s)\n", e2eoperator.WorkloadNamespace, workloadPodName, completionElapsed.Round(time.Second))
}

func describePodFailure(pod *corev1.Pod) string {
	failureDetails := []string{helpers.PodStatusSummary(pod)}
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.State.Terminated != nil {
			failureDetails = append(failureDetails, fmt.Sprintf("container %s exited with code %d", containerStatus.Name, containerStatus.State.Terminated.ExitCode))
		}
	}
	if pod.Status.Message != "" {
		failureDetails = append(failureDetails, pod.Status.Message)
	}
	return strings.Join(failureDetails, ", ")
}

func deletePod(ctx context.Context, clients e2eoperator.Clients, podName string, deletionTimeout time.Duration) {
	ginkgo.GinkgoHelper()

	ginkgo.By(fmt.Sprintf("Deleting workload pod %s/%s", e2eoperator.WorkloadNamespace, podName))
	err := clients.Workloads.Delete(ctx, e2eoperator.WorkloadNamespace, podName)
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete the workload pod %s/%s", e2eoperator.WorkloadNamespace, podName)

	framework.WaitForPodNotFound(ctx, clients.Kube, e2eoperator.WorkloadNamespace, podName, deletionTimeout)
}

func runVectorAddPod(ctx context.Context, clients e2eoperator.Clients, pod *corev1.Pod, expectedPhase corev1.PodPhase, expectedLogSubstrings ...string) {
	ginkgo.GinkgoHelper()
	podName := pod.Name
	ginkgo.DeferCleanup(func(ctx context.Context) {
		deletePod(ctx, clients, podName, workloadDeletionTimeout)
	}, ginkgo.NodeTimeout(workloadDeletionTimeout+cleanupTimeoutMargin))
	_, err := clients.Workloads.DeployPod(ctx, e2eoperator.WorkloadNamespace, pod)
	framework.ExpectNoError(err, "failed to create pod %s", podName)
	verifyVectorAddPod(ctx, clients, pod, expectedPhase, workloadPodCompletionTimeout, expectedLogSubstrings...)
}

// verifyVectorAddPod waits for a Pod to finish and checks the vectorAdd output.
func verifyVectorAddPod(ctx context.Context, clients e2eoperator.Clients, pod *corev1.Pod, expectedPhase corev1.PodPhase, completionTimeout time.Duration, expectedLogSubstrings ...string) time.Duration {
	ginkgo.GinkgoHelper()
	elapsed := waitForPodPhase(ctx, clients, pod.Name, expectedPhase, completionTimeout)
	ginkgo.By(fmt.Sprintf("Checking the logs of pod %s for the expected vectorAdd output", pod.Name))
	podLogs, err := clients.Pods.Logs(ctx, pod, &corev1.PodLogOptions{Container: pod.Spec.Containers[0].Name})
	framework.ExpectNoError(err, "failed to read the logs of pod %s", pod.Name)
	for _, expectedLogSubstring := range expectedLogSubstrings {
		gomega.Expect(podLogs).To(gomega.ContainSubstring(expectedLogSubstring), "the logs of pod %s do not contain %q", pod.Name, expectedLogSubstring)
	}
	return elapsed
}

func waitForPodPhase(ctx context.Context, clients e2eoperator.Clients, podName string, expectedPhase corev1.PodPhase, completionTimeout time.Duration) time.Duration {
	ginkgo.GinkgoHelper()
	waitDescription := fmt.Sprintf("Waiting for the pod to reach phase %s", expectedPhase)
	elapsed := framework.WaitFor(ctx, completionTimeout, waitDescription, func(g gomega.Gomega) {
		pod, err := clients.Workloads.Get(ctx, e2eoperator.WorkloadNamespace, podName)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to get pod %s", podName)
		if expectedPhase != pod.Status.Phase && isPodPhaseTerminal(pod.Status.Phase) {
			gomega.StopTrying(fmt.Sprintf("pod %s ended in phase %s instead of %s: %s",
				podName, pod.Status.Phase, expectedPhase, describePodFailure(pod))).Now()
		}
		g.Expect(pod.Status.Phase).To(gomega.Equal(expectedPhase), "%s", describePodProgress(pod))
	})
	ginkgo.GinkgoWriter.Printf("The pod reached phase %s (%s)\n", expectedPhase, elapsed.Round(time.Second))
	return elapsed
}

func describePodProgress(pod *corev1.Pod) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			return fmt.Sprintf("pod %s is %s: not scheduled (%s): %s", pod.Name, pod.Status.Phase, condition.Reason, condition.Message)
		}
	}
	return helpers.PodStatusSummary(pod)
}

func isPodPhaseTerminal(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}

const (
	vectorAddBinaryPath = "/cuda-samples/sample"

	noGPUDeviceNodesMessage                 = "no GPU device nodes visible"
	vectorAddAllocationFailureMessagePrefix = "Failed to allocate device vector A (error code"

	claimAllocationEventTimeout = time.Minute
	gpuClaimRequestName         = "gpu"
	podResourceClaimName        = "gpu"
)

func RunGPUClaimWorkload(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoHelper()
	podName := fmt.Sprintf("%s-claim-%s", gpuWorkloadNamePrefix, framework.RandomSuffix())
	claimName := podName + "-gpu"

	ginkgo.By(fmt.Sprintf("Creating ResourceClaim %s for one GPU in namespace %s", claimName, e2eoperator.WorkloadNamespace))
	createdClaim := createGPUResourceClaim(ctx, clients, claimName)
	// The resourceclaim controller clears status.allocation once the pod is done,
	// so the allocation is read from a watch started before the pod exists.
	claimWatcher := watchResourceClaim(ctx, clients, createdClaim)
	defer claimWatcher.Stop()

	ginkgo.By(fmt.Sprintf("Deploying vectorAdd pod %s with that claim", podName))
	runVectorAddPod(ctx, clients, newVectorAddClaimPod(podName, claimName), corev1.PodSucceeded, vectorAddSuccessMessage)
	ginkgo.GinkgoWriter.Printf("The vectorAdd sample passed on the GPU of the ResourceClaim\n")

	ginkgo.By(fmt.Sprintf("Checking that DRA driver %s allocated one GPU to ResourceClaim %s", e2eoperands.GPUDRADriverName, claimName))
	claimAllocation := receiveResourceClaimAllocation(ctx, claimWatcher, claimName)
	expectOneGPUAllocatedToClaimRequest(claimAllocation, claimName)
}

func RunVectorAddWithoutResourceClaim(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoHelper()
	podName := fmt.Sprintf("%s-no-claim-%s", gpuWorkloadNamePrefix, framework.RandomSuffix())

	ginkgo.By(fmt.Sprintf("Deploying vectorAdd pod %s without a ResourceClaim in namespace %s; it should see no GPU and fail",
		podName, e2eoperator.WorkloadNamespace))
	runVectorAddPod(ctx, clients, newClaimlessVectorAddPod(podName), corev1.PodFailed,
		noGPUDeviceNodesMessage, vectorAddAllocationFailureMessagePrefix)
	ginkgo.GinkgoWriter.Printf("The pod sees no GPU device node and vectorAdd fails on its first CUDA call\n")
}

func createGPUResourceClaim(ctx context.Context, clients e2eoperator.Clients, claimName string) *resourcev1.ResourceClaim {
	ginkgo.GinkgoHelper()
	ginkgo.DeferCleanup(func(ctx context.Context) {
		deleteResourceClaim(ctx, clients, claimName)
	}, ginkgo.NodeTimeout(workloadDeletionTimeout+cleanupTimeoutMargin))
	resourceClaimClient := clients.Kube.ResourceV1().ResourceClaims(e2eoperator.WorkloadNamespace)
	createdClaim, err := resourceClaimClient.Create(ctx, newGPUResourceClaim(claimName), metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create ResourceClaim %s", claimName)
	return createdClaim
}

func watchResourceClaim(ctx context.Context, clients e2eoperator.Clients, claim *resourcev1.ResourceClaim) watch.Interface {
	ginkgo.GinkgoHelper()
	resourceClaimClient := clients.Kube.ResourceV1().ResourceClaims(claim.Namespace)
	claimWatcher, err := resourceClaimClient.Watch(ctx, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", claim.Name).String(),
		ResourceVersion: claim.ResourceVersion,
	})
	framework.ExpectNoError(err, "failed to watch ResourceClaim %s", claim.Name)
	return claimWatcher
}

func receiveResourceClaimAllocation(ctx context.Context, claimWatcher watch.Interface, claimName string) *resourcev1.AllocationResult {
	ginkgo.GinkgoHelper()
	allocationTimer := time.NewTimer(claimAllocationEventTimeout)
	defer allocationTimer.Stop()
	for {
		select {
		case claimEvent, isWatchOpen := <-claimWatcher.ResultChan():
			if !isWatchOpen {
				ginkgo.Fail(fmt.Sprintf("the watch of ResourceClaim %s closed before any event recorded an allocation", claimName))
			}
			if claimEvent.Type == watch.Error {
				framework.ExpectNoError(apierrors.FromObject(claimEvent.Object), "the watch of ResourceClaim %s failed", claimName)
			}
			if claim, isResourceClaim := claimEvent.Object.(*resourcev1.ResourceClaim); isResourceClaim && claim.Status.Allocation != nil {
				return claim.Status.Allocation
			}
		case <-allocationTimer.C:
			ginkgo.Fail(fmt.Sprintf("no watch event of ResourceClaim %s recorded an allocation within %s", claimName, claimAllocationEventTimeout))
		case <-ctx.Done():
			framework.ExpectNoError(ctx.Err(), "stopped waiting for the allocation of ResourceClaim %s", claimName)
		}
	}
}

func expectOneGPUAllocatedToClaimRequest(claimAllocation *resourcev1.AllocationResult, claimName string) {
	ginkgo.GinkgoHelper()
	allocatedDevices := claimAllocation.Devices.Results
	gomega.Expect(allocatedDevices).To(gomega.HaveLen(1), "ResourceClaim %s should have exactly one allocated device", claimName)
	allocatedDevice := allocatedDevices[0]
	gomega.Expect(allocatedDevice.Request).To(gomega.Equal(gpuClaimRequestName), "the device of ResourceClaim %s was allocated for the wrong request", claimName)
	gomega.Expect(allocatedDevice.Driver).To(gomega.Equal(e2eoperands.GPUDRADriverName), "the device of ResourceClaim %s was allocated by the wrong DRA driver", claimName)
	gomega.Expect(allocatedDevice.Device).NotTo(gomega.BeEmpty(), "the allocation of ResourceClaim %s names no device", claimName)
	ginkgo.GinkgoWriter.Printf("DRA allocated one GPU to the ResourceClaim: driver %s, pool %s, device %s, request %s\n",
		allocatedDevice.Driver, allocatedDevice.Pool, allocatedDevice.Device, allocatedDevice.Request)
}

func newGPUResourceClaim(claimName string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: e2eoperator.WorkloadNamespace},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{
					Name: gpuClaimRequestName,
					Exactly: &resourcev1.ExactDeviceRequest{
						DeviceClassName: e2eoperands.GPUDeviceClassName,
						AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
						Count:           1,
					},
				}},
			},
		},
	}
}

func deleteResourceClaim(ctx context.Context, clients e2eoperator.Clients, claimName string) {
	ginkgo.GinkgoHelper()
	resourceClaimClient := clients.Kube.ResourceV1().ResourceClaims(e2eoperator.WorkloadNamespace)
	err := resourceClaimClient.Delete(ctx, claimName, metav1.DeleteOptions{})
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete ResourceClaim %s", claimName)
	framework.WaitForNotFound(ctx, workloadDeletionTimeout, "ResourceClaim "+claimName, func(ctx context.Context) error {
		_, err := resourceClaimClient.Get(ctx, claimName, metav1.GetOptions{})
		return err
	})
}

func newClaimlessVectorAddPod(podName string) *corev1.Pod {
	listGPUDeviceNodesThenRunVectorAddScript := fmt.Sprintf(`ls /dev/nvidia[0-9]* 2>/dev/null || echo %q; exec %s`,
		noGPUDeviceNodesMessage, vectorAddBinaryPath)
	pod := newGPUTaintTolerantVectorAddPod(podName)
	pod.Spec.NodeSelector = map[string]string{consts.GPUPresentLabel: "true"}
	pod.Spec.Containers[0].Command = []string{"sh", "-c", listGPUDeviceNodesThenRunVectorAddScript}
	return pod
}

func newVectorAddClaimPod(podName, claimName string) *corev1.Pod {
	pod := newGPUTaintTolerantVectorAddPod(podName)
	pod.Spec.ResourceClaims = []corev1.PodResourceClaim{{
		Name:              podResourceClaimName,
		ResourceClaimName: new(claimName),
	}}
	pod.Spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: podResourceClaimName}}
	return pod
}

func newGPUTaintTolerantVectorAddPod(podName string) *corev1.Pod {
	pod := newVectorAddPod(podName)
	pod.Spec.Tolerations = []corev1.Toleration{{
		Key:      "nvidia.com/gpu",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	return pod
}
