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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apiimagev1 "github.com/openshift/api/image/v1"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
)

// KUBECONFIG is the only seam available without changing signatures: the callers build their
// own clients from the ambient kubeconfig and take no injectable dependency.
func serveKubernetesAPI(t *testing.T, handlers map[string]http.HandlerFunc) {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, handler := range handlers {
		mux.HandleFunc(pattern, handler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	writeKubeconfig(t, "server: "+server.URL)
}

func writeKubeconfig(t *testing.T, clusterFields string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    %s
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`, clusterFields)
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	t.Setenv("KUBECONFIG", path)
}

// An unregistered auth provider is one of the few ways to fail the client constructor and not
// clientcmd: a bad CA path or an unparseable proxy-url is rejected earlier, and GetConfigOrDie
// answers that by exiting the process rather than returning an error a test could assert on.
func writeUnbuildableKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	contents := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    auth-provider:
      name: unregistered-auth-provider
`
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	t.Setenv("KUBECONFIG", path)
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const (
	clusterVersionPath = "/apis/config.openshift.io/v1/clusterversions/version"
	clusterProxyPath   = "/apis/config.openshift.io/v1/proxies/cluster"
	serverVersionPath  = "/version"

	notFoundStatusBody      = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
	internalErrorStatusBody = `{"kind":"Status","status":"Failure","code":500}`
)

func clusterVersionBody(history string) string {
	return `{"apiVersion":"config.openshift.io/v1","kind":"ClusterVersion","metadata":{"name":"version"},"status":{"history":[` + history + `]}}`
}

func serverVersionBody(gitVersion string) string {
	return `{"major":"1","minor":"31","gitVersion":"` + gitVersion + `"}`
}

// An absent ClusterVersion resource is how init distinguishes vanilla Kubernetes from
// OpenShift; it tolerates the NotFound rather than failing on it.
func serveVanillaKubernetesAPI(t *testing.T, gitVersion string) {
	t.Helper()
	serveKubernetesAPI(t, map[string]http.HandlerFunc{
		clusterVersionPath: jsonHandler(http.StatusNotFound, notFoundStatusBody),
		serverVersionPath:  jsonHandler(http.StatusOK, serverVersionBody(gitVersion)),
	})
}

// Needed to reach the driver-toolkit and initOCPParams branches, which are gated on a non-empty
// n.openshift.
func serveOpenShiftAPI(t *testing.T) {
	t.Helper()
	serveKubernetesAPI(t, map[string]http.HandlerFunc{
		clusterVersionPath: jsonHandler(http.StatusOK, clusterVersionBody(`{"state":"Completed","version":"4.16.9"}`)),
		serverVersionPath:  jsonHandler(http.StatusOK, serverVersionBody("v1.31.4")),
	})
}

func newInitReconciler(t *testing.T, interceptors interceptor.Funcs, objects ...ctrlclient.Object) *ClusterPolicyReconciler {
	t.Helper()
	// initOCPParams reads the driver-toolkit ImageStream on OpenShift, so the type has to be
	// registered even for the tests that never create one.
	scheme := newTestScheme(t, gpuv1.AddToScheme, apiimagev1.AddToScheme)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptors).
		WithObjects(objects...).
		Build()
	return &ClusterPolicyReconciler{
		Client:    client,
		Log:       logr.Discard(),
		Scheme:    scheme,
		Namespace: "gpu-operator",
	}
}

func newInitController(t *testing.T) (*ClusterPolicyController, *stateManagerMetrics) {
	t.Helper()
	// Seed a value init must overwrite with the reconciler's namespace. Seeding the expected
	// value would make the assertion pass even if init never wrote it.
	withOperatorNamespace(t, "stale-namespace")
	isolateDefaultGPUWorkloadConfig(t)
	metrics := newStateManagerMetrics(t)
	return &ClusterPolicyController{operatorMetrics: metrics.operatorMetrics}, metrics
}

// init lists nodes once per discovery step -- discoverGPUNodes, getGPUNodeOSInfo, getRuntime and
// getKernelVersionsMap in that order -- so failing the call after a given number of successes is
// what pins an error to one step rather than to node listing in general. The call counter is
// returned because several of those steps return the same error unchanged, leaving the number of
// lists that got through as the only thing that tells them apart.
func failListAfter(successfulCalls int, err error) (interceptor.Funcs, *int) {
	listCalls := new(0)
	return interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			(*listCalls)++
			if *listCalls > successfulCalls {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	}, listCalls
}

func TestOpenshiftVersion(t *testing.T) {
	testCases := []struct {
		name            string
		history         string
		expectedVersion string
		expectedError   string
	}{
		{
			name:            "reports major and minor from the completed entry",
			history:         `{"state":"Completed","version":"4.16.9"}`,
			expectedVersion: "4.16",
		},
		{
			// History is newest-first, so a partial upgrade in progress must not be reported.
			name:            "skips entries that are not completed",
			history:         `{"state":"Partial","version":"4.17.3"},{"state":"Completed","version":"4.16.9"}`,
			expectedVersion: "4.16",
		},
		{
			name:            "reports a version carrying no minor component",
			history:         `{"state":"Completed","version":"4"}`,
			expectedVersion: "4",
		},
		{
			name:          "errors when no entry has completed",
			history:       `{"state":"Partial","version":"4.17.3"}`,
			expectedError: "failed to find Completed Cluster Version",
		},
		{
			name:          "errors when the history is empty",
			history:       ``,
			expectedError: "failed to find Completed Cluster Version",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			serveKubernetesAPI(t, map[string]http.HandlerFunc{
				clusterVersionPath: jsonHandler(http.StatusOK, clusterVersionBody(tc.history)),
			})

			version, err := OpenshiftVersion(context.Background())

			if tc.expectedError != "" {
				require.ErrorContains(t, err, tc.expectedError)
				require.Empty(t, version)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedVersion, version)
		})
	}
}

func TestOpenshiftVersionNotFoundOnVanillaKubernetes(t *testing.T) {
	// init tolerates NotFound as "this is not OpenShift" and nothing else, so the classification
	// is the contract here, not merely that an error came back.
	serveKubernetesAPI(t, map[string]http.HandlerFunc{
		clusterVersionPath: jsonHandler(http.StatusNotFound, notFoundStatusBody),
	})

	version, err := OpenshiftVersion(context.Background())

	require.True(t, apierrors.IsNotFound(err), "expected a NotFound error, got %v", err)
	require.Empty(t, version)
}

func TestOpenshiftVersionClientBuildError(t *testing.T) {
	writeUnbuildableKubeconfig(t)

	version, err := OpenshiftVersion(context.Background())

	require.ErrorContains(t, err, `no Auth Provider found for name "unregistered-auth-provider"`)
	require.Empty(t, version)
}

func TestKubernetesVersion(t *testing.T) {
	t.Run("reports the git version", func(t *testing.T) {
		serveKubernetesAPI(t, map[string]http.HandlerFunc{
			serverVersionPath: jsonHandler(http.StatusOK, serverVersionBody("v1.31.4")),
		})

		version, err := KubernetesVersion()

		require.NoError(t, err)
		require.Equal(t, "v1.31.4", version)
	})

	t.Run("errors when the API server rejects the request", func(t *testing.T) {
		serveKubernetesAPI(t, map[string]http.HandlerFunc{
			serverVersionPath: jsonHandler(http.StatusInternalServerError, internalErrorStatusBody),
		})

		version, err := KubernetesVersion()

		require.ErrorContains(t, err, "unable to fetch server version information")
		require.Empty(t, version)
	})
}

func TestKubernetesVersionClientBuildError(t *testing.T) {
	writeUnbuildableKubeconfig(t)

	version, err := KubernetesVersion()

	require.ErrorContains(t, err, "error building discovery client")
	require.ErrorContains(t, err, `no Auth Provider found for name "unregistered-auth-provider"`)
	require.Empty(t, version)
}

func TestGetClusterWideProxy(t *testing.T) {
	t.Run("returns the configured proxy", func(t *testing.T) {
		serveKubernetesAPI(t, map[string]http.HandlerFunc{
			clusterProxyPath: jsonHandler(http.StatusOK, `{"apiVersion":"config.openshift.io/v1","kind":"Proxy","metadata":{"name":"cluster"},"spec":{"httpProxy":"http://proxy.example.com:3128","noProxy":".cluster.local"}}`),
		})

		proxy, err := GetClusterWideProxy(context.Background())

		require.NoError(t, err)
		require.Equal(t, "http://proxy.example.com:3128", proxy.Spec.HTTPProxy)
		require.Equal(t, ".cluster.local", proxy.Spec.NoProxy)
	})

	t.Run("returns no proxy when the resource is absent", func(t *testing.T) {
		serveKubernetesAPI(t, map[string]http.HandlerFunc{
			clusterProxyPath: jsonHandler(http.StatusNotFound, notFoundStatusBody),
		})

		proxy, err := GetClusterWideProxy(context.Background())

		require.True(t, apierrors.IsNotFound(err), "expected a NotFound error, got %v", err)
		require.Nil(t, proxy)
	})
}

func TestGetClusterWideProxyClientBuildError(t *testing.T) {
	writeUnbuildableKubeconfig(t)

	proxy, err := GetClusterWideProxy(context.Background())

	require.ErrorContains(t, err, `no Auth Provider found for name "unregistered-auth-provider"`)
	require.Nil(t, proxy)
}

func TestValidateClusterPolicySpec(t *testing.T) {
	testCases := []struct {
		name          string
		spec          *gpuv1.ClusterPolicySpec
		expectedError string
	}{
		{
			name: "valid CDI object in spec",
			spec: &gpuv1.ClusterPolicySpec{
				CDI: gpuv1.CDIConfigSpec{
					Enabled:          new(true),
					NRIPluginEnabled: new(true),
				},
			},
		},
		{
			name: "invalid CDI object in spec",
			spec: &gpuv1.ClusterPolicySpec{
				CDI: gpuv1.CDIConfigSpec{
					Enabled:          new(false),
					NRIPluginEnabled: new(true),
				},
			},
			expectedError: "the NRI Plugin cannot be enabled when CDI is disabled",
		},
		{
			name: "invalid CDI and Toolkit config combination",
			spec: &gpuv1.ClusterPolicySpec{
				CDI: gpuv1.CDIConfigSpec{
					Enabled:          new(true),
					NRIPluginEnabled: new(true),
				},
				Toolkit: gpuv1.ToolkitSpec{
					Enabled: new(false),
				},
			},
			expectedError: "the NRI Plugin cannot be enabled when the Container Toolkit is disabled",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClusterPolicySpec(tc.spec)
			if tc.expectedError == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.expectedError)
			}
		})
	}
}

