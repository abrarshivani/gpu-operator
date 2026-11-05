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
	"time"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"

	"github.com/NVIDIA/gpu-operator/internal/certification"
	"github.com/NVIDIA/gpu-operator/internal/certification/k8s"
)

type operandCheck struct {
	appLabel  string
	isEnabled func(spec *nvidiav1.ClusterPolicySpec) bool
}

var operands = []operandCheck{
	{
		appLabel:  "nvidia-container-toolkit-daemonset",
		isEnabled: func(spec *nvidiav1.ClusterPolicySpec) bool { return spec.Toolkit.IsEnabled() },
	},
	{
		appLabel:  "nvidia-device-plugin-daemonset",
		isEnabled: func(spec *nvidiav1.ClusterPolicySpec) bool { return spec.DevicePlugin.IsEnabled() },
	},
	{
		appLabel:  "nvidia-dcgm-exporter",
		isEnabled: func(spec *nvidiav1.ClusterPolicySpec) bool { return spec.DCGMExporter.IsEnabled() },
	},
	{
		appLabel:  "gpu-feature-discovery",
		isEnabled: func(spec *nvidiav1.ClusterPolicySpec) bool { return spec.GPUFeatureDiscovery.IsEnabled() },
	},
	{
		appLabel: "nvidia-operator-validator",
		// The validator has no enabled field; the operator always deploys it.
		isEnabled: func(*nvidiav1.ClusterPolicySpec) bool { return true },
	},
}

// operandVerificationClient is the subset of *k8s.Clients used by operand-verification.
type operandVerificationClient interface {
	GetClusterPolicy(ctx context.Context) (*nvidiav1.ClusterPolicy, error)
	HasDriverPods(ctx context.Context, namespace string) bool
	WaitForPodsReady(ctx context.Context, namespace, labelSelector string, timeout time.Duration) error
	WaitForPodReady(ctx context.Context, namespace, appLabel string, timeout time.Duration) error
}

type Operands struct{}

func (o *Operands) Name() string {
	return "operand-verification"
}

func (o *Operands) Run(ctx context.Context, clients *k8s.Clients, cfg certification.Config) *certification.TestResult {
	return o.run(ctx, clients, cfg)
}

func (o *Operands) run(ctx context.Context, clients operandVerificationClient, cfg certification.Config) *certification.TestResult {
	result := certification.NewTestResult(o.Name())
	defer result.Finalize()

	timeout := cfg.ParseTimeout()

	clusterPolicy, cpErr := clients.GetClusterPolicy(ctx)
	if cpErr != nil {
		result.Fail("failed to get ClusterPolicy: %v", cpErr)
		return result
	}

	// Check driver pods (skip if driver is disabled or pre-installed on host)
	if clients.HasDriverPods(ctx, cfg.Namespace) {
		if err := clients.WaitForPodsReady(ctx, cfg.Namespace, k8s.DriverLabelSelector, timeout); err != nil {
			result.Fail("driver pods: %v", err)
			return result
		}
		result.AddDetail("driver pods: ready")
	} else {
		result.AddDetail("driver pods: skipped (no operator-managed driver pods found)")
	}

	for _, operand := range operands {
		if !operand.isEnabled(&clusterPolicy.Spec) {
			result.AddDetail("%s: skipped (disabled in ClusterPolicy)", operand.appLabel)
			continue
		}
		if err := clients.WaitForPodReady(ctx, cfg.Namespace, operand.appLabel, timeout); err != nil {
			result.Fail("operand %s: %v", operand.appLabel, err)
			return result
		}
		result.AddDetail("%s: ready", operand.appLabel)
	}

	return result
}
