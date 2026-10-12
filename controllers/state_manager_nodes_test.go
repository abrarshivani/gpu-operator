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

package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
)

func TestGetGPUNodeOSInfo(t *testing.T) {
	testCases := []struct {
		name          string
		osName        string
		osVersion     string
		expectedOSTag string
	}{
		{
			name:          "talos version with v prefix",
			osName:        "talos",
			osVersion:     "v1.12.6",
			expectedOSTag: "talosv1.12.6",
		},
		{
			name:          "rhel 9 omits minor version",
			osName:        "rhel",
			osVersion:     "9.4",
			expectedOSTag: "rhel9",
		},
		{
			name:          "rhel 8 omits minor version",
			osName:        "rhel",
			osVersion:     "8.10",
			expectedOSTag: "rhel8",
		},
		{
			name:          "rhel 10 omits minor version",
			osName:        "rhel",
			osVersion:     "10.2",
			expectedOSTag: "rhel10",
		},
		{
			name:          "rocky omits minor version",
			osName:        "rocky",
			osVersion:     "9.5",
			expectedOSTag: "rocky9",
		},
		{
			name:          "ol omits minor version",
			osName:        "ol",
			osVersion:     "9.5",
			expectedOSTag: "ol9",
		},
		{
			name:          "ubuntu preserves full version",
			osName:        "ubuntu",
			osVersion:     "24.04",
			expectedOSTag: "ubuntu24.04",
		},
		{
			name:          "sles preserves dotted version",
			osName:        "sles",
			osVersion:     "15.6",
			expectedOSTag: "sles15.6",
		},
		{
			name:          "sles preserves service-pack version",
			osName:        "sles",
			osVersion:     "15-SP6",
			expectedOSTag: "sles15-SP6",
		},
		{
			name:          "sl-micro preserves dotted version",
			osName:        "sl-micro",
			osVersion:     "6.0",
			expectedOSTag: "sl-micro6.0",
		},
		{
			name:          "archlinux preserves rolling version",
			osName:        "archlinux",
			osVersion:     "rolling",
			expectedOSTag: "archlinuxrolling",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := gpuNodeWithOSLabels("gpu-node-1", tc.osName, tc.osVersion)
			controller := newClusterPolicyController(newNodeClient(t, interceptor.Funcs{}, node))

			osName, osTag, err := controller.getGPUNodeOSInfo()

			require.NoError(t, err)
			require.Equal(t, tc.osName, osName)
			require.Equal(t, tc.expectedOSTag, osTag)
		})
	}
}

func TestGetGPUNodeOSInfoListError(t *testing.T) {
	expectedErr := errors.New("list failed")
	controller := newClusterPolicyController(newErrorListClient(t, expectedErr))

	osName, osTag, err := controller.getGPUNodeOSInfo()
	require.ErrorIs(t, err, expectedErr)
	require.Empty(t, osName)
	require.Empty(t, osTag)
	require.ErrorContains(t, err, "unable to list nodes with GPU present")
}

func TestGetGPUNodeOSInfoNoGPUNodes(t *testing.T) {
	controller := newClusterPolicyController(newNodeClient(t, interceptor.Funcs{}))

	osName, osTag, err := controller.getGPUNodeOSInfo()
	require.ErrorContains(t, err, "no nodes found with GPU present")
	require.Empty(t, osName)
	require.Empty(t, osTag)
}

func TestGetGPUNodeOSInfoMissingLabels(t *testing.T) {
	testCases := []struct {
		name          string
		nodeLabels    map[string]string
		expectedError string
	}{
		{
			name: "missing OS release label",
			nodeLabels: map[string]string{
				commonGPULabelKey:      commonGPULabelValue,
				nfdOSVersionIDLabelKey: "9.4",
			},
			expectedError: "unable to retrieve OS name",
		},
		{
			name: "missing OS version label",
			nodeLabels: map[string]string{
				commonGPULabelKey:      commonGPULabelValue,
				nfdOSReleaseIDLabelKey: "rhel",
			},
			expectedError: "unable to retrieve OS version",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := nodeWithLabels("gpu-node-1", tc.nodeLabels)
			controller := newClusterPolicyController(newNodeClient(t, interceptor.Funcs{}, node))

			osName, osTag, err := controller.getGPUNodeOSInfo()
			require.ErrorContains(t, err, tc.expectedError)
			require.Empty(t, osName)
			require.Empty(t, osTag)
		})
	}
}

