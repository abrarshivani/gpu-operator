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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newControllerWithNamespaces(t *testing.T, interceptors interceptor.Funcs, namespaces ...*corev1.Namespace) ClusterPolicyController {
	t.Helper()
	return newClusterPolicyController(newCoreV1Client(t, interceptors, asClientObjects(namespaces...)...))
}

func TestSetPodSecurityLabelsForNamespaceSkipsUnsuggestedOpenShiftNamespace(t *testing.T) {
	withOperatorNamespace(t, "some-other-namespace")
	// The namespace may be shared with untrusted operators outside the suggested namespace, so
	// the labels must not be applied. Rejecting the Get pins that the function returns first.
	controller := newControllerWithNamespaces(t, failTestOnGet(t))
	controller.openshift = "4.16"

	require.NoError(t, controller.setPodSecurityLabelsForNamespace())
}

func TestSetPodSecurityLabelsForNamespaceGetError(t *testing.T) {
	withOperatorNamespace(t, "gpu-operator")
	controller := newControllerWithNamespaces(t, failGet(errors.New("get failed")))

	err := controller.setPodSecurityLabelsForNamespace()

	// The cause is formatted with %v, so only the text survives.
	require.ErrorContains(t, err, "could not get Namespace gpu-operator from client")
	require.ErrorContains(t, err, "get failed")
}

func TestSetPodSecurityLabelsForNamespaceAlreadyLabeledSkipsPatch(t *testing.T) {
	withOperatorNamespace(t, "gpu-operator")
	controller := newControllerWithNamespaces(t, failTestOnPatch(t), newNamespace("gpu-operator", managedPodSecurityLabels()))

	require.NoError(t, controller.setPodSecurityLabelsForNamespace())
}

func TestSetPodSecurityLabelsForNamespaceWrites(t *testing.T) {
	testCases := []struct {
		name           string
		existingLabels map[string]string
	}{
		{
			// K8s < 1.21 does not auto-label namespaces, so the map can arrive nil.
			name:           "namespace without any labels",
			existingLabels: nil,
		},
		{
			name:           "namespace keeping an unrelated label",
			existingLabels: map[string]string{"kubernetes.io/metadata.name": "gpu-operator"},
		},
		{
			name: "namespace with one pod security mode at the wrong level",
			existingLabels: map[string]string{
				podSecurityLabelPrefix + "enforce": "baseline",
				podSecurityLabelPrefix + "audit":   podSecurityLevelPrivileged,
				podSecurityLabelPrefix + "warn":    podSecurityLevelPrivileged,
			},
		},
		{
			// enforce-version shares the prefix but is not one of the three managed modes,
			// so the operator must leave it exactly as it found it.
			name: "namespace keeping a pod security version label it does not manage",
			existingLabels: map[string]string{
				podSecurityLabelPrefix + "enforce-version": "v1.31",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			withOperatorNamespace(t, "gpu-operator")
			controller := newControllerWithNamespaces(t, interceptor.Funcs{}, newNamespace("gpu-operator", tc.existingLabels))

			require.NoError(t, controller.setPodSecurityLabelsForNamespace())

			storedNamespace := getNamespace(t, controller.client, "gpu-operator")
			requirePrivilegedPodSecurityLabels(t, storedNamespace)
			for key, value := range tc.existingLabels {
				if _, isManaged := managedPodSecurityLabels()[key]; !isManaged {
					require.Equal(t, value, storedNamespace.Labels[key], "unrelated label %s", key)
				}
			}
		})
	}
}

func TestSetPodSecurityLabelsForNamespacePatchError(t *testing.T) {
	withOperatorNamespace(t, "gpu-operator")
	controller := newControllerWithNamespaces(t, failPatch(errors.New("patch failed")), newNamespace("gpu-operator", nil))

	err := controller.setPodSecurityLabelsForNamespace()

	require.ErrorContains(t, err, "unable to label namespace gpu-operator with pod security levels")
	require.ErrorContains(t, err, "patch failed")
}

