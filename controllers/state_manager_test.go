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
	"maps"
	"testing"

	"github.com/go-logr/logr"
	promcli "github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apiimagev1 "github.com/openshift/api/image/v1"
)

// Snapshotting on test entry would be unsafe: getEffectiveStateLabels edits the nested maps in
// place, so an earlier test may already have corrupted them. Package init is the only point
// guaranteed to run first.
var pristineGPUStateLabels = deepCopyStateLabels(gpuStateLabels)

func deepCopyStateLabels(source map[string]map[string]string) map[string]map[string]string {
	copiedStateLabels := make(map[string]map[string]string, len(source))
	for config, stateLabels := range source {
		copiedStateLabels[config] = maps.Clone(stateLabels)
	}
	return copiedStateLabels
}

func isolateGPUStateLabels(t *testing.T) {
	t.Helper()
	resetGPUStateLabels()
	t.Cleanup(resetGPUStateLabels)
}

func resetGPUStateLabels() {
	for config := range gpuStateLabels {
		if _, isPristine := pristineGPUStateLabels[config]; !isPristine {
			delete(gpuStateLabels, config)
		}
	}
	for config, stateLabels := range pristineGPUStateLabels {
		gpuStateLabels[config] = maps.Clone(stateLabels)
	}
}

// ClusterPolicyController.init reassigns this global from Spec.SandboxWorkloads.DefaultWorkload.
var pristineDefaultGPUWorkloadConfig = defaultGPUWorkloadConfig

func isolateDefaultGPUWorkloadConfig(t *testing.T) {
	t.Helper()
	defaultGPUWorkloadConfig = pristineDefaultGPUWorkloadConfig
	t.Cleanup(func() { defaultGPUWorkloadConfig = pristineDefaultGPUWorkloadConfig })
}

// setPodSecurityLabelsForNamespace and ocpEnsureNamespaceMonitoring read the package-level
// clusterPolicyCtrl, not their receiver, so setting only the receiver exercises nothing. The
// two are the same object in production, making this a testability constraint, not a live bug.
func withOperatorNamespace(t *testing.T, namespace string) {
	t.Helper()
	previousNamespace := clusterPolicyCtrl.operatorNamespace
	clusterPolicyCtrl.operatorNamespace = namespace
	t.Cleanup(func() { clusterPolicyCtrl.operatorNamespace = previousNamespace })
}

func newTestScheme(t *testing.T, addToSchemeFuncs ...func(*runtime.Scheme) error) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	for _, addToScheme := range addToSchemeFuncs {
		require.NoError(t, addToScheme(scheme))
	}
	return scheme
}

func asClientObjects[T ctrlclient.Object](objects ...T) []ctrlclient.Object {
	clientObjects := make([]ctrlclient.Object, 0, len(objects))
	for _, object := range objects {
		clientObjects = append(clientObjects, object)
	}
	return clientObjects
}

func newCoreV1Client(t *testing.T, interceptors interceptor.Funcs, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithInterceptorFuncs(interceptors).
		WithObjects(objects...).
		Build()
}

func newNodeClient(t *testing.T, interceptors interceptor.Funcs, nodes ...*corev1.Node) ctrlclient.Client {
	t.Helper()
	return newCoreV1Client(t, interceptors, asClientObjects(nodes...)...)
}

func newClusterPolicyController(client ctrlclient.Client) ClusterPolicyController {
	return ClusterPolicyController{ctx: context.Background(), client: client, logger: logr.Discard()}
}

func newNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func gpuNodeWithOSLabels(name, osRelease, osVersion string) *corev1.Node {
	return nodeWithLabels(name, map[string]string{
		commonGPULabelKey:      commonGPULabelValue,
		nfdOSReleaseIDLabelKey: osRelease,
		nfdOSVersionIDLabelKey: osVersion,
	})
}

func getNamespace(t *testing.T, client ctrlclient.Client, name string) *corev1.Namespace {
	t.Helper()
	storedNamespace := &corev1.Namespace{}
	require.NoError(t, client.Get(context.Background(), ctrlclient.ObjectKey{Name: name}, storedNamespace))
	return storedNamespace
}

// Spelled out rather than built from podSecurityModes and podSecurityLabelPrefix: deriving the
// expectation from the same constants the production loop reads would make it self-referential,
// and every one of those constants is an upstream Kubernetes Pod Security Admission name that
// the operator does not get to redefine.
func managedPodSecurityLabels() map[string]string {
	return map[string]string{
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/audit":   "privileged",
		"pod-security.kubernetes.io/warn":    "privileged",
	}
}

func requirePrivilegedPodSecurityLabels(t *testing.T, namespace *corev1.Namespace) {
	t.Helper()
	require.Subset(t, namespace.Labels, managedPodSecurityLabels())
}