func TestGetGPUNodeOSInfoListsOneGPUNode(t *testing.T) {
	// The fake client applies label selectors but ignores Limit, so a results-only assertion
	// passes whether or not the option is set. Capturing the options is the only way to pin it.
	var capturedListOptions []ctrlclient.ListOptions
	client := newNodeClient(t, interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			options := ctrlclient.ListOptions{}
			for _, option := range opts {
				option.ApplyToList(&options)
			}
			capturedListOptions = append(capturedListOptions, options)
			return c.List(ctx, list, opts...)
		},
	}, gpuNodeWithOSLabels("gpu-1", "ubuntu", "22.04"))
	controller := newClusterPolicyController(client)

	_, osTag, err := controller.getGPUNodeOSInfo()

	require.NoError(t, err)
	require.Equal(t, "ubuntu22.04", osTag)
	require.Len(t, capturedListOptions, 1)
	require.EqualValues(t, 1, capturedListOptions[0].Limit, "only the first GPU node is needed, so the list must be limited")
	require.NotNil(t, capturedListOptions[0].LabelSelector)
	require.Equal(t, labels.SelectorFromSet(labels.Set{commonGPULabelKey: commonGPULabelValue}).String(),
		capturedListOptions[0].LabelSelector.String())
}

func TestDiscoverGPUNodesListError(t *testing.T) {
	expectedErr := errors.New("list failed")
	controller := newClusterPolicyController(newErrorListClient(t, expectedErr))

	hasNFDLabels, gpuNodeCount, err := controller.discoverGPUNodes()

	require.ErrorIs(t, err, expectedErr)
	require.ErrorContains(t, err, "unable to list nodes")
	require.False(t, hasNFDLabels)
	require.Zero(t, gpuNodeCount)
}