func TestStep(t *testing.T) {
	expectedErr := errors.New("step failed")

	testCases := []struct {
		name           string
		controls       []controlFunc
		expectedStatus gpuv1.State
		expectedIndex  int
		expectedError  error
	}{
		{
			name:           "advances on success",
			controls:       []controlFunc{{func(ClusterPolicyController) (gpuv1.State, error) { return gpuv1.Ready, nil }}},
			expectedStatus: gpuv1.Ready,
			expectedIndex:  1,
		},
		{
			name:           "does not advance on error",
			controls:       []controlFunc{{func(ClusterPolicyController) (gpuv1.State, error) { return gpuv1.NotReady, expectedErr }}},
			expectedStatus: gpuv1.NotReady,
			expectedIndex:  0,
			expectedError:  expectedErr,
		},
		{
			// A state is only as ready as its least ready resource, so one not-ready control
			// func has to outweigh the ready ones that ran before it.
			name: "reports not-ready when a later control func is not ready",
			controls: []controlFunc{{
				func(ClusterPolicyController) (gpuv1.State, error) { return gpuv1.Ready, nil },
				func(ClusterPolicyController) (gpuv1.State, error) { return gpuv1.NotReady, nil },
			}},
			expectedStatus: gpuv1.NotReady,
			expectedIndex:  1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			controller := ClusterPolicyController{
				controls:   tc.controls,
				stateNames: []string{"test-state"},
				singleton:  &gpuv1.ClusterPolicy{},
				logger:     logr.Discard(),
			}

			status, err := controller.step()

			if tc.expectedError != nil {
				require.ErrorIs(t, err, tc.expectedError)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.expectedStatus, status)
			require.Equal(t, tc.expectedIndex, controller.idx)
		})
	}
}

// Mirrors the field index ClusterPolicyReconciler registers with the manager. Without it the
// fake client rejects the MatchingFields selector cleanupAllDriverDaemonSets lists with.
func indexDaemonSetByOwner(object ctrlclient.Object) []string {
	owner := metav1.GetControllerOf(object)
	if owner == nil {
		return nil
	}
	return []string{owner.Name}
}

func newDriverDaemonSetClient(t *testing.T, interceptors interceptor.Funcs, daemonSets ...*appsv1.DaemonSet) ctrlclient.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t, appsv1.AddToScheme, gpuv1.AddToScheme)).
		WithInterceptorFuncs(interceptors).
		WithIndex(&appsv1.DaemonSet{}, clusterPolicyControllerIndexKey, indexDaemonSetByOwner).
		WithObjects(asClientObjects(daemonSets...)...).
		Build()
}

