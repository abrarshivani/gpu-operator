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
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	e2eoperator "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operator"
	"github.com/NVIDIA/gpu-operator/tests/e2e/helpers"
)

func expectNoClusterPolicyOrGPUCluster(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoHelper()
	ginkgo.By("Checking that no ClusterPolicy or GPUCluster is left from an earlier install")
	var leftoverObjectDescriptions []string
	clusterPolicies, err := clients.ClusterPolicies.List(ctx)
	if !apierrors.IsNotFound(err) {
		framework.ExpectNoError(err, "failed to list ClusterPolicies")
		for _, clusterPolicy := range clusterPolicies.Items {
			leftoverObjectDescriptions = append(leftoverObjectDescriptions, describeLeftoverObject("ClusterPolicy", &clusterPolicy))
		}
	}
	gpuClusters, err := clients.GPUClusters.List(ctx)
	if !apierrors.IsNotFound(err) {
		framework.ExpectNoError(err, "failed to list GPUClusters")
		for _, gpuCluster := range gpuClusters.Items {
			leftoverObjectDescriptions = append(leftoverObjectDescriptions, describeLeftoverObject("GPUCluster", &gpuCluster))
		}
	}
	if len(leftoverObjectDescriptions) > 0 {
		ginkgo.Fail(strings.Join(leftoverObjectDescriptions, "\n"))
	}
	ginkgo.GinkgoWriter.Printf("Found no ClusterPolicy or GPUCluster\n")
}

func describeLeftoverObject(kind string, object metav1.Object) string {
	leftoverObjectDescription := fmt.Sprintf("%s %s is left from an earlier install", kind, object.GetName())
	deletionStateDescription := describeDeletionState(object)
	if deletionStateDescription == "" {
		return leftoverObjectDescription
	}
	return fmt.Sprintf("%s (%s)", leftoverObjectDescription, deletionStateDescription)
}

func describeDeletionState(object metav1.Object) string {
	var deletionStateDescriptions []string
	if deletionTimestamp := object.GetDeletionTimestamp(); deletionTimestamp != nil {
		deletionStateDescriptions = append(deletionStateDescriptions, "deletionTimestamp "+deletionTimestamp.UTC().Format(time.RFC3339))
	}
	if finalizers := object.GetFinalizers(); len(finalizers) > 0 {
		deletionStateDescriptions = append(deletionStateDescriptions, fmt.Sprintf("finalizers %v", finalizers))
	}
	return strings.Join(deletionStateDescriptions, ", ")
}

func waitForOperatorNamespaceDeleted(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoHelper()
	operatorNamespace := e2eoperator.TestContext.OperatorNamespace
	framework.WaitForNotFound(ctx, framework.DefaultNamespaceDeletionTimeout, "Namespace "+operatorNamespace, func(ctx context.Context) error {
		_, err := clients.Kube.CoreV1().Namespaces().Get(ctx, operatorNamespace, metav1.GetOptions{})
		return err
	})
}

func daemonSetNames(daemonSets []appsv1.DaemonSet) []string {
	names := make([]string, 0, len(daemonSets))
	for _, daemonSet := range daemonSets {
		names = append(names, daemonSet.Name)
	}
	return names
}

func podStatusSummaries(pods []corev1.Pod) []string {
	summaries := make([]string, 0, len(pods))
	for _, pod := range pods {
		summaries = append(summaries, helpers.PodStatusSummary(&pod))
	}
	return summaries
}
