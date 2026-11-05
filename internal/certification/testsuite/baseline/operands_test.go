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

package baseline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"

	"github.com/NVIDIA/gpu-operator/internal/certification"
)

type fakeOperandVerificationClient struct {
	clusterPolicy    *nvidiav1.ClusterPolicy
	clusterPolicyErr error
	hasDriverPods    bool
	readyAppLabels   map[string]bool
	waitedAppLabels  []string
}

func (c *fakeOperandVerificationClient) GetClusterPolicy(context.Context) (*nvidiav1.ClusterPolicy, error) {
	return c.clusterPolicy, c.clusterPolicyErr
}

func (c *fakeOperandVerificationClient) HasDriverPods(context.Context, string) bool {
	return c.hasDriverPods
}

func (c *fakeOperandVerificationClient) WaitForPodsReady(context.Context, string, string, time.Duration) error {
	return nil
}

func (c *fakeOperandVerificationClient) WaitForPodReady(_ context.Context, _ string, appLabel string, _ time.Duration) error {
	c.waitedAppLabels = append(c.waitedAppLabels, appLabel)
	if !c.readyAppLabels[appLabel] {
		return context.DeadlineExceeded
	}
	return nil
}

func TestOperandsRun(t *testing.T) {
	allOperandsReady := map[string]bool{
		"nvidia-container-toolkit-daemonset": true,
		"nvidia-device-plugin-daemonset":     true,
		"nvidia-dcgm-exporter":               true,
		"gpu-feature-discovery":              true,
		"nvidia-operator-validator":          true,
	}

	testCases := map[string]struct {
		clusterPolicySpec       nvidiav1.ClusterPolicySpec
		clusterPolicyErr        error
		hasDriverPods           bool
		readyAppLabels          map[string]bool
		expectedStatus          certification.Status
		expectedError           string
		expectedWaitedAppLabels []string
		expectedDetails         []string
	}{
		"all operands enabled by default and ready": {
			readyAppLabels: allOperandsReady,
			expectedStatus: certification.StatusPassed,
			expectedWaitedAppLabels: []string{
				"nvidia-container-toolkit-daemonset",
				"nvidia-device-plugin-daemonset",
				"nvidia-dcgm-exporter",
				"gpu-feature-discovery",
				"nvidia-operator-validator",
			},
			expectedDetails: []string{
				"→ driver pods: skipped (no operator-managed driver pods found)",
				"→ nvidia-container-toolkit-daemonset: ready",
				"→ nvidia-device-plugin-daemonset: ready",
				"→ nvidia-dcgm-exporter: ready",
				"→ gpu-feature-discovery: ready",
				"→ nvidia-operator-validator: ready",
			},
		},
		"driver pods present and ready": {
			hasDriverPods:  true,
			readyAppLabels: allOperandsReady,
			expectedStatus: certification.StatusPassed,
			expectedWaitedAppLabels: []string{
				"nvidia-container-toolkit-daemonset",
				"nvidia-device-plugin-daemonset",
				"nvidia-dcgm-exporter",
				"gpu-feature-discovery",
				"nvidia-operator-validator",
			},
			expectedDetails: []string{
				"→ driver pods: ready",
				"→ nvidia-container-toolkit-daemonset: ready",
				"→ nvidia-device-plugin-daemonset: ready",
				"→ nvidia-dcgm-exporter: ready",
				"→ gpu-feature-discovery: ready",
				"→ nvidia-operator-validator: ready",
			},
		},
		"toolkit disabled is skipped and remaining operands pass": {
			clusterPolicySpec: nvidiav1.ClusterPolicySpec{
				Toolkit: nvidiav1.ToolkitSpec{Enabled: new(false)},
			},
			readyAppLabels: map[string]bool{
				"nvidia-device-plugin-daemonset": true,
				"nvidia-dcgm-exporter":           true,
				"gpu-feature-discovery":          true,
				"nvidia-operator-validator":      true,
			},
			expectedStatus: certification.StatusPassed,
			expectedWaitedAppLabels: []string{
				"nvidia-device-plugin-daemonset",
				"nvidia-dcgm-exporter",
				"gpu-feature-discovery",
				"nvidia-operator-validator",
			},
			expectedDetails: []string{
				"→ driver pods: skipped (no operator-managed driver pods found)",
				"→ nvidia-container-toolkit-daemonset: skipped (disabled in ClusterPolicy)",
				"→ nvidia-device-plugin-daemonset: ready",
				"→ nvidia-dcgm-exporter: ready",
				"→ gpu-feature-discovery: ready",
				"→ nvidia-operator-validator: ready",
			},
		},
		"toolkit and dcgm exporter disabled for host-installed stack": {
			clusterPolicySpec: nvidiav1.ClusterPolicySpec{
				Driver:       nvidiav1.DriverSpec{Enabled: new(false)},
				Toolkit:      nvidiav1.ToolkitSpec{Enabled: new(false)},
				DCGMExporter: nvidiav1.DCGMExporterSpec{Enabled: new(false)},
				DCGM:         nvidiav1.DCGMSpec{Enabled: new(false)},
			},
			readyAppLabels: map[string]bool{
				"nvidia-device-plugin-daemonset": true,
				"gpu-feature-discovery":          true,
				"nvidia-operator-validator":      true,
			},
			expectedStatus: certification.StatusPassed,
			expectedWaitedAppLabels: []string{
				"nvidia-device-plugin-daemonset",
				"gpu-feature-discovery",
				"nvidia-operator-validator",
			},
			expectedDetails: []string{
				"→ driver pods: skipped (no operator-managed driver pods found)",
				"→ nvidia-container-toolkit-daemonset: skipped (disabled in ClusterPolicy)",
				"→ nvidia-device-plugin-daemonset: ready",
				"→ nvidia-dcgm-exporter: skipped (disabled in ClusterPolicy)",
				"→ gpu-feature-discovery: ready",
				"→ nvidia-operator-validator: ready",
			},
		},
		"device plugin and gfd disabled are skipped": {
			clusterPolicySpec: nvidiav1.ClusterPolicySpec{
				DevicePlugin:        nvidiav1.DevicePluginSpec{Enabled: new(false)},
				GPUFeatureDiscovery: nvidiav1.GPUFeatureDiscoverySpec{Enabled: new(false)},
			},
			readyAppLabels: allOperandsReady,
			expectedStatus: certification.StatusPassed,
			expectedWaitedAppLabels: []string{
				"nvidia-container-toolkit-daemonset",
				"nvidia-dcgm-exporter",
				"nvidia-operator-validator",
			},
			expectedDetails: []string{
				"→ driver pods: skipped (no operator-managed driver pods found)",
				"→ nvidia-container-toolkit-daemonset: ready",
				"→ nvidia-device-plugin-daemonset: skipped (disabled in ClusterPolicy)",
				"→ nvidia-dcgm-exporter: ready",
				"→ gpu-feature-discovery: skipped (disabled in ClusterPolicy)",
				"→ nvidia-operator-validator: ready",
			},
		},
		"explicitly enabled operand that is not ready fails": {
			clusterPolicySpec: nvidiav1.ClusterPolicySpec{
				Toolkit: nvidiav1.ToolkitSpec{Enabled: new(true)},
			},
			readyAppLabels:          map[string]bool{},
			expectedStatus:          certification.StatusFailed,
			expectedError:           "operand nvidia-container-toolkit-daemonset: context deadline exceeded",
			expectedWaitedAppLabels: []string{"nvidia-container-toolkit-daemonset"},
			expectedDetails: []string{
				"→ driver pods: skipped (no operator-managed driver pods found)",
			},
		},
		"unreadable ClusterPolicy fails without waiting for operands": {
			clusterPolicyErr: errors.New("no ClusterPolicy resource found"),
			readyAppLabels:   allOperandsReady,
			expectedStatus:   certification.StatusFailed,
			expectedError:    "failed to get ClusterPolicy: no ClusterPolicy resource found",
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			client := &fakeOperandVerificationClient{
				clusterPolicy:    &nvidiav1.ClusterPolicy{Spec: testCase.clusterPolicySpec},
				clusterPolicyErr: testCase.clusterPolicyErr,
				hasDriverPods:    testCase.hasDriverPods,
				readyAppLabels:   testCase.readyAppLabels,
			}

			result := (&Operands{}).run(t.Context(), client, certification.Config{})

			require.NotNil(t, result)
			require.Equal(t, testCase.expectedStatus, result.Status)
			assert.Equal(t, testCase.expectedError, result.Error)
			assert.Equal(t, testCase.expectedWaitedAppLabels, client.waitedAppLabels)
			assert.Equal(t, testCase.expectedDetails, result.Details)
		})
	}
}