func newClusterPolicyOwnedDaemonSet(name, ownerName string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "gpu-operator",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: gpuv1.SchemeGroupVersion.String(),
				Kind:       "ClusterPolicy",
				Name:       ownerName,
				Controller: new(true),
			}},
		},
	}
}

func newDriverCRDClusterPolicy() *gpuv1.ClusterPolicy {
	return &gpuv1.ClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-cluster-policy"},
		Spec:       gpuv1.ClusterPolicySpec{Driver: gpuv1.DriverSpec{UseNvidiaDriverCRD: new(true)}},
	}
}

func TestStepCleansUpDriverDaemonSetsWhenTheDriverCRDIsEnabled(t *testing.T) {
	testCases := []struct {
		stateName     string
		daemonSetName string
	}{
		{stateName: "state-driver", daemonSetName: "nvidia-driver-daemonset"},
		{stateName: "state-vgpu-manager", daemonSetName: "nvidia-vgpu-manager-daemonset"},
	}

	for _, tc := range testCases {
		t.Run(tc.stateName, func(t *testing.T) {
			var capturedPropagationPolicies []metav1.DeletionPropagation
			var deletedDaemonSetNames []string
			client := newDriverDaemonSetClient(t, interceptor.Funcs{
				Delete: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.DeleteOption) error {
					options := ctrlclient.DeleteOptions{}
					for _, option := range opts {
						option.ApplyToDelete(&options)
					}
					require.NotNil(t, options.PropagationPolicy)
					capturedPropagationPolicies = append(capturedPropagationPolicies, *options.PropagationPolicy)
					deletedDaemonSetNames = append(deletedDaemonSetNames, obj.GetName())
					return c.Delete(ctx, obj, opts...)
				},
			}, newClusterPolicyOwnedDaemonSet(tc.daemonSetName, "gpu-cluster-policy"))
			controller := newClusterPolicyController(client)
			controller.singleton = newDriverCRDClusterPolicy()
			controller.stateNames = []string{tc.stateName}

			status, err := controller.step()

			require.NoError(t, err)
			require.Equal(t, gpuv1.Disabled, status)
			require.Equal(t, 1, controller.idx)
			require.Equal(t, []string{tc.daemonSetName}, deletedDaemonSetNames)
			// Orphan is what keeps the running driver pods serving while NVIDIADriver rolls
			// replacements; any other policy tears the GPU workloads down during the handover.
			require.Equal(t, []metav1.DeletionPropagation{metav1.DeletePropagationOrphan}, capturedPropagationPolicies)
		})
	}
}

func TestStepDoesNotAdvanceWhenDriverDaemonSetCleanupFails(t *testing.T) {
	expectedErr := errors.New("list failed")
	controller := newClusterPolicyController(newErrorListClient(t, expectedErr))
	controller.singleton = newDriverCRDClusterPolicy()
	controller.stateNames = []string{"state-driver"}

	status, err := controller.step()

	require.ErrorIs(t, err, expectedErr)
	require.ErrorContains(t, err, "failed to cleanup all NVIDIA driver daemonsets owned by ClusterPolicy")
	require.Equal(t, gpuv1.NotReady, status)
	require.Zero(t, controller.idx)
}