func TestDiscoverGPUNodes(t *testing.T) {
	testCases := []struct {
		name                   string
		nodes                  []*corev1.Node
		driverToolkitRequested bool
		expectedNFDLabels      bool
		expectedGPUNodeCount   int
		expectedRHCOSVersions  map[string]bool
	}{
		{
			name: "counts only nodes carrying the common GPU label",
			nodes: []*corev1.Node{
				nodeWithLabels("gpu-1", map[string]string{commonGPULabelKey: commonGPULabelValue}),
				nodeWithLabels("gpu-2", map[string]string{commonGPULabelKey: commonGPULabelValue}),
				nodeWithLabels("cpu-1", map[string]string{"kubernetes.io/hostname": "cpu-1"}),
				nodeWithLabels("cpu-2", map[string]string{commonGPULabelKey: "false"}),
			},
			expectedGPUNodeCount:  2,
			expectedRHCOSVersions: map[string]bool{},
		},
		{
			name: "reports NFD labels from a node that carries no GPU label",
			nodes: []*corev1.Node{
				nodeWithLabels("cpu-1", map[string]string{nfdLabelPrefix + "cpu-model.vendor_id": "Intel"}),
			},
			expectedNFDLabels:     true,
			expectedGPUNodeCount:  0,
			expectedRHCOSVersions: map[string]bool{},
		},
		{
			name: "reports no NFD labels when no node carries them",
			nodes: []*corev1.Node{
				nodeWithLabels("gpu-1", map[string]string{commonGPULabelKey: commonGPULabelValue}),
			},
			expectedGPUNodeCount:  1,
			expectedRHCOSVersions: map[string]bool{},
		},
		{
			name: "ignores RHCOS versions when the driver toolkit is not requested",
			nodes: []*corev1.Node{
				nodeWithLabels("gpu-1", map[string]string{
					commonGPULabelKey:        commonGPULabelValue,
					nfdOSTreeVersionLabelKey: "413.92.202",
				}),
			},
			driverToolkitRequested: false,
			expectedNFDLabels:      true,
			expectedGPUNodeCount:   1,
			expectedRHCOSVersions:  map[string]bool{},
		},
		{
			name: "collects RHCOS versions from GPU nodes when the driver toolkit is requested",
			nodes: []*corev1.Node{
				nodeWithLabels("gpu-1", map[string]string{
					commonGPULabelKey:        commonGPULabelValue,
					nfdOSTreeVersionLabelKey: "413.92.202",
				}),
				nodeWithLabels("gpu-2", map[string]string{
					commonGPULabelKey:        commonGPULabelValue,
					nfdOSTreeVersionLabelKey: "414.92.202",
				}),
			},
			driverToolkitRequested: true,
			expectedNFDLabels:      true,
			expectedGPUNodeCount:   2,
			expectedRHCOSVersions:  map[string]bool{"413.92.202": true, "414.92.202": true},
		},
		{
			name: "skips a GPU node missing the RHCOS version label",
			nodes: []*corev1.Node{
				nodeWithLabels("gpu-1", map[string]string{commonGPULabelKey: commonGPULabelValue}),
			},
			driverToolkitRequested: true,
			expectedGPUNodeCount:   1,
			expectedRHCOSVersions:  map[string]bool{},
		},
		{
			name:                  "reports an empty cluster",
			expectedGPUNodeCount:  0,
			expectedRHCOSVersions: map[string]bool{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			metrics := newStateManagerMetrics(t)
			controller := newClusterPolicyController(newNodeClient(t, interceptor.Funcs{}, tc.nodes...))
			controller.operatorMetrics = metrics.operatorMetrics
			controller.ocpDriverToolkit = OpenShiftDriverToolkit{
				requested:     tc.driverToolkitRequested,
				rhcosVersions: map[string]bool{},
			}

			hasNFDLabels, gpuNodeCount, err := controller.discoverGPUNodes()

			require.NoError(t, err)
			require.Equal(t, tc.expectedNFDLabels, hasNFDLabels)
			require.Equal(t, tc.expectedGPUNodeCount, gpuNodeCount)
			require.Equal(t, tc.expectedRHCOSVersions, controller.ocpDriverToolkit.rhcosVersions)
			require.Equal(t, float64(tc.expectedGPUNodeCount), metrics.gpuNodesTotal.value)
		})
	}
}

func TestGetRuntimeString(t *testing.T) {
	testCases := []struct {
		name                    string
		containerRuntimeVersion string
		expectedRuntime         gpuv1.Runtime
		expectedError           string
	}{
		{
			name:                    "containerd",
			containerRuntimeVersion: "containerd://1.0.0",
			expectedRuntime:         gpuv1.Containerd,
		},
		{
			name:                    "docker",
			containerRuntimeVersion: "docker://1.0.0",
			expectedRuntime:         gpuv1.Docker,
		},
		{
			name:                    "crio",
			containerRuntimeVersion: "cri-o://1.0.0",
			expectedRuntime:         gpuv1.CRIO,
		},
		{
			name:                    "unknown",
			containerRuntimeVersion: "unknown://1.0.0",
			expectedError:           "runtime not recognized: unknown://1.0.0",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := corev1.Node{
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{
						ContainerRuntimeVersion: tc.containerRuntimeVersion,
					},
				},
			}
			runtime, err := getRuntimeString(node)

			require.Equal(t, tc.expectedRuntime, runtime)
			if tc.expectedError == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.expectedError)
			}
		})
	}
}

func newGPUNodeWithRuntime(name, containerRuntimeVersion string) *corev1.Node {
	node := nodeWithLabels(name, map[string]string{commonGPULabelKey: commonGPULabelValue})
	node.Status.NodeInfo.ContainerRuntimeVersion = containerRuntimeVersion
	return node
}

