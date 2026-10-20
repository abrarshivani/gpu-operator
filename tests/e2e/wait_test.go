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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	pollInterval = 5 * time.Second

	operandReadyTimeout    = 45 * time.Minute
	podDeletionTimeout     = 45 * time.Minute
	driverUpgradeTimeout   = 45 * time.Minute
	gpuWorkloadTimeout     = 45 * time.Minute
	operatorRestartTimeout = 100 * time.Second
	helmInstallTimeout     = 5 * time.Minute
)

func waitFor(ctx context.Context, timeout time.Duration, description string, check func(g gomega.Gomega)) {
	ginkgo.GinkgoHelper()
	ginkgo.By(description)
	gomega.Eventually(ctx, check).WithTimeout(timeout).WithPolling(pollInterval).Should(gomega.Succeed())
}

func waitForPodNotFound(ctx context.Context, clients clusterClients, namespace, podName string, timeout time.Duration) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, timeout, fmt.Sprintf("waiting for pod %s/%s to be deleted", namespace, podName), func(g gomega.Gomega) {
		_, err := clients.kube.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		g.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "a NotFound error"))
	})
}