func TestIsStateEnabled(t *testing.T) {
	enabled := new(true)
	disabled := new(false)

	testCases := []struct {
		name            string
		stateName       string
		sandboxEnabled  bool
		spec            gpuv1.ClusterPolicySpec
		expectedEnabled bool
	}{
		{
			name:            "pre-requisites disabled when the NRI plugin runs",
			stateName:       "pre-requisites",
			spec:            gpuv1.ClusterPolicySpec{CDI: gpuv1.CDIConfigSpec{Enabled: enabled, NRIPluginEnabled: enabled}},
			expectedEnabled: false,
		},
		{
			name:            "pre-requisites enabled without the NRI plugin",
			stateName:       "pre-requisites",
			spec:            gpuv1.ClusterPolicySpec{CDI: gpuv1.CDIConfigSpec{Enabled: enabled, NRIPluginEnabled: disabled}},
			expectedEnabled: true,
		},
		{
			name:            "container toolkit enabled",
			stateName:       "state-container-toolkit",
			spec:            gpuv1.ClusterPolicySpec{Toolkit: gpuv1.ToolkitSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "container toolkit disabled",
			stateName:       "state-container-toolkit",
			spec:            gpuv1.ClusterPolicySpec{Toolkit: gpuv1.ToolkitSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "driver enabled",
			stateName:       "state-driver",
			spec:            gpuv1.ClusterPolicySpec{Driver: gpuv1.DriverSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "driver disabled",
			stateName:       "state-driver",
			spec:            gpuv1.ClusterPolicySpec{Driver: gpuv1.DriverSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "device plugin enabled",
			stateName:       "state-device-plugin",
			spec:            gpuv1.ClusterPolicySpec{DevicePlugin: gpuv1.DevicePluginSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "device plugin disabled",
			stateName:       "state-device-plugin",
			spec:            gpuv1.ClusterPolicySpec{DevicePlugin: gpuv1.DevicePluginSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "dcgm exporter enabled",
			stateName:       "state-dcgm-exporter",
			spec:            gpuv1.ClusterPolicySpec{DCGMExporter: gpuv1.DCGMExporterSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "dcgm exporter disabled",
			stateName:       "state-dcgm-exporter",
			spec:            gpuv1.ClusterPolicySpec{DCGMExporter: gpuv1.DCGMExporterSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "mig manager enabled",
			stateName:       "state-mig-manager",
			spec:            gpuv1.ClusterPolicySpec{MIGManager: gpuv1.MIGManagerSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "mig manager disabled",
			stateName:       "state-mig-manager",
			spec:            gpuv1.ClusterPolicySpec{MIGManager: gpuv1.MIGManagerSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "node status exporter enabled",
			stateName:       "state-node-status-exporter",
			spec:            gpuv1.ClusterPolicySpec{NodeStatusExporter: gpuv1.NodeStatusExporterSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "node status exporter disabled",
			stateName:       "state-node-status-exporter",
			spec:            gpuv1.ClusterPolicySpec{NodeStatusExporter: gpuv1.NodeStatusExporterSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		// state-mps-control-daemon has no spec of its own; it tracks the device-plugin toggle.
		{
			name:            "mps control daemon follows an enabled device plugin",
			stateName:       "state-mps-control-daemon",
			spec:            gpuv1.ClusterPolicySpec{DevicePlugin: gpuv1.DevicePluginSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "mps control daemon follows a disabled device plugin",
			stateName:       "state-mps-control-daemon",
			spec:            gpuv1.ClusterPolicySpec{DevicePlugin: gpuv1.DevicePluginSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "dcgm enabled",
			stateName:       "state-dcgm",
			spec:            gpuv1.ClusterPolicySpec{DCGM: gpuv1.DCGMSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "dcgm disabled",
			stateName:       "state-dcgm",
			spec:            gpuv1.ClusterPolicySpec{DCGM: gpuv1.DCGMSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "gpu feature discovery enabled",
			stateName:       "gpu-feature-discovery",
			spec:            gpuv1.ClusterPolicySpec{GPUFeatureDiscovery: gpuv1.GPUFeatureDiscoverySpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "gpu feature discovery disabled",
			stateName:       "gpu-feature-discovery",
			spec:            gpuv1.ClusterPolicySpec{GPUFeatureDiscovery: gpuv1.GPUFeatureDiscoverySpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		// state-kata-manager is unconditionally disabled: the component is deprecated and
		// production ignores its ClusterPolicy field. Enabling the field is what makes this
		// case falsifiable rather than a restatement of the zero value.
		{
			name:           "kata manager stays disabled even when its spec enables it",
			stateName:      "state-kata-manager",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				KataManager:      gpuv1.KataManagerSpec{Enabled: enabled},
				SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
			},
			expectedEnabled: false,
		},
		{
			name:            "vfio manager enabled with sandbox workloads",
			stateName:       "state-vfio-manager",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{VFIOManager: gpuv1.VFIOManagerSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "vfio manager disabled without sandbox workloads",
			stateName:       "state-vfio-manager",
			sandboxEnabled:  false,
			spec:            gpuv1.ClusterPolicySpec{VFIOManager: gpuv1.VFIOManagerSpec{Enabled: enabled}},
			expectedEnabled: false,
		},
		{
			name:            "vfio manager disabled by its own spec",
			stateName:       "state-vfio-manager",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{VFIOManager: gpuv1.VFIOManagerSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "vgpu manager enabled with sandbox workloads",
			stateName:       "state-vgpu-manager",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{VGPUManager: gpuv1.VGPUManagerSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "vgpu manager disabled by its own spec",
			stateName:       "state-vgpu-manager",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{VGPUManager: gpuv1.VGPUManagerSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		{
			name:            "vgpu manager disabled without sandbox workloads",
			stateName:       "state-vgpu-manager",
			sandboxEnabled:  false,
			spec:            gpuv1.ClusterPolicySpec{VGPUManager: gpuv1.VGPUManagerSpec{Enabled: enabled}},
			expectedEnabled: false,
		},
		{
			name:            "vgpu device manager enabled with sandbox workloads",
			stateName:       "state-vgpu-device-manager",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{VGPUDeviceManager: gpuv1.VGPUDeviceManagerSpec{Enabled: enabled}},
			expectedEnabled: true,
		},
		{
			name:            "vgpu device manager disabled without sandbox workloads",
			stateName:       "state-vgpu-device-manager",
			sandboxEnabled:  false,
			spec:            gpuv1.ClusterPolicySpec{VGPUDeviceManager: gpuv1.VGPUDeviceManagerSpec{Enabled: enabled}},
			expectedEnabled: false,
		},
		{
			name:            "vgpu device manager disabled by its own spec",
			stateName:       "state-vgpu-device-manager",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{VGPUDeviceManager: gpuv1.VGPUDeviceManagerSpec{Enabled: disabled}},
			expectedEnabled: false,
		},
		// state-cc-manager is a three-way conjunction; one case per falsifier, so a mutation
		// that drops any single conjunct fails at least one case.
		{
			name:           "cc manager enabled with sandbox workloads in kata mode",
			stateName:      "state-cc-manager",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				CCManager:        gpuv1.CCManagerSpec{Enabled: enabled},
				SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
			},
			expectedEnabled: true,
		},
		{
			name:           "cc manager disabled without sandbox workloads",
			stateName:      "state-cc-manager",
			sandboxEnabled: false,
			spec: gpuv1.ClusterPolicySpec{
				CCManager:        gpuv1.CCManagerSpec{Enabled: enabled},
				SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
			},
			expectedEnabled: false,
		},
		{
			name:           "cc manager disabled by its own spec",
			stateName:      "state-cc-manager",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				CCManager:        gpuv1.CCManagerSpec{Enabled: disabled},
				SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
			},
			expectedEnabled: false,
		},
		{
			name:           "cc manager disabled in kubevirt mode",
			stateName:      "state-cc-manager",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				CCManager:        gpuv1.CCManagerSpec{Enabled: enabled},
				SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.KubeVirt)},
			},
			expectedEnabled: false,
		},
		{
			name:            "sandbox validation follows sandbox workloads enabled",
			stateName:       "state-sandbox-validation",
			sandboxEnabled:  true,
			expectedEnabled: true,
		},
		{
			name:            "sandbox validation follows sandbox workloads disabled",
			stateName:       "state-sandbox-validation",
			sandboxEnabled:  false,
			expectedEnabled: false,
		},
		// state-operator-validation is unconditional. Disabling every component and sandbox
		// workloads is what makes a mutation to `return false` fail here.
		{
			name:      "operator validation stays enabled with everything else disabled",
			stateName: "state-operator-validation",
			spec: gpuv1.ClusterPolicySpec{
				Driver:       gpuv1.DriverSpec{Enabled: disabled},
				Toolkit:      gpuv1.ToolkitSpec{Enabled: disabled},
				DevicePlugin: gpuv1.DevicePluginSpec{Enabled: disabled},
			},
			expectedEnabled: true,
		},
		// state-sandbox-device-plugin and state-kata-device-plugin are three-way conjunctions;
		// one case per falsifier.
		{
			name:           "sandbox device plugin enabled in kubevirt mode",
			stateName:      "state-sandbox-device-plugin",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:    gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.KubeVirt)},
				SandboxDevicePlugin: gpuv1.SandboxDevicePluginSpec{Enabled: enabled},
			},
			expectedEnabled: true,
		},
		{
			name:           "sandbox device plugin disabled in kata mode",
			stateName:      "state-sandbox-device-plugin",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:    gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
				SandboxDevicePlugin: gpuv1.SandboxDevicePluginSpec{Enabled: enabled},
			},
			expectedEnabled: false,
		},
		{
			name:           "sandbox device plugin disabled by its own spec",
			stateName:      "state-sandbox-device-plugin",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:    gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.KubeVirt)},
				SandboxDevicePlugin: gpuv1.SandboxDevicePluginSpec{Enabled: disabled},
			},
			expectedEnabled: false,
		},
		{
			name:           "sandbox device plugin disabled without sandbox workloads",
			stateName:      "state-sandbox-device-plugin",
			sandboxEnabled: false,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:    gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.KubeVirt)},
				SandboxDevicePlugin: gpuv1.SandboxDevicePluginSpec{Enabled: enabled},
			},
			expectedEnabled: false,
		},
		{
			name:           "kata device plugin enabled in kata mode",
			stateName:      "state-kata-device-plugin",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:        gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
				KataSandboxDevicePlugin: gpuv1.KataDevicePluginSpec{ComponentCommonSpec: gpuv1.ComponentCommonSpec{Enabled: enabled}},
			},
			expectedEnabled: true,
		},
		{
			name:           "kata device plugin disabled in kubevirt mode",
			stateName:      "state-kata-device-plugin",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:        gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.KubeVirt)},
				KataSandboxDevicePlugin: gpuv1.KataDevicePluginSpec{ComponentCommonSpec: gpuv1.ComponentCommonSpec{Enabled: enabled}},
			},
			expectedEnabled: false,
		},
		{
			name:           "kata device plugin disabled by its own spec",
			stateName:      "state-kata-device-plugin",
			sandboxEnabled: true,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:        gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
				KataSandboxDevicePlugin: gpuv1.KataDevicePluginSpec{ComponentCommonSpec: gpuv1.ComponentCommonSpec{Enabled: disabled}},
			},
			expectedEnabled: false,
		},
		{
			name:           "kata device plugin disabled without sandbox workloads",
			stateName:      "state-kata-device-plugin",
			sandboxEnabled: false,
			spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads:        gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)},
				KataSandboxDevicePlugin: gpuv1.KataDevicePluginSpec{ComponentCommonSpec: gpuv1.ComponentCommonSpec{Enabled: enabled}},
			},
			expectedEnabled: false,
		},
		{
			name:      "operator metrics stays enabled with everything else disabled",
			stateName: "state-operator-metrics",
			spec: gpuv1.ClusterPolicySpec{
				Driver:       gpuv1.DriverSpec{Enabled: disabled},
				Toolkit:      gpuv1.ToolkitSpec{Enabled: disabled},
				DevicePlugin: gpuv1.DevicePluginSpec{Enabled: disabled},
			},
			expectedEnabled: true,
		},
		// Enabled is an optional pointer field and the nil defaults are asymmetric: DriverSpec
		// defaults to true, the sandbox-gated operands default to false.
		{
			name:            "driver enabled by default when its spec omits the field",
			stateName:       "state-driver",
			expectedEnabled: true,
		},
		{
			name:            "vgpu manager disabled by default when its spec omits the field",
			stateName:       "state-vgpu-manager",
			sandboxEnabled:  true,
			expectedEnabled: false,
		},
		{
			name:            "sandbox device plugin disabled by default when its spec omits the field",
			stateName:       "state-sandbox-device-plugin",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.KubeVirt)}},
			expectedEnabled: false,
		},
		{
			name:            "kata device plugin disabled by default when its spec omits the field",
			stateName:       "state-kata-device-plugin",
			sandboxEnabled:  true,
			spec:            gpuv1.ClusterPolicySpec{SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Mode: string(gpuv1.Kata)}},
			expectedEnabled: false,
		},
		{
			name:            "unknown state is disabled",
			stateName:       "state-does-not-exist",
			expectedEnabled: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			controller := ClusterPolicyController{
				singleton:      &gpuv1.ClusterPolicy{Spec: tc.spec},
				sandboxEnabled: tc.sandboxEnabled,
				logger:         logr.Discard(),
			}

			require.Equal(t, tc.expectedEnabled, controller.isStateEnabled(tc.stateName))
		})
	}
}