// Embeds a working client rather than a nil one: a caller reaching for any method other than
// List would otherwise nil-panic instead of reporting what it tried to do.
type errorListClient struct {
	ctrlclient.Client
	err error
}

func newErrorListClient(t *testing.T, err error) errorListClient {
	t.Helper()
	return errorListClient{Client: newCoreV1Client(t, interceptor.Funcs{}), err: err}
}

func (c errorListClient) List(_ context.Context, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
	return c.err
}

// Delegates rather than swallowing the Patch: a test that re-reads the object afterwards would
// otherwise assert against state no write could ever change.
func failTestOnPatch(t *testing.T) interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, patch ctrlclient.Patch, opts ...ctrlclient.PatchOption) error {
			t.Errorf("unexpected Patch of %q; the caller should have returned before writing", obj.GetName())
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

// Delegates rather than swallowing the Get, which would hand the caller a zero-valued object
// and turn a reported failure into a downstream nil-map panic.
func failTestOnGet(t *testing.T) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			t.Errorf("unexpected Get of %q; the caller should have returned before reading", key.Name)
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

func failPatch(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(_ context.Context, _ ctrlclient.WithWatch, _ ctrlclient.Object, _ ctrlclient.Patch, _ ...ctrlclient.PatchOption) error {
			return err
		},
	}
}

func failGet(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(_ context.Context, _ ctrlclient.WithWatch, _ ctrlclient.ObjectKey, _ ctrlclient.Object, _ ...ctrlclient.GetOption) error {
			return err
		},
	}
}

func failImageStreamGet(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if _, isImageStream := obj.(*apiimagev1.ImageStream); isImageStream {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// Exists because prometheus/testutil is not vendored, and vendoring it would add a non-test
// file to a tests-only change. Mirrors countingCounter in clusterpolicy_controller_test.go.
type recordingGauge struct {
	promcli.Gauge
	t        *testing.T
	value    float64
	setCount int
}

func (g *recordingGauge) Set(value float64) {
	g.value = value
	g.setCount++
	g.Gauge.Set(value)
}

// Only Set feeds value and setCount, so a production switch to any other mutator would slip
// past every assertion made against them, including the ones asserting a gauge was untouched.
func (g *recordingGauge) rejectMutator(mutatorName string) {
	g.t.Errorf("gauge mutated through %s; only Set is recorded, so the assertions would not see it", mutatorName)
}

func (g *recordingGauge) Inc()              { g.rejectMutator("Inc") }
func (g *recordingGauge) Dec()              { g.rejectMutator("Dec") }
func (g *recordingGauge) Add(_ float64)     { g.rejectMutator("Add") }
func (g *recordingGauge) Sub(_ float64)     { g.rejectMutator("Sub") }
func (g *recordingGauge) SetToCurrentTime() { g.rejectMutator("SetToCurrentTime") }

func newRecordingGauge(t *testing.T) *recordingGauge {
	t.Helper()
	return &recordingGauge{Gauge: promcli.NewGauge(promcli.GaugeOpts{}), t: t}
}

type stateManagerMetrics struct {
	operatorMetrics                 *OperatorMetrics
	gpuNodesTotal                   *recordingGauge
	driverToolkitEnabled            *recordingGauge
	driverToolkitImageStreamMissing *recordingGauge
	driverToolkitNfdTooOld          *recordingGauge
}

// Deliberately does not call InitOperatorMetrics: that registers into the global
// controller-runtime registry and panics on a second call per test binary.
func newStateManagerMetrics(t *testing.T) *stateManagerMetrics {
	t.Helper()
	metrics := &stateManagerMetrics{
		gpuNodesTotal:                   newRecordingGauge(t),
		driverToolkitEnabled:            newRecordingGauge(t),
		driverToolkitImageStreamMissing: newRecordingGauge(t),
		driverToolkitNfdTooOld:          newRecordingGauge(t),
	}
	metrics.operatorMetrics = &OperatorMetrics{
		gpuNodesTotal:                   metrics.gpuNodesTotal,
		openshiftDriverToolkitEnabled:   metrics.driverToolkitEnabled,
		openshiftDriverToolkitIsMissing: metrics.driverToolkitImageStreamMissing,
		openshiftDriverToolkitNfdTooOld: metrics.driverToolkitNfdTooOld,
		// No test asserts this one, but ocpHasDriverToolkitImageStream calls Set on it
		// whenever the ImageStream is found, and a nil gauge would panic there.
		openshiftDriverToolkitIsBroken: promcli.NewGauge(promcli.GaugeOpts{}),
	}
	return metrics
}
