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

type fakeDCGMMetricsClient struct {
	clusterPolicy     *nvidiav1.ClusterPolicy
	clusterPolicyErr  error
	metricsBody       []byte
	metricsQueryCount int
}

func (c *fakeDCGMMetricsClient) GetClusterPolicy(context.Context) (*nvidiav1.ClusterPolicy, error) {
	return c.clusterPolicy, c.clusterPolicyErr
}

func (c *fakeDCGMMetricsClient) WaitForCondition(ctx context.Context, _ time.Duration, condition func(ctx context.Context) (bool, error)) error {
	done, err := condition(ctx)
	if err != nil {
		return err
	}
	if !done {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *fakeDCGMMetricsClient) GetServiceDirect(context.Context, string, string, int, string) ([]byte, error) {
	c.metricsQueryCount++
	return c.metricsBody, nil
}

func (c *fakeDCGMMetricsClient) ProxyGetService(context.Context, string, string, int, string) ([]byte, error) {
	c.metricsQueryCount++
	return nil, errors.New("proxy not expected")
}

func TestDCGMMetricsRun(t *testing.T) {
	testCases := map[string]struct {
		clusterPolicySpec         nvidiav1.ClusterPolicySpec
		clusterPolicyErr          error
		expectedStatus            certification.Status
		expectedError             string
		expectedMetricsQueryCount int
		expectedDetailsContain    string
	}{
		"dcgm exporter disabled is skipped": {
			clusterPolicySpec: nvidiav1.ClusterPolicySpec{
				DCGMExporter: nvidiav1.DCGMExporterSpec{Enabled: new(false)},
			},
			expectedStatus:         certification.StatusSkipped,
			expectedDetailsContain: "→ DCGM exporter is disabled in ClusterPolicy",
		},
		"dcgm exporter enabled by default queries metrics": {
			expectedStatus:            certification.StatusPassed,
			expectedMetricsQueryCount: 1,
			expectedDetailsContain:    "→ found 1 DCGM metric lines",
		},
		"unreadable ClusterPolicy fails without querying metrics": {
			clusterPolicyErr: errors.New("no ClusterPolicy resource found"),
			expectedStatus:   certification.StatusFailed,
			expectedError:    "failed to get ClusterPolicy: no ClusterPolicy resource found",
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			client := &fakeDCGMMetricsClient{
				clusterPolicy:    &nvidiav1.ClusterPolicy{Spec: testCase.clusterPolicySpec},
				clusterPolicyErr: testCase.clusterPolicyErr,
				metricsBody:      []byte("DCGM_FI_DEV_GPU_TEMP{gpu=\"0\"} 40\n"),
			}

			result := (&DCGMMetrics{}).run(t.Context(), client, certification.Config{})

			require.NotNil(t, result)
			require.Equal(t, testCase.expectedStatus, result.Status)
			assert.Equal(t, testCase.expectedError, result.Error)
			assert.Equal(t, testCase.expectedMetricsQueryCount, client.metricsQueryCount)
			if testCase.expectedDetailsContain != "" {
				assert.Contains(t, result.Details, testCase.expectedDetailsContain)
			}
		})
	}
}