func TestInitOnVanillaKubernetes(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	controller.ocpDriverToolkit.requested = true
	node := gpuNodeWithOSLabels("gpu-1", "ubuntu", "22.04")
	node.Status.NodeInfo.ContainerRuntimeVersion = "containerd://1.7.0"
	reconciler := newInitReconciler(t, interceptor.Funcs{}, node)

	// The twenty addState calls read manifests from /opt/gpu-operator, which is absent here.
	// filePathWalkDir swallows the walk error, so each state registers with no resources rather
	// than failing, which is what makes init reachable without manifest fixtures.
	require.NoError(t, controller.init(context.Background(), reconciler, &gpuv1.ClusterPolicy{}))

	// step() walks these in order under a single idx, so the order is part of the contract.
	require.Equal(t, []string{
		"pre-requisites",
		"state-operator-metrics",
		"state-driver",
		"state-container-toolkit",
		"state-operator-validation",
		"state-device-plugin",
		"state-mps-control-daemon",
		"state-dcgm",
		"state-dcgm-exporter",
		"gpu-feature-discovery",
		"state-mig-manager",
		"state-node-status-exporter",
		"state-vgpu-manager",
		"state-vgpu-device-manager",
		"state-sandbox-validation",
		"state-vfio-manager",
		"state-sandbox-device-plugin",
		"state-kata-device-plugin",
		"state-kata-manager",
		"state-cc-manager",
	}, controller.stateNames)
	// The literal count, not len(stateNames): addState appends to controls and stateNames in
	// lockstep, so comparing them to each other holds however many states were dropped.
	require.Len(t, controller.controls, 20)
	require.Equal(t, "v1.31.4", controller.k8sVersion)
	require.Empty(t, controller.openshift)
	require.Equal(t, gpuv1.Containerd, controller.runtime)
	require.Equal(t, "ubuntu", controller.gpuNodeOSRelease)
	require.Equal(t, "ubuntu22.04", controller.gpuNodeOSTag)
	require.True(t, controller.hasGPUNodes)
	// The GPU node carries NFD's OS labels, which is what discovery reports back.
	require.True(t, controller.hasNFDLabels)
	require.False(t, controller.ocpDriverToolkit.requested)
	require.Equal(t, "gpu-operator", clusterPolicyCtrl.operatorNamespace)
}

