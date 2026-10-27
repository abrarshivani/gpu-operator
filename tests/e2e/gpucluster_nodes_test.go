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
	"net"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
)

const (
	nodeRebootTimeout = 20 * time.Minute

	nodeRebootPodName   = e2eTestPrefix + "-node-reboot"
	nodeRebootContainer = "reboot"
	nodeRebootImage     = "nvcr.io/nvidia/cuda:12.6.0-base-ubuntu22.04"
)

func nodeRebootSpecs(install *operatorInstall) {
	ginkgo.Context("when the GPU node reboots", ginkgo.Label("disruptive"), func() {
		var rebootedNodeName string
		var preRebootSliceUIDs map[types.UID]bool

		ginkgo.BeforeAll(func(ctx context.Context) {
			gpuNodes, err := install.clients.nodes.ListGPUNodes(ctx)
			framework.ExpectNoError(err, "failed to list GPU nodes")
			gomega.Expect(gpuNodes).NotTo(gomega.BeEmpty(), "no GPU nodes found")
			gpuNode := gpuNodes[0]

			nodeIsThisHost, err := isNodeThisHost(&gpuNode)
			framework.ExpectNoError(err, "failed to compare node %s with this host", gpuNode.Name)
			if nodeIsThisHost {
				ginkgo.Skip(fmt.Sprintf("GPU node %s is the host running this suite, so rebooting it would end the run; run the suite from another host to cover the node reboot", gpuNode.Name))
			}
			rebootedNodeName = gpuNode.Name
		})

		ginkgo.It("reboots the GPU node through a privileged pod", func(ctx context.Context) {
			clients := install.clients
			node, err := clients.kube.CoreV1().Nodes().Get(ctx, rebootedNodeName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get node %s", rebootedNodeName)
			preRebootBootID := node.Status.NodeInfo.BootID
			gomega.Expect(preRebootBootID).NotTo(gomega.BeEmpty(), "node %s reports no boot ID", rebootedNodeName)

			preRebootSliceUIDs, err = gpuResourceSliceUIDs(ctx, clients, rebootedNodeName)
			framework.ExpectNoError(err, "failed to list the ResourceSlices of node %s", rebootedNodeName)
			gomega.Expect(preRebootSliceUIDs).NotTo(gomega.BeEmpty(), "node %s publishes no ResourceSlice of %s", rebootedNodeName, gpuDriverName)

			_, err = clients.kube.CoreV1().Pods(tcfg.namespace).Create(ctx, newNodeRebootPod(rebootedNodeName, preRebootBootID), metav1.CreateOptions{})
			framework.ExpectNoError(err, "failed to create pod %s/%s", tcfg.namespace, nodeRebootPodName)
			ginkgo.DeferCleanup(deleteNodeRebootPod, clients)

			waitFor(ctx, nodeRebootTimeout, fmt.Sprintf("waiting for node %s to boot again and be ready", rebootedNodeName), func(g gomega.Gomega) {
				node, err := clients.kube.CoreV1().Nodes().Get(ctx, rebootedNodeName, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(node.Status.NodeInfo.BootID).NotTo(gomega.Equal(preRebootBootID), "node %s has not rebooted", rebootedNodeName)
				g.Expect(isNodeReady(node)).To(gomega.BeTrue(), "node %s is not ready", rebootedNodeName)
			})
		})

		ginkgo.It("republishes the GPUs and brings the DRA operands back", func(ctx context.Context) {
			clients := install.clients
			waitForResourceSlicesDeleted(ctx, clients, preRebootSliceUIDs)
			waitForGPUResourceSlices(ctx, clients)
			waitForDRAOperandsReady(ctx, clients)
			waitForGPUClusterReady(ctx, clients, operandReadyTimeout)
		})

		ginkgo.It("runs a GPU workload with a ResourceClaim", func(ctx context.Context) {
			runGPUClaimWorkload(ctx, install.clients)
		})
	})
}

func nvidiaDriverWithGPUClusterSpecs(install *operatorInstall) {
	ginkgo.Context("with NVIDIADrivers", ginkgo.Label("disruptive"), func() {
		ginkgo.BeforeAll(func() {
			ginkgo.DeferCleanup(func(ctx context.Context) {
				for _, driverName := range []string{duplicateDefaultDriverName, userDriverName, defaultDriverName} {
					framework.ExpectNoError(install.clients.drivers.DeleteIfExists(ctx, driverName), "failed to delete NVIDIADriver/%s", driverName)
				}
			})
		})

		ginkgo.It("moves the default to an arbitrarily named NVIDIADriver", func(ctx context.Context) {
			moveDefaultToArbitrarilyNamedDriver(ctx, install.clients)
		})

		ginkgo.It("hands GPU nodes to a user-defined NVIDIADriver", func(ctx context.Context) {
			handGPUNodesToUserDriver(ctx, install.clients)
		})

		ginkgo.It("applies custom labels to the driver pods", func(ctx context.Context) {
			applyCustomLabelsToDriverPods(ctx, install.clients)
		})

		ginkgo.It("reports a node conflict when the default field is cleared", func(ctx context.Context) {
			verifyNodeConflictWhenDefaultCleared(ctx, install.clients)
		})

		ginkgo.It("reports multiple default NVIDIADrivers as not ready", func(ctx context.Context) {
			verifyMultipleDefaultDriversNotReady(ctx, install.clients)
		})

		ginkgo.It("brings the GPUCluster back and runs a GPU workload with a ResourceClaim", func(ctx context.Context) {
			clients := install.clients
			waitForGPUClusterReady(ctx, clients, operandReadyTimeout)
			waitForDRAOperandsReady(ctx, clients)
			runGPUClaimWorkload(ctx, clients)
		})
	})
}

// isNodeThisHost reports whether node is the host running this process, matching its name or
// hostname address against os.Hostname and its IP addresses against the local interfaces.
func isNodeThisHost(node *corev1.Node) (bool, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return false, fmt.Errorf("failed to get the hostname: %w", err)
	}
	interfaceAddresses, err := net.InterfaceAddrs()
	if err != nil {
		return false, fmt.Errorf("failed to list the interface addresses: %w", err)
	}

	if strings.EqualFold(node.Name, hostname) {
		return true, nil
	}
	for _, nodeAddress := range node.Status.Addresses {
		if nodeAddress.Type == corev1.NodeHostName && strings.EqualFold(nodeAddress.Address, hostname) {
			return true, nil
		}
		nodeIP := net.ParseIP(nodeAddress.Address)
		if nodeIP == nil {
			continue
		}
		for _, interfaceAddress := range interfaceAddresses {
			if interfaceNetwork, isIPNetwork := interfaceAddress.(*net.IPNet); isIPNetwork && interfaceNetwork.IP.Equal(nodeIP) {
				return true, nil
			}
		}
	}
	return false, nil
}

func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// newNodeRebootPod returns a pod that reboots nodeName from the host's namespaces.
func newNodeRebootPod(nodeName, bootID string) *corev1.Pod {
	// kubelet can run a RestartPolicy=Never pod again once the reboot has wiped the container
	// runtime state, so the pod reboots the node only during the boot it was created in.
	rebootCommand := fmt.Sprintf(
		`if [ "$(cat /proc/sys/kernel/random/boot_id)" = '%s' ]; then exec nsenter --target 1 --mount --uts --ipc --net --pid -- systemctl reboot; fi`,
		bootID)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: nodeRebootPodName, Namespace: tcfg.namespace},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            nodeRebootContainer,
				Image:           nodeRebootImage,
				Command:         []string{"sh", "-c", rebootCommand},
				SecurityContext: &corev1.SecurityContext{Privileged: new(true)},
			}},
		},
	}
}

