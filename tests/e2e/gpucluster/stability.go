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

package gpucluster

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	e2eoperands "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operands"
	e2eoperator "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operator"
	"github.com/NVIDIA/gpu-operator/tests/e2e/helpers"
)

const (
	draKubeletPluginApp  = "nvidia-dra-driver-kubelet-plugin"
	draControllerApp     = "nvidia-dra-driver-controller"
	draDCGMExporterApp   = "nvidia-dcgm-exporter-dra"
	draValidatorSelector = "app.kubernetes.io/component=nvidia-dra-validator"

	operandStabilityPeriod       = 5 * time.Minute
	operandStabilityPollInterval = 10 * time.Second
)

var operandSelectors = []string{
	helpers.DriverComponentSelector,
	e2eoperands.AppSelector(draKubeletPluginApp),
	e2eoperands.AppSelector(draControllerApp),
	e2eoperands.AppSelector(draDCGMExporterApp),
	draValidatorSelector,
}

func expectOperandsStable(
	ctx context.Context,
	clients e2eoperator.Clients,
	restartCountsAtBringUp map[operandContainer]int32,
) {
	ginkgo.GinkgoHelper()
	ginkgo.By(fmt.Sprintf("Watching the operands for restarts or replaced pods over %s", operandStabilityPeriod))
	gomega.Consistently(ctx, func(g gomega.Gomega) error {
		currentRestartCounts, err := operandRestartCounts(ctx, clients)
		if err != nil {
			return gomega.TryAgainAfter(framework.PollInterval).Wrap(fmt.Errorf("failed to read the operand restart counts: %w", err))
		}
		g.Expect(describeOperandChanges(restartCountsAtBringUp, currentRestartCounts)).To(gomega.BeEmpty(),
			"an operand container restarted or its pod was replaced")
		return nil
	}).WithTimeout(operandStabilityPeriod).WithPolling(operandStabilityPollInterval).Should(gomega.Succeed())
	ginkgo.GinkgoWriter.Printf("No container restarted and no pod was replaced over %s\n", operandStabilityPeriod)
}

func describeOperandChanges(restartCountsAtBringUp, currentRestartCounts map[operandContainer]int32) []string {
	var changeDescriptions []string
	for container, restartCountAtBringUp := range restartCountsAtBringUp {
		currentRestartCount, found := currentRestartCounts[container]
		switch {
		case !found:
			changeDescriptions = append(changeDescriptions, fmt.Sprintf("pod %s container %s is missing",
				container.podName, container.containerName))
		case currentRestartCount != restartCountAtBringUp:
			changeDescriptions = append(changeDescriptions, fmt.Sprintf("pod %s container %s restarted (%d -> %d)",
				container.podName, container.containerName, restartCountAtBringUp, currentRestartCount))
		}
	}
	for container := range currentRestartCounts {
		if _, recordedAtBringUp := restartCountsAtBringUp[container]; !recordedAtBringUp {
			changeDescriptions = append(changeDescriptions, fmt.Sprintf("pod %s container %s appeared after bring-up",
				container.podName, container.containerName))
		}
	}
	slices.Sort(changeDescriptions)
	return changeDescriptions
}

type operandContainer struct {
	podName       string
	podUID        types.UID
	containerName string
}

func operandRestartCounts(ctx context.Context, clients e2eoperator.Clients) (map[operandContainer]int32, error) {
	restartCountByContainer := map[operandContainer]int32{}
	for _, selector := range operandSelectors {
		pods, err := clients.Pods.List(ctx, e2eoperator.TestContext.OperatorNamespace, selector)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			for _, containerStatus := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
				container := operandContainer{podName: pod.Name, podUID: pod.UID, containerName: containerStatus.Name}
				restartCountByContainer[container] = containerStatus.RestartCount
			}
		}
	}
	return restartCountByContainer, nil
}