func TestInitSkipsDiscoveryWhenStatesAreRegistered(t *testing.T) {
	// A non-empty controls slice means a previous reconcile already registered the states, so
	// init must not re-detect versions. The kubeconfig points at a server that fails the test on
	// any request: leaving KUBECONFIG unset would make a regression here reach the developer's
	// real cluster, or call os.Exit and take the whole test binary down.
	serveKubernetesAPI(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected API request to %s; version detection must be skipped", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	controller, _ := newInitController(t)
	controller.controls = []controlFunc{{func(ClusterPolicyController) (gpuv1.State, error) { return gpuv1.Ready, nil }}}
	reconciler := newInitReconciler(t, interceptor.Funcs{})

	require.NoError(t, controller.init(context.Background(), reconciler, &gpuv1.ClusterPolicy{}))

	require.Len(t, controller.controls, 1)
	require.Empty(t, controller.k8sVersion)
}

func TestInitRejectsNonSemverKubernetesVersion(t *testing.T) {
	serveVanillaKubernetesAPI(t, "not-a-version")
	controller, _ := newInitController(t)

	err := controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}), &gpuv1.ClusterPolicy{})

	require.ErrorContains(t, err, "k8s version detected 'not-a-version' is not a valid semantic version")
}

func TestInitRejectsInvalidClusterPolicy(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
		CDI: gpuv1.CDIConfigSpec{Enabled: new(false), NRIPluginEnabled: new(true)},
	}}

	err := controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}), policy)

	require.ErrorContains(t, err, "error validating clusterpolicy")
	require.ErrorContains(t, errors.Unwrap(err), "the NRI Plugin cannot be enabled when CDI is disabled")
}

func TestInitPropagatesOpenshiftVersionError(t *testing.T) {
	serveKubernetesAPI(t, map[string]http.HandlerFunc{
		clusterVersionPath: jsonHandler(http.StatusInternalServerError, internalErrorStatusBody),
	})
	controller, _ := newInitController(t)

	err := controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}), &gpuv1.ClusterPolicy{})

	require.False(t, apierrors.IsNotFound(err), "only NotFound is tolerated, got %v", err)
	// The Status body carries no reason, so client-go classifies it from the 500 alone.
	require.True(t, apierrors.IsInternalError(err), "expected the API server failure to surface, got %v", err)
}

func TestInitPropagatesKubernetesVersionError(t *testing.T) {
	serveKubernetesAPI(t, map[string]http.HandlerFunc{
		clusterVersionPath: jsonHandler(http.StatusNotFound, notFoundStatusBody),
		serverVersionPath:  jsonHandler(http.StatusInternalServerError, internalErrorStatusBody),
	})
	controller, _ := newInitController(t)

	err := controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}), &gpuv1.ClusterPolicy{})

	require.ErrorContains(t, err, "unable to fetch server version information")
}

func TestInitSandboxWorkloads(t *testing.T) {
	testCases := []struct {
		name                   string
		defaultWorkload        string
		expectedWorkloadConfig string
	}{
		{
			name:                   "a valid default workload overrides the package default",
			defaultWorkload:        gpuWorkloadConfigVMVgpu,
			expectedWorkloadConfig: gpuWorkloadConfigVMVgpu,
		},
		{
			// An unrecognized value is ignored rather than adopted: init assigns the global
			// only after isValidWorkloadConfig passes, so the previous config survives.
			name:                   "an invalid default workload is ignored",
			defaultWorkload:        "vm-bogus",
			expectedWorkloadConfig: gpuWorkloadConfigContainer,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			serveVanillaKubernetesAPI(t, "v1.31.4")
			controller, _ := newInitController(t)
			defaultGPUWorkloadConfig = gpuWorkloadConfigContainer
			policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
				SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Enabled: new(true), DefaultWorkload: tc.defaultWorkload},
			}}

			require.NoError(t, controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}), policy))

			require.True(t, controller.sandboxEnabled)
			require.Equal(t, tc.expectedWorkloadConfig, defaultGPUWorkloadConfig)
		})
	}
}

func TestInitDisablesSandboxWorkloads(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	// Seed the field as enabled so asserting false proves init cleared it on a reconcile that
	// follows an enabled one, rather than restating the zero value.
	controller.sandboxEnabled = true
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
		SandboxWorkloads: gpuv1.SandboxWorkloadsSpec{Enabled: new(false), DefaultWorkload: gpuWorkloadConfigVMVgpu},
	}}

	require.NoError(t, controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}), policy))

	require.False(t, controller.sandboxEnabled)
	// The default workload is only adopted from a ClusterPolicy that enables sandbox workloads.
	require.Equal(t, gpuWorkloadConfigContainer, defaultGPUWorkloadConfig)
}

func TestInitAppliesPodSecurityLabels(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	reconciler := newInitReconciler(t, interceptor.Funcs{}, newNamespace("gpu-operator", nil))
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
		PSA: gpuv1.PSASpec{Enabled: new(true)},
	}}

	require.NoError(t, controller.init(context.Background(), reconciler, policy))

	requirePrivilegedPodSecurityLabels(t, getNamespace(t, reconciler.Client, "gpu-operator"))
}

func TestInitPropagatesPodSecurityLabelError(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	reconciler := newInitReconciler(t, interceptor.Funcs{})
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{PSA: gpuv1.PSASpec{Enabled: new(true)}}}

	err := controller.init(context.Background(), reconciler, policy)

	require.ErrorContains(t, err, "could not get Namespace gpu-operator from client")
}

func TestInitPropagatesGPUNodeOSInfoError(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	// A GPU node with no NFD OS labels makes getGPUNodeOSInfo fail after discovery succeeded.
	reconciler := newInitReconciler(t, interceptor.Funcs{}, nodeWithLabels("gpu-1", map[string]string{commonGPULabelKey: commonGPULabelValue}))

	err := controller.init(context.Background(), reconciler, &gpuv1.ClusterPolicy{})

	require.ErrorContains(t, err, "failed to retrieve GPU node OS info")
	require.ErrorContains(t, errors.Unwrap(err), "unable to retrieve OS name")
}

func TestInitRejectsEmptyGPUNodeOSInfo(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	// An empty value, not an absent label: presence is all getGPUNodeOSInfo checks, so this
	// returns successfully and leaves the guard below as the only thing that can catch it.
	node := gpuNodeWithOSLabels("gpu-1", "", "22.04")

	err := controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}, node), &gpuv1.ClusterPolicy{})

	require.ErrorContains(t, err, "GPU node OS info is empty")
}

func TestInitPropagatesNodeDiscoveryError(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	expectedErr := errors.New("list failed")
	interceptors, listCalls := failListAfter(0, expectedErr)
	reconciler := newInitReconciler(t, interceptors)

	err := controller.init(context.Background(), reconciler, &gpuv1.ClusterPolicy{})

	require.ErrorIs(t, err, expectedErr)
	// getRuntime's message starts with the same words, so only the whole string tells the two
	// list sites apart.
	require.ErrorContains(t, err, "unable to list nodes: list failed")
	require.Equal(t, 1, *listCalls, "discovery is the first step to list nodes")
}