func TestOCPEnsureNamespaceMonitoringSkipsUnsuggestedNamespace(t *testing.T) {
	withOperatorNamespace(t, "some-other-namespace")
	// Outside the suggested namespace the operator must not enable cluster monitoring, because
	// the namespace may be shared with untrusted operators. Nothing may even be read.
	controller := newControllerWithNamespaces(t, failTestOnGet(t))

	require.NoError(t, controller.ocpEnsureNamespaceMonitoring())
}

func TestOCPEnsureNamespaceMonitoringGetError(t *testing.T) {
	withOperatorNamespace(t, ocpSuggestedNamespace)
	controller := newControllerWithNamespaces(t, failGet(errors.New("get failed")))

	err := controller.ocpEnsureNamespaceMonitoring()

	require.ErrorContains(t, err, "could not get Namespace "+ocpSuggestedNamespace+" from client")
	require.ErrorContains(t, err, "get failed")
}

func TestOCPEnsureNamespaceMonitoringHonorsExistingLabel(t *testing.T) {
	testCases := []struct {
		name               string
		existingLabelValue string
	}{
		{name: "monitoring already enabled", existingLabelValue: ocpNamespaceMonitoringLabelValue},
		// An explicit "false" is a deliberate user opt-out and must survive reconciliation.
		{name: "monitoring explicitly disabled by the user", existingLabelValue: "false"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			withOperatorNamespace(t, ocpSuggestedNamespace)
			namespace := newNamespace(ocpSuggestedNamespace, map[string]string{ocpNamespaceMonitoringLabelKey: tc.existingLabelValue})
			controller := newControllerWithNamespaces(t, failTestOnPatch(t), namespace)

			require.NoError(t, controller.ocpEnsureNamespaceMonitoring())

			storedNamespace := getNamespace(t, controller.client, ocpSuggestedNamespace)
			require.Equal(t, tc.existingLabelValue, storedNamespace.Labels[ocpNamespaceMonitoringLabelKey])
		})
	}
}

func TestOCPEnsureNamespaceMonitoringEnablesWhenLabelAbsent(t *testing.T) {
	// Both names are spelled out rather than taken from the constants production reads: the
	// namespace is the one published in the ClusterServiceVersion suggested-namespace
	// annotation, and the label key is OpenShift's own, so neither is ours to redefine.
	withOperatorNamespace(t, "nvidia-gpu-operator")
	// The namespace carries a label map but not the monitoring key. A namespace with no labels
	// at all would panic here, because unlike setPodSecurityLabelsForNamespace this function
	// assigns into ns.Labels without initializing a nil map. That is unreachable on Kubernetes
	// >= 1.21, which stamps kubernetes.io/metadata.name onto every namespace; the fake client
	// does not, so a fixture must supply a label map of its own.
	namespace := newNamespace("nvidia-gpu-operator", map[string]string{"kubernetes.io/metadata.name": "nvidia-gpu-operator"})
	controller := newControllerWithNamespaces(t, interceptor.Funcs{}, namespace)

	require.NoError(t, controller.ocpEnsureNamespaceMonitoring())

	storedNamespace := getNamespace(t, controller.client, "nvidia-gpu-operator")
	require.Equal(t, ocpNamespaceMonitoringLabelValue, storedNamespace.Labels["openshift.io/cluster-monitoring"])
	require.Equal(t, "nvidia-gpu-operator", storedNamespace.Labels["kubernetes.io/metadata.name"])
}

func TestOCPEnsureNamespaceMonitoringPatchError(t *testing.T) {
	withOperatorNamespace(t, ocpSuggestedNamespace)
	namespace := newNamespace(ocpSuggestedNamespace, map[string]string{"kubernetes.io/metadata.name": ocpSuggestedNamespace})
	controller := newControllerWithNamespaces(t, failPatch(errors.New("patch failed")), namespace)

	err := controller.ocpEnsureNamespaceMonitoring()

	require.ErrorContains(t, err, "unable to label namespace "+ocpSuggestedNamespace+" for the GPU Operator monitoring")
	require.ErrorContains(t, err, "patch failed")
}