func newNonGPUNodeWithRuntime(name, containerRuntimeVersion string) *corev1.Node {
	node := nodeWithLabels(name, map[string]string{})
	node.Status.NodeInfo.ContainerRuntimeVersion = containerRuntimeVersion
	return node
}

func TestGetRuntimeOnOpenShiftSkipsNodeLookup(t *testing.T) {
	// OpenShift short-circuits to CRI-O before listing. The erroring client is the falsifier:
	// delete the early return and getRuntime reaches the List, which fails the test.
	controller := newClusterPolicyController(newErrorListClient(t, errors.New("list must not be called")))
	controller.openshift = "4.16"

	require.NoError(t, controller.getRuntime())
	require.Equal(t, gpuv1.CRIO, controller.runtime)
}

func TestGetRuntimeListError(t *testing.T) {
	controller := newClusterPolicyController(newErrorListClient(t, errors.New("list failed")))

	err := controller.getRuntime()

	// getRuntime formats the cause with %v, so the chain is broken and only text can be asserted.
	require.ErrorContains(t, err, "unable to list nodes prior to checking container runtime")
	require.ErrorContains(t, err, "list failed")
}

func TestGetRuntimeFromNodes(t *testing.T) {
	// The fake client returns nodes ordered by name, not in the order they are built, so the
	// fixtures are named to make the intended visit order explicit.
	testCases := []struct {
		name            string
		nodes           []*corev1.Node
		expectedRuntime gpuv1.Runtime
	}{
		{
			name:            "docker node",
			nodes:           []*corev1.Node{newGPUNodeWithRuntime("node-1", "docker://24.0.0")},
			expectedRuntime: gpuv1.Docker,
		},
		{
			name: "containerd wins when it is visited after docker",
			nodes: []*corev1.Node{
				newGPUNodeWithRuntime("node-1-docker", "docker://24.0.0"),
				newGPUNodeWithRuntime("node-2-containerd", "containerd://1.7.0"),
			},
			expectedRuntime: gpuv1.Containerd,
		},
		{
			name: "containerd wins when it is visited before docker",
			nodes: []*corev1.Node{
				newGPUNodeWithRuntime("node-1-containerd", "containerd://1.7.0"),
				newGPUNodeWithRuntime("node-2-docker", "docker://24.0.0"),
			},
			expectedRuntime: gpuv1.Containerd,
		},
		{
			name: "unrecognized runtimes are skipped in favor of a later node",
			nodes: []*corev1.Node{
				newGPUNodeWithRuntime("node-1-unknown", "mystery://1.0.0"),
				newGPUNodeWithRuntime("node-2-crio", "cri-o://1.30.0"),
			},
			expectedRuntime: gpuv1.CRIO,
		},
		{
			name:            "defaults to containerd when every runtime is unrecognized",
			nodes:           []*corev1.Node{newGPUNodeWithRuntime("node-1", "mystery://1.0.0")},
			expectedRuntime: gpuv1.Containerd,
		},
		{
			name:            "defaults to containerd when no node carries the GPU label",
			nodes:           []*corev1.Node{newNonGPUNodeWithRuntime("node-1", "docker://24.0.0")},
			expectedRuntime: gpuv1.Containerd,
		},
		{
			name:            "defaults to containerd on an empty cluster",
			expectedRuntime: gpuv1.Containerd,
		},
		{
			// Only the GPU-labeled node may be consulted. Dropping the MatchingLabels option
			// would let the unlabeled containerd node be found and break the loop early.
			name: "ignores an unlabeled node that would otherwise win",
			nodes: []*corev1.Node{
				newNonGPUNodeWithRuntime("node-1-unlabeled", "containerd://1.7.0"),
				newGPUNodeWithRuntime("node-2-labeled", "cri-o://1.30.0"),
			},
			expectedRuntime: gpuv1.CRIO,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			controller := newClusterPolicyController(newNodeClient(t, interceptor.Funcs{}, tc.nodes...))

			require.NoError(t, controller.getRuntime())
			require.Equal(t, tc.expectedRuntime, controller.runtime)
		})
	}
}