func TestInitPropagatesRuntimeDetectionError(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	expectedErr := errors.New("runtime list failed")
	// discoverGPUNodes lists first and must succeed; getRuntime's later list is the one that
	// fails, so the error can only come from the runtime-detection step.
	interceptors, listCalls := failListAfter(1, expectedErr)
	reconciler := newInitReconciler(t, interceptors)

	err := controller.init(context.Background(), reconciler, &gpuv1.ClusterPolicy{})

	require.ErrorContains(t, err, "unable to list nodes prior to checking container runtime")
	require.ErrorContains(t, err, expectedErr.Error())
	require.Equal(t, 2, *listCalls)
}

func TestInitCollectsKernelVersionsForPrecompiledDrivers(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	node := gpuNodeWithOSLabels("gpu-1", "ubuntu", "22.04")
	node.Labels[nfdKernelLabelKey] = "5.15.0-91-generic"
	node.Status.NodeInfo.ContainerRuntimeVersion = "containerd://1.7.0"
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
		Driver: gpuv1.DriverSpec{Enabled: new(true), UsePrecompiled: new(true)},
	}}

	require.NoError(t, controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}, node), policy))

	require.Equal(t, map[string]string{"5.15.0-91-generic": "ubuntu22.04"}, controller.kernelVersionMap)
}

func TestInitSkipsKernelVersionsWhenTheDriverIsDisabled(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	// Precompiled drivers alone must not trigger the lookup: with the driver disabled there is
	// no precompiled image to select a kernel for.
	node := gpuNodeWithOSLabels("gpu-1", "ubuntu", "22.04")
	node.Labels[nfdKernelLabelKey] = "5.15.0-91-generic"
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
		Driver: gpuv1.DriverSpec{Enabled: new(false), UsePrecompiled: new(true)},
	}}

	require.NoError(t, controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}, node), policy))

	require.Nil(t, controller.kernelVersionMap)
}

func TestInitPropagatesKernelVersionMapError(t *testing.T) {
	serveVanillaKubernetesAPI(t, "v1.31.4")
	controller, _ := newInitController(t)
	expectedErr := errors.New("kernel list failed")
	node := gpuNodeWithOSLabels("gpu-1", "ubuntu", "22.04")
	// getKernelVersionsMap returns the list error unchanged, as discoverGPUNodes' caller would
	// too, so the number of lists that got through is what attributes it to the kernel step.
	interceptors, listCalls := failListAfter(3, expectedErr)
	reconciler := newInitReconciler(t, interceptors, node)
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
		Driver: gpuv1.DriverSpec{Enabled: new(true), UsePrecompiled: new(true)},
	}}

	require.ErrorIs(t, controller.init(context.Background(), reconciler, policy), expectedErr)
	require.Equal(t, 4, *listCalls)
}

func TestInitOnOpenShiftRequestsDriverToolkit(t *testing.T) {
	serveOpenShiftAPI(t)
	controller, _ := newInitController(t)
	// The operator namespace is not the suggested one, so ocpEnsureNamespaceMonitoring (reached
	// through initOCPParams) returns early instead of needing a Namespace fixture.
	reconciler := newInitReconciler(t, interceptor.Funcs{})

	require.NoError(t, controller.init(context.Background(), reconciler, &gpuv1.ClusterPolicy{}))

	require.Equal(t, "4.16", controller.openshift)
	require.True(t, controller.ocpDriverToolkit.requested)
	require.NotNil(t, controller.ocpDriverToolkit.rhcosVersions)
	require.NotNil(t, controller.ocpDriverToolkit.rhcosDriverToolkitImages)
	require.Equal(t, gpuv1.CRIO, controller.runtime)
}

func TestInitOnOpenShiftHonorsDriverToolkitOptOut(t *testing.T) {
	serveOpenShiftAPI(t)
	controller, metrics := newInitController(t)
	// Seed both fields as already set, so clearing them is what the assertions detect. Leaving
	// the zero value would let deletion of the whole production branch pass unnoticed.
	controller.ocpDriverToolkit.requested = true
	controller.ocpDriverToolkit.enabled = true
	policy := &gpuv1.ClusterPolicy{Spec: gpuv1.ClusterPolicySpec{
		Operator: gpuv1.OperatorSpec{UseOpenShiftDriverToolkit: new(false)},
	}}

	require.NoError(t, controller.init(context.Background(), newInitReconciler(t, interceptor.Funcs{}), policy))

	require.False(t, controller.ocpDriverToolkit.requested)
	require.False(t, controller.ocpDriverToolkit.enabled)
	require.Equal(t, 1, metrics.driverToolkitEnabled.setCount)
	require.Equal(t, float64(openshiftDriverToolkitDisabled), metrics.driverToolkitEnabled.value)
}

func TestInitPropagatesOCPParamsError(t *testing.T) {
	serveOpenShiftAPI(t)
	controller, _ := newInitController(t)
	expectedErr := errors.New("imagestream unavailable")
	reconciler := newInitReconciler(t, failImageStreamGet(expectedErr))

	require.ErrorIs(t, controller.init(context.Background(), reconciler, &gpuv1.ClusterPolicy{}), expectedErr)
}

func newDriverToolkitImageStream(tags ...apiimagev1.TagReference) *apiimagev1.ImageStream {
	return &apiimagev1.ImageStream{
		ObjectMeta: metav1.ObjectMeta{Name: "driver-toolkit", Namespace: consts.OpenshiftNamespace},
		Spec:       apiimagev1.ImageStreamSpec{Tags: tags},
	}
}

// Already labeled, so initOCPParams' monitoring tail call is a no-op and cannot mask the
// branch under test.
func newMonitoringEnabledNamespace() *corev1.Namespace {
	return newNamespace(ocpSuggestedNamespace, map[string]string{ocpNamespaceMonitoringLabelKey: ocpNamespaceMonitoringLabelValue})
}

func newDriverToolkitController(t *testing.T, interceptors interceptor.Funcs, operatorNamespace *corev1.Namespace, objects ...ctrlclient.Object) (*ClusterPolicyController, *stateManagerMetrics) {
	t.Helper()
	client := fake.NewClientBuilder().
		WithScheme(newTestScheme(t, apiimagev1.AddToScheme)).
		WithInterceptorFuncs(interceptors).
		WithObjects(operatorNamespace).
		WithObjects(objects...).
		Build()

	metrics := newStateManagerMetrics(t)
	controller := newClusterPolicyController(client)
	controller.operatorMetrics = metrics.operatorMetrics
	controller.singleton = &gpuv1.ClusterPolicy{}
	controller.ocpDriverToolkit = OpenShiftDriverToolkit{
		rhcosVersions:            map[string]bool{},
		rhcosDriverToolkitImages: map[string]string{},
	}
	return &controller, metrics
}

