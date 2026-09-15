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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
)

func TestIsValidWorkloadConfig(t *testing.T) {
	testCases := []struct {
		config           string
		expectedValidity bool
	}{
		{gpuWorkloadConfigContainer, true}, {gpuWorkloadConfigVMPassthrough, true}, {gpuWorkloadConfigVMVgpu, true},
		{"invalid", false}, {"", false},
	}
	for _, tc := range testCases {
		if got := isValidWorkloadConfig(tc.config); got != tc.expectedValidity {
			t.Errorf("isValidWorkloadConfig(%q) = %v, expected %v", tc.config, got, tc.expectedValidity)
		}
	}
}

func TestHasOperandsDisabled(t *testing.T) {
	testCases := []struct {
		labels                   map[string]string
		expectedOperandsDisabled bool
	}{
		{map[string]string{commonOperandsLabelKey: "false"}, true},
		{map[string]string{commonOperandsLabelKey: commonOperandsLabelValue}, false},
		{map[string]string{}, false},
	}
	for _, tc := range testCases {
		if got := hasOperandsDisabled(tc.labels); got != tc.expectedOperandsDisabled {
			t.Errorf("hasOperandsDisabled(%v) = %v, expected %v", tc.labels, got, tc.expectedOperandsDisabled)
		}
	}
}

func TestHasNFDLabels(t *testing.T) {
	testCases := []struct {
		labels            map[string]string
		expectedNFDLabels bool
	}{
		{map[string]string{nfdLabelPrefix + "cpu": "true"}, true},
		{map[string]string{"other-label": "value"}, false},
		{map[string]string{}, false},
	}
	for _, tc := range testCases {
		if got := hasNFDLabels(tc.labels); got != tc.expectedNFDLabels {
			t.Errorf("hasNFDLabels(%v) = %v, expected %v", tc.labels, got, tc.expectedNFDLabels)
		}
	}
}

func TestHasMIGManagerLabel(t *testing.T) {
	testCases := []struct {
		labels                  map[string]string
		expectedMIGManagerLabel bool
	}{
		{map[string]string{migManagerLabelKey: migManagerLabelValue}, true},
		{map[string]string{"other": "value"}, false},
	}
	for _, tc := range testCases {
		if got := hasMIGManagerLabel(tc.labels); got != tc.expectedMIGManagerLabel {
			t.Errorf("hasMIGManagerLabel(%v) = %v, expected %v", tc.labels, got, tc.expectedMIGManagerLabel)
		}
	}
}

func TestHasCommonGPULabel(t *testing.T) {
	testCases := []struct {
		labels                 map[string]string
		expectedCommonGPULabel bool
	}{
		{map[string]string{commonGPULabelKey: commonGPULabelValue}, true},
		{map[string]string{commonGPULabelKey: "false"}, false},
		{map[string]string{}, false},
	}
	for _, tc := range testCases {
		if got := hasCommonGPULabel(tc.labels); got != tc.expectedCommonGPULabel {
			t.Errorf("hasCommonGPULabel(%v) = %v, expected %v", tc.labels, got, tc.expectedCommonGPULabel)
		}
	}
}

func TestHasGPULabels(t *testing.T) {
	testCases := []struct {
		labels            map[string]string
		expectedGPULabels bool
	}{
		{map[string]string{nfdLabelPrefix + "pci-10de.present": "true"}, true},
		{map[string]string{nfdLabelPrefix + "pci-0302_10de.present": "true"}, true},
		{map[string]string{nfdLabelPrefix + "pci-0300_10de.present": "true"}, true},
		{map[string]string{nfdLabelPrefix + "pci-10de.present": "false"}, false},
		{map[string]string{"other": "true"}, false},
	}
	for _, tc := range testCases {
		if got := hasGPULabels(tc.labels); got != tc.expectedGPULabels {
			t.Errorf("hasGPULabels(%v) = %v, expected %v", tc.labels, got, tc.expectedGPULabels)
		}
	}
}

func TestHasMIGCapableGPU(t *testing.T) {
	testCases := []struct {
		labels                map[string]string
		expectedMIGCapableGPU bool
	}{
		{map[string]string{migCapableLabelKey: migCapableLabelValue}, true},
		{map[string]string{migCapableLabelKey: "false"}, false},
		{map[string]string{gpuProductLabelKey: "NVIDIA-A100"}, true},
		{map[string]string{gpuProductLabelKey: "NVIDIA-H100"}, true},
		{map[string]string{gpuProductLabelKey: "NVIDIA-A30"}, true},
		{map[string]string{gpuProductLabelKey: "NVIDIA-T4"}, false},
		{map[string]string{vgpuHostDriverLabelKey: "535.54"}, false},
		{map[string]string{vgpuHostDriverLabelKey: "535.54", gpuProductLabelKey: "NVIDIA-A100"}, false},
		{map[string]string{vgpuHostDriverLabelKey: "", gpuProductLabelKey: "NVIDIA-A100"}, true},
		{map[string]string{migCapableLabelKey: "false", gpuProductLabelKey: "NVIDIA-A100"}, false},
		{map[string]string{}, false},
	}
	for _, tc := range testCases {
		if got := hasMIGCapableGPU(tc.labels); got != tc.expectedMIGCapableGPU {
			t.Errorf("hasMIGCapableGPU(%v) = %v, expected %v", tc.labels, got, tc.expectedMIGCapableGPU)
		}
	}
}