func deleteNodeRebootPod(ctx context.Context, clients clusterClients) {
	ginkgo.GinkgoHelper()
	err := clients.kube.CoreV1().Pods(tcfg.namespace).Delete(ctx, nodeRebootPodName, metav1.DeleteOptions{})
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete pod %s/%s", tcfg.namespace, nodeRebootPodName)
	waitForPodNotFound(ctx, clients, tcfg.namespace, nodeRebootPodName, podDeletionTimeout)
}

// gpuResourceSliceUIDs returns the UIDs of the ResourceSlices that the DRA driver publishes for nodeName.
func gpuResourceSliceUIDs(ctx context.Context, clients clusterClients, nodeName string) (map[types.UID]bool, error) {
	resourceSlices, err := clients.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list ResourceSlices: %w", err)
	}
	sliceUIDs := make(map[types.UID]bool)
	for _, resourceSlice := range resourceSlices.Items {
		if resourceSlice.Spec.Driver == gpuDriverName && resourceSlice.Spec.NodeName != nil && *resourceSlice.Spec.NodeName == nodeName {
			sliceUIDs[resourceSlice.UID] = true
		}
	}
	return sliceUIDs, nil
}

// waitForResourceSlicesDeleted waits until none of the ResourceSlices with sliceUIDs exists. kubelet
// deletes the ResourceSlices of its node when it starts, so a reboot replaces every one of them.
func waitForResourceSlicesDeleted(ctx context.Context, clients clusterClients, sliceUIDs map[types.UID]bool) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, operandReadyTimeout, "waiting for the ResourceSlices published before the reboot to be deleted", func(g gomega.Gomega) {
		resourceSlices, err := clients.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		for _, resourceSlice := range resourceSlices.Items {
			g.Expect(sliceUIDs).NotTo(gomega.HaveKey(resourceSlice.UID), "ResourceSlice %s still exists", resourceSlice.Name)
		}
	})
}