func TestInitOCPParamsPrecompiledDriversDisableDriverToolkit(t *testing.T) {
	withOperatorNamespace(t, ocpSuggestedNamespace)
	controller, metrics := newDriverToolkitController(t, interceptor.Funcs{}, newMonitoringEnabledNamespace())
	controller.singleton.Spec.Driver.UsePrecompiled = new(true)
	controller.ocpDriverToolkit.requested = true
	// Seed the field as enabled so asserting false proves the precompiled branch cleared it,
	// rather than restating the zero value.
	controller.ocpDriverToolkit.enabled = true

	require.NoError(t, controller.initOCPParams())

	require.False(t, controller.ocpDriverToolkit.enabled)
	// The precompiled branch bypasses the else-if that writes the driver-toolkit gauges.
	require.Zero(t, metrics.driverToolkitEnabled.setCount)
	require.Zero(t, metrics.driverToolkitImageStreamMissing.setCount)
}

func TestInitOCPParamsSkipsImageStreamWhenNotRequested(t *testing.T) {
	withOperatorNamespace(t, ocpSuggestedNamespace)
	// A NotFound ImageStream is converted to (false, nil), so "returns nil" alone would still
	// pass if the branch were wrongly entered. Failing the ImageStream read is what pins it:
	// initOCPParams propagates that error unwrapped.
	interceptors := failImageStreamGet(errors.New("driver-toolkit ImageStream must not be read"))
	controller, metrics := newDriverToolkitController(t, interceptors, newMonitoringEnabledNamespace())
	controller.ocpDriverToolkit.requested = false
	controller.ocpDriverToolkit.enabled = true

	require.NoError(t, controller.initOCPParams())

	require.True(t, controller.ocpDriverToolkit.enabled, "the not-requested path must leave the field alone")
	require.Zero(t, metrics.driverToolkitEnabled.setCount)
}

func TestInitOCPParamsDriverToolkitRequested(t *testing.T) {
	testCases := []struct {
		name                            string
		imageStream                     *apiimagev1.ImageStream
		rhcosVersions                   map[string]bool
		hasGPUNodes                     bool
		expectedEnabled                 bool
		expectedEnabledGauge            float64
		expectedImageStreamMissingGauge float64
		expectedNfdTooOldGauge          float64
	}{
		{
			name:                            "image stream and compatible NFD enable the driver toolkit",
			imageStream:                     newDriverToolkitImageStream(),
			rhcosVersions:                   map[string]bool{"413.92.202": true},
			hasGPUNodes:                     true,
			expectedEnabled:                 true,
			expectedEnabledGauge:            openshiftDriverToolkitEnabled,
			expectedImageStreamMissingGauge: 0,
		},
		{
			name:                            "a missing image stream disables the driver toolkit",
			rhcosVersions:                   map[string]bool{"413.92.202": true},
			hasGPUNodes:                     true,
			expectedEnabled:                 false,
			expectedEnabledGauge:            openshiftDriverToolkitNotPossible,
			expectedImageStreamMissingGauge: 1,
		},
		{
			name:                            "GPU nodes without RHCOS versions report NFD as too old",
			imageStream:                     newDriverToolkitImageStream(),
			rhcosVersions:                   map[string]bool{},
			hasGPUNodes:                     true,
			expectedEnabled:                 false,
			expectedEnabledGauge:            openshiftDriverToolkitNotPossible,
			expectedImageStreamMissingGauge: 0,
			expectedNfdTooOldGauge:          1,
		},
		{
			name:                            "no GPU nodes does not report NFD as too old",
			imageStream:                     newDriverToolkitImageStream(),
			rhcosVersions:                   map[string]bool{},
			hasGPUNodes:                     false,
			expectedEnabled:                 false,
			expectedEnabledGauge:            openshiftDriverToolkitNotPossible,
			expectedImageStreamMissingGauge: 0,
			expectedNfdTooOldGauge:          0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			withOperatorNamespace(t, ocpSuggestedNamespace)
			var objects []ctrlclient.Object
			if tc.imageStream != nil {
				objects = append(objects, tc.imageStream)
			}
			controller, metrics := newDriverToolkitController(t, interceptor.Funcs{}, newMonitoringEnabledNamespace(), objects...)
			controller.ocpDriverToolkit.requested = true
			controller.ocpDriverToolkit.rhcosVersions = tc.rhcosVersions
			controller.hasGPUNodes = tc.hasGPUNodes

			require.NoError(t, controller.initOCPParams())

			require.Equal(t, tc.expectedEnabled, controller.ocpDriverToolkit.enabled)
			require.Equal(t, tc.expectedEnabledGauge, metrics.driverToolkitEnabled.value)
			require.Equal(t, tc.expectedImageStreamMissingGauge, metrics.driverToolkitImageStreamMissing.value)
			require.Equal(t, tc.expectedNfdTooOldGauge, metrics.driverToolkitNfdTooOld.value)
			// Each gauge starts at zero, so a value assertion alone cannot tell "written 0"
			// from "never written"; the write counts are what kill deletion of the Set(0) calls.
			require.Equal(t, 1, metrics.driverToolkitEnabled.setCount)
			require.Equal(t, 1, metrics.driverToolkitImageStreamMissing.setCount)
			require.Equal(t, 1, metrics.driverToolkitNfdTooOld.setCount)
		})
	}
}

func TestInitOCPParamsImageStreamError(t *testing.T) {
	withOperatorNamespace(t, ocpSuggestedNamespace)
	expectedErr := errors.New("imagestream unavailable")
	controller, _ := newDriverToolkitController(t, failImageStreamGet(expectedErr), newMonitoringEnabledNamespace())
	controller.ocpDriverToolkit.requested = true

	require.ErrorIs(t, controller.initOCPParams(), expectedErr)
}

func TestInitOCPParamsNamespaceMonitoringError(t *testing.T) {
	withOperatorNamespace(t, ocpSuggestedNamespace)
	controller, _ := newDriverToolkitController(t, failPatch(errors.New("patch failed")),
		newNamespace(ocpSuggestedNamespace, map[string]string{"kubernetes.io/metadata.name": ocpSuggestedNamespace}))
	controller.ocpDriverToolkit.requested = false

	err := controller.initOCPParams()

	// ocpEnsureNamespaceMonitoring rebuilds the cause with %s, so only the text survives.
	require.ErrorContains(t, err, "unable to label namespace "+ocpSuggestedNamespace+" for the GPU Operator monitoring")
}