func TestGetWorkloadConfigInvalidLabelValue(t *testing.T) {
	isolateDefaultGPUWorkloadConfig(t)
	// Must differ from "container": that is both the package default and the value the
	// !sandboxEnabled branch returns literally, so either would pass for the wrong reason.
	defaultGPUWorkloadConfig = gpuWorkloadConfigVMVgpu

	config, err := getWorkloadConfig(map[string]string{gpuWorkloadConfigLabelKey: "vm-bogus"}, true)

	// getWorkloadConfig formats the offending value with %v and wraps no cause, so only the
	// message can be asserted.
	require.ErrorContains(t, err, "invalid GPU workload config: vm-bogus")
	require.Equal(t, gpuWorkloadConfigVMVgpu, config)
}

func TestGetEffectiveStateLabels(t *testing.T) {
	// Without this guard the vm-passthrough subtests below leak their edits into every later test
	// in the binary: the maps they are handed are the package-level ones, not copies.
	isolateGPUStateLabels(t)
	// Every expectation below is spelled out in full and as literals. A spot check on one key
	// lets a label go missing from the map unnoticed, and naming the keys through the same
	// constants production reads would let a renamed constant pass.
	t.Run("container", func(t *testing.T) {
		require.Equal(t, map[string]string{
			"nvidia.com/gpu.deploy.driver":                "true",
			"nvidia.com/gpu.deploy.gpu-feature-discovery": "true",
			"nvidia.com/gpu.deploy.container-toolkit":     "true",
			"nvidia.com/gpu.deploy.device-plugin":         "true",
			"nvidia.com/gpu.deploy.dcgm":                  "true",
			"nvidia.com/gpu.deploy.dcgm-exporter":         "true",
			"nvidia.com/gpu.deploy.node-status-exporter":  "true",
			"nvidia.com/gpu.deploy.operator-validator":    "true",
			"nvidia.com/gpu.deploy.client":                "true",
		}, getEffectiveStateLabels(gpuWorkloadConfigContainer, "kubevirt"))
	})
	t.Run("vm-vgpu", func(t *testing.T) {
		require.Equal(t, map[string]string{
			"nvidia.com/gpu.deploy.sandbox-device-plugin": "true",
			"nvidia.com/gpu.deploy.vgpu-manager":          "true",
			"nvidia.com/gpu.deploy.vgpu-device-manager":   "true",
			"nvidia.com/gpu.deploy.sandbox-validator":     "true",
			"nvidia.com/gpu.deploy.cc-manager":            "true",
			"nvidia.com/gpu.deploy.client":                "true",
		}, getEffectiveStateLabels(gpuWorkloadConfigVMVgpu, "kata"))
	})
	t.Run("vm-passthrough-kubevirt", func(t *testing.T) {
		// The map handed back is the package-level one, which never holds the kata key until a
		// kata call adds it, so asserting its absence proves nothing unless it is there first.
		gpuStateLabels[gpuWorkloadConfigVMPassthrough][kataDevicePluginDeployLabelKey] = "true"

		require.Equal(t, map[string]string{
			"nvidia.com/gpu.deploy.sandbox-device-plugin": "true",
			"nvidia.com/gpu.deploy.sandbox-validator":     "true",
			"nvidia.com/gpu.deploy.vfio-manager":          "true",
			"nvidia.com/gpu.deploy.kata-manager":          "true",
			"nvidia.com/gpu.deploy.cc-manager":            "true",
			"nvidia.com/gpu.deploy.client":                "true",
		}, getEffectiveStateLabels(gpuWorkloadConfigVMPassthrough, string(gpuv1.KubeVirt)))
	})
	t.Run("vm-passthrough-kata", func(t *testing.T) {
		require.Equal(t, map[string]string{
			"nvidia.com/gpu.deploy.kata-sandbox-device-plugin": "true",
			"nvidia.com/gpu.deploy.sandbox-validator":          "true",
			"nvidia.com/gpu.deploy.vfio-manager":               "true",
			"nvidia.com/gpu.deploy.kata-manager":               "true",
			"nvidia.com/gpu.deploy.cc-manager":                 "true",
			"nvidia.com/gpu.deploy.client":                     "true",
		}, getEffectiveStateLabels(gpuWorkloadConfigVMPassthrough, string(gpuv1.Kata)))
	})
	t.Run("invalid-config", func(t *testing.T) {
		require.Nil(t, getEffectiveStateLabels("invalid", "kubevirt"))
	})
}

