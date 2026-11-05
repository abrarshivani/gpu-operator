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
	"strings"
	"time"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"

	"github.com/NVIDIA/gpu-operator/internal/certification"
	"github.com/NVIDIA/gpu-operator/internal/certification/k8s"
)

const (
	dcgmExporterServiceName = "nvidia-dcgm-exporter"
	dcgmExporterPort        = 9400
	dcgmMetricsPath         = "/metrics"
	dcgmMetricPrefix        = "DCGM_"
)

type DCGMMetrics struct{}

// dcgmMetricsClient is the subset of *k8s.Clients used by dcgm-metrics.
type dcgmMetricsClient interface {
	GetClusterPolicy(ctx context.Context) (*nvidiav1.ClusterPolicy, error)
	WaitForCondition(ctx context.Context, timeout time.Duration, condition func(ctx context.Context) (bool, error)) error
	GetServiceDirect(ctx context.Context, namespace, serviceName string, port int, path string) ([]byte, error)
	ProxyGetService(ctx context.Context, namespace, serviceName string, port int, path string) ([]byte, error)
}

func (d *DCGMMetrics) Name() string {
	return "dcgm-metrics"
}

func (d *DCGMMetrics) Run(ctx context.Context, clients *k8s.Clients, cfg certification.Config) *certification.TestResult {
	return d.run(ctx, clients, cfg)
}

func (d *DCGMMetrics) run(ctx context.Context, clients dcgmMetricsClient, cfg certification.Config) *certification.TestResult {
	result := certification.NewTestResult(d.Name())
	defer result.Finalize()

	clusterPolicy, cpErr := clients.GetClusterPolicy(ctx)
	if cpErr != nil {
		result.Fail("failed to get ClusterPolicy: %v", cpErr)
		return result
	}
	if !clusterPolicy.Spec.DCGMExporter.IsEnabled() {
		result.Skip("DCGM exporter is disabled in ClusterPolicy")
		return result
	}

	result.AddDetail("querying DCGM exporter metrics (service %s:%d)",
		dcgmExporterServiceName, dcgmExporterPort)

	var body []byte
	var lastErr error
	err := clients.WaitForCondition(ctx, 60*time.Second, func(ctx context.Context) (bool, error) {
		// Try direct service DNS first (works in-cluster), fall back to API server proxy.
		data, err := clients.GetServiceDirect(ctx, cfg.Namespace, dcgmExporterServiceName, dcgmExporterPort, dcgmMetricsPath)
		if err != nil {
			data, err = clients.ProxyGetService(ctx, cfg.Namespace, dcgmExporterServiceName, dcgmExporterPort, dcgmMetricsPath)
		}
		if err != nil {
			lastErr = err
			return false, nil // retry
		}
		body = data
		return true, nil
	})
	if err != nil {
		if lastErr != nil {
			result.Fail("failed to query metrics endpoint: %v", lastErr)
		} else {
			result.Fail("failed to query metrics endpoint: %v", err)
		}
		return result
	}

	metricsBody := string(body)

	if !strings.Contains(metricsBody, dcgmMetricPrefix) {
		result.Fail("no DCGM metrics found in response (expected metrics starting with %s)", dcgmMetricPrefix)
		return result
	}

	lines := strings.Split(metricsBody, "\n")
	dcgmMetricCount := 0
	for _, line := range lines {
		if strings.HasPrefix(line, dcgmMetricPrefix) {
			dcgmMetricCount++
		}
	}

	result.AddDetail("found %d DCGM metric lines", dcgmMetricCount)

	sampleCount := 0
	for _, line := range lines {
		if strings.HasPrefix(line, dcgmMetricPrefix) && !strings.HasPrefix(line, "# ") {
			metricWithLabels, _, _ := strings.Cut(line, " ")
			metricName, _, _ := strings.Cut(metricWithLabels, "{")
			result.AddDetail("sample metric: %s", metricName)
			sampleCount++
			if sampleCount >= 3 {
				break
			}
		}
	}

	return result
}