func TestGetEffectiveStateLabelsAliasesThePackageMap(t *testing.T) {
	isolateGPUStateLabels(t)
	// Pinned, not fixed: a production change that clones before editing should fail here and be
	// a deliberate decision rather than a silent one.
	require.Contains(t, gpuStateLabels[gpuWorkloadConfigVMPassthrough], kubevirtDevicePluginDeployLabelKey)

	effectiveStateLabels := getEffectiveStateLabels(gpuWorkloadConfigVMPassthrough, string(gpuv1.Kata))

	require.NotContains(t, gpuStateLabels[gpuWorkloadConfigVMPassthrough], kubevirtDevicePluginDeployLabelKey,
		"the kubevirt key was deleted from the package-level map, not from a copy")
	effectiveStateLabels["probe.nvidia.com/aliasing"] = "true"
	require.Equal(t, "true", gpuStateLabels[gpuWorkloadConfigVMPassthrough]["probe.nvidia.com/aliasing"],
		"writes through the returned map reach the package-level map")
}

func TestRemoveAllGPUStateLabels(t *testing.T) {
	// Without this the kata subtest below silently exercises the wrong code path: if an earlier
	// test left kata-device-plugin in the shared maps, the first loop removes it and the
	// dedicated kata statement never runs.
	isolateGPUStateLabels(t)
	t.Run("removes kata device plugin label", func(t *testing.T) {
		labels := map[string]string{
			kataDevicePluginDeployLabelKey: "true",
			"other":                        "keep",
		}
		labelsModified := removeAllGPUStateLabels(labels)
		require.True(t, labelsModified)
		require.NotContains(t, labels, kataDevicePluginDeployLabelKey)
		require.Equal(t, "keep", labels["other"])
	})
	t.Run("removes kubevirt device plugin label", func(t *testing.T) {
		labels := map[string]string{
			kubevirtDevicePluginDeployLabelKey: "true",
		}
		labelsModified := removeAllGPUStateLabels(labels)
		require.True(t, labelsModified)
		require.NotContains(t, labels, kubevirtDevicePluginDeployLabelKey)
	})
	t.Run("removes GPUCluster deploy labels", func(t *testing.T) {
		labels := map[string]string{
			driverDeployLabelKey:       "true",
			draDriverDeployLabelKey:    "true",
			draValidatorDeployLabelKey: "true",
			gfdDeployLabelKey:          "true",
			dcgmDeployLabelKey:         "true",
			dcgmExporterDeployLabelKey: "true",
			"other":                    "keep",
		}
		labelsModified := removeAllGPUStateLabels(labels)
		require.True(t, labelsModified)
		require.Equal(t, map[string]string{"other": "keep"}, labels)
	})
	t.Run("nothing to remove", func(t *testing.T) {
		labels := map[string]string{"kubernetes.io/hostname": "plain"}
		labelsModified := removeAllGPUStateLabels(labels)
		require.False(t, labelsModified)
		require.Equal(t, map[string]string{"kubernetes.io/hostname": "plain"}, labels)
	})
}

func TestRemoveAllGPUStateLabelsRemovesMIGManager(t *testing.T) {
	// mig-manager is in none of the gpuStateLabels maps, so only its own statement can remove it.
	isolateGPUStateLabels(t)
	labels := map[string]string{
		migManagerLabelKey:       migManagerLabelValue,
		"kubernetes.io/hostname": "node-1",
	}

	labelsModified := removeAllGPUStateLabels(labels)

	require.True(t, labelsModified)
	require.Equal(t, map[string]string{"kubernetes.io/hostname": "node-1"}, labels)
}

func TestRemoveGPUStateLabelsKeepsMIGManagerForContainerConfig(t *testing.T) {
	isolateGPUStateLabels(t)
	workloadConfig := &gpuWorkloadConfiguration{config: gpuWorkloadConfigContainer, node: "node-1", log: logr.Discard()}
	labels := map[string]string{
		migManagerLabelKey:                 migManagerLabelValue,
		kubevirtDevicePluginDeployLabelKey: "true",
		"kubernetes.io/hostname":           "node-1",
	}

	labelsModified := workloadConfig.removeGPUStateLabels(labels)

	require.True(t, labelsModified)
	require.Equal(t, migManagerLabelValue, labels[migManagerLabelKey])
	require.NotContains(t, labels, kubevirtDevicePluginDeployLabelKey)
	require.Equal(t, "node-1", labels["kubernetes.io/hostname"])
}
