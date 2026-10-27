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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvidiav1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/conditions"
	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	"github.com/NVIDIA/gpu-operator/tests/e2e/helpers"
)

const (
	clusterPolicyConflictObservationPeriod = time.Minute

	secondGPUClusterName = "e2e-second-gpu-cluster"

	// TimeSlicingSettings only gates the time-slicing settings of claim configs, so enabling it changes
	// nothing for the claims these specs create.
	timeSlicingSettingsFeatureGate = "TimeSlicingSettings"
)

// labeledDRAOperandSelectors match the pods that carry spec.daemonsets.labels.
var labeledDRAOperandSelectors = []string{
	appSelector(draKubeletPluginApp),
	appSelector(draControllerApp),
	draValidatorSelector,
	appSelector(draDCGMExporterApp),
}

func gpuClusterChangeSpecs(install *operatorInstall) {
	ginkgo.Context("when the GPUCluster changes", func() {
		ginkgo.It("passes env vars, a feature gate and labels to the DRA operands", func(ctx context.Context) {
			clients := install.clients
			kubeletPluginEnv := nvidiav1.EnvVar{Name: "E2E_KUBELET_PLUGIN_ENV", Value: "set-by-e2e"}
			controllerEnv := nvidiav1.EnvVar{Name: "E2E_CONTROLLER_ENV", Value: "set-by-e2e"}
			customLabels := map[string]string{"cloudprovider": "aws", "platform": "kubernetes"}
			err := clients.gpuClusters.Modify(ctx, gpuClusterName, func(gpuCluster *nvidiav1alpha1.GPUCluster) {
				gpuCluster.Spec.DRADriver.GPUs.KubeletPlugin.Env = []nvidiav1.EnvVar{kubeletPluginEnv}
				gpuCluster.Spec.DRADriver.ComputeDomains.Controller.Env = []nvidiav1.EnvVar{controllerEnv}
				if gpuCluster.Spec.DRADriver.FeatureGates == nil {
					gpuCluster.Spec.DRADriver.FeatureGates = map[string]bool{}
				}
				gpuCluster.Spec.DRADriver.FeatureGates[timeSlicingSettingsFeatureGate] = true
				if gpuCluster.Spec.Daemonsets.Labels == nil {
					gpuCluster.Spec.Daemonsets.Labels = map[string]string{}
				}
				maps.Copy(gpuCluster.Spec.Daemonsets.Labels, customLabels)
			})
			framework.ExpectNoError(err, "failed to set the DRA driver env, feature gate and operand labels")

			waitForDRADriverEnv(ctx, clients, kubeletPluginEnv, controllerEnv)
			featureGateEnvMatcher := haveFeatureGateEnabled(timeSlicingSettingsFeatureGate)
			waitForDRAContainerEnv(ctx, clients, draKubeletPluginApp, draKubeletPluginGPUsContainer, kubeletPluginPodTemplate, featureGateEnvMatcher)
			waitForDRAContainerEnv(ctx, clients, draKubeletPluginApp, draKubeletPluginComputeContainer, kubeletPluginPodTemplate, featureGateEnvMatcher)
			waitForDRAOperandPodLabels(ctx, clients, customLabels)
			waitForDRAOperandsReady(ctx, clients)
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
		})

		ginkgo.It("removes and restores compute domains and the DCGM exporter", func(ctx context.Context) {
			clients := install.clients
			setComputeDomainsAndDCGMExporterEnabled(ctx, clients, false)
			waitForKubeletPluginContainers(ctx, clients, draKubeletPluginGPUsContainer)
			waitForNotFound(ctx, daemonSetUpdateTimeout, fmt.Sprintf("waiting for daemonset %s to be deleted", draDCGMExporterApp), func(ctx context.Context) error {
				_, err := clients.daemonSets.Get(ctx, tcfg.namespace, draDCGMExporterApp)
				return err
			})
			waitForPodsDeleted(ctx, clients, appSelector(draDCGMExporterApp))
			// The controller Deployment and the compute-domain DeviceClasses stay, because the DRA driver
			// state does not prune the objects of a disabled capability.
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)

			setComputeDomainsAndDCGMExporterEnabled(ctx, clients, true)
			waitForKubeletPluginContainers(ctx, clients, draKubeletPluginGPUsContainer, draKubeletPluginComputeContainer)
			waitForPodsReady(ctx, clients, appSelector(draDCGMExporterApp))
			waitForDRAOperandsReady(ctx, clients)
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
		})

		ginkgo.It("recovers after the operator restarts", func(ctx context.Context) {
			clients := install.clients
			restartOperator(ctx, clients)

			kubeletPluginEnv := nvidiav1.EnvVar{Name: "E2E_KUBELET_PLUGIN_ENV", Value: "set-after-operator-restart"}
			controllerEnv := nvidiav1.EnvVar{Name: "E2E_CONTROLLER_ENV", Value: "set-after-operator-restart"}
			err := clients.gpuClusters.Modify(ctx, gpuClusterName, func(gpuCluster *nvidiav1alpha1.GPUCluster) {
				gpuCluster.Spec.DRADriver.GPUs.KubeletPlugin.Env = []nvidiav1.EnvVar{kubeletPluginEnv}
				gpuCluster.Spec.DRADriver.ComputeDomains.Controller.Env = []nvidiav1.EnvVar{controllerEnv}
			})
			framework.ExpectNoError(err, "failed to change the DRA driver env")

			waitForDRADriverEnv(ctx, clients, kubeletPluginEnv, controllerEnv)
			waitForDRAOperandsReady(ctx, clients)
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
		})

		ginkgo.It("recreates a deleted kubelet plugin daemonset and DeviceClass", func(ctx context.Context) {
			clients := install.clients
			kubeletPluginDaemonSet, err := clients.daemonSets.Get(ctx, tcfg.namespace, draKubeletPluginApp)
			framework.ExpectNoError(err, "failed to get daemonset %s", draKubeletPluginApp)
			deviceClasses := clients.kube.ResourceV1().DeviceClasses()
			gpuDeviceClass, err := deviceClasses.Get(ctx, gpuDeviceClassName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get DeviceClass %s", gpuDeviceClassName)

			err = clients.kube.AppsV1().DaemonSets(tcfg.namespace).Delete(ctx, draKubeletPluginApp, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete daemonset %s", draKubeletPluginApp)
			err = deviceClasses.Delete(ctx, gpuDeviceClassName, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete DeviceClass %s", gpuDeviceClassName)

			var recreatedDaemonSetUID types.UID
			waitFor(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for daemonset %s and DeviceClass %s to be recreated", draKubeletPluginApp, gpuDeviceClassName), func(g gomega.Gomega) {
				recreatedDaemonSet, err := clients.daemonSets.Get(ctx, tcfg.namespace, draKubeletPluginApp)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(recreatedDaemonSet.UID).NotTo(gomega.Equal(kubeletPluginDaemonSet.UID), "daemonset %s was not recreated", draKubeletPluginApp)
				recreatedDeviceClass, err := deviceClasses.Get(ctx, gpuDeviceClassName, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(recreatedDeviceClass.UID).NotTo(gomega.Equal(gpuDeviceClass.UID), "DeviceClass %s was not recreated", gpuDeviceClassName)
				recreatedDaemonSetUID = recreatedDaemonSet.UID
			})

			waitFor(ctx, operandReadyTimeout, fmt.Sprintf("waiting for the pods of the recreated daemonset %s to be ready", draKubeletPluginApp), func(g gomega.Gomega) {
				pods, err := clients.pods.List(ctx, tcfg.namespace, appSelector(draKubeletPluginApp))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(pods).NotTo(gomega.BeEmpty(), "no pods of daemonset %s", draKubeletPluginApp)
				for _, pod := range pods {
					g.Expect(metav1.GetControllerOf(&pod)).To(gomega.HaveField("UID", recreatedDaemonSetUID), "pod %s is not owned by the recreated daemonset", pod.Name)
					g.Expect(helpers.IsPodReady(&pod)).To(gomega.BeTrue(), "pod %s is not ready", pod.Name)
					g.Expect(pod.DeletionTimestamp).To(gomega.BeNil(), "pod %s is terminating", pod.Name)
				}
			})
			waitForGPUResourceSlices(ctx, clients)
			waitForDRAOperandsReady(ctx, clients)
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
		})

		ginkgo.It("runs a claim workload after the changes", func(ctx context.Context) {
			runGPUClaimWorkload(ctx, install.clients)
			waitForGPUClusterReady(ctx, install.clients, gpuClusterReconcileTimeout)
		})
	})
}

func singletonAndClusterPolicyConflictSpecs(install *operatorInstall) {
	ginkgo.Context("when another GPUCluster or a ClusterPolicy is created", func() {
		ginkgo.It("rejects a second GPUCluster", func(ctx context.Context) {
			clients := install.clients
			gpuCluster, err := clients.gpuClusters.Get(ctx, gpuClusterName)
			framework.ExpectNoError(err, "failed to get GPUCluster %s", gpuClusterName)
			secondGPUCluster := &nvidiav1alpha1.GPUCluster{
				ObjectMeta: metav1.ObjectMeta{Name: secondGPUClusterName},
				Spec:       *gpuCluster.Spec.DeepCopy(),
			}

			ginkgo.DeferCleanup(deleteSecondGPUCluster, clients)
			_, err = clients.gpuClusters.Create(ctx, secondGPUCluster)
			gomega.Expect(err).To(gomega.MatchError(apierrors.IsInvalid, "an Invalid error"))
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("GPUCluster is a singleton, metadata.name must be 'gpu-cluster'")))

			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
		})

		ginkgo.It("reports a conflict with a ClusterPolicy and recovers once it is deleted", func(ctx context.Context) {
			clients := install.clients
			clusterPolicy := clusterPolicyFromChart(ctx, install)
			ginkgo.DeferCleanup(deleteClusterPolicy, clients, clusterPolicy.Name)
			_, err := clients.clusterPolicies.Create(ctx, clusterPolicy)
			framework.ExpectNoError(err, "failed to create ClusterPolicy %s", clusterPolicy.Name)

			waitFor(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for GPUCluster/%s to report the ClusterPolicy conflict", gpuClusterName), func(g gomega.Gomega) {
				gpuCluster, err := clients.gpuClusters.Get(ctx, gpuClusterName)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(gpuCluster.Status.State).To(gomega.Equal(nvidiav1alpha1.NotReady))
				g.Expect(meta.FindStatusCondition(gpuCluster.Status.Conditions, conditions.Error)).
					To(haveTrueErrorCondition(conditions.PrerequisiteNotMet, "may not exist at the same time"))
			})
			waitFor(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for ClusterPolicy/%s to report the GPUCluster conflict", clusterPolicy.Name), func(g gomega.Gomega) {
				createdClusterPolicy, err := clients.clusterPolicies.Get(ctx, clusterPolicy.Name)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(createdClusterPolicy.Status.State).To(gomega.Equal(nvidiav1.NotReady))
				g.Expect(meta.FindStatusCondition(createdClusterPolicy.Status.Conditions, conditions.Error)).
					To(haveTrueErrorCondition(conditions.ReconcileFailed, "cannot co-exist"))
			})

			ginkgo.By("checking that the ClusterPolicy deploys neither the device plugin nor GFD")
			gomega.Consistently(ctx, func(g gomega.Gomega) {
				for _, app := range []string{devicePluginApp, gpuFeatureDiscoveryApp} {
					daemonSets, err := clients.daemonSets.List(ctx, tcfg.namespace, appSelector(app))
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(daemonSets).To(gomega.BeEmpty(), "daemonset %s is deployed", app)
				}
			}).WithTimeout(clusterPolicyConflictObservationPeriod).WithPolling(pollInterval).Should(gomega.Succeed())
			expectDevicePluginAndGFDNotDeployed(ctx, clients)

			deleteClusterPolicy(ctx, clients, clusterPolicy.Name)
			waitForGPUClusterReady(ctx, clients, gpuClusterReconcileTimeout)
			waitForDRAOperandsReady(ctx, clients)
		})
	})
}

func setComputeDomainsAndDCGMExporterEnabled(ctx context.Context, clients clusterClients, enabled bool) {
	ginkgo.GinkgoHelper()
	err := clients.gpuClusters.Modify(ctx, gpuClusterName, func(gpuCluster *nvidiav1alpha1.GPUCluster) {
		gpuCluster.Spec.DRADriver.ComputeDomains.Enabled = new(enabled)
		if gpuCluster.Spec.DCGMExporter == nil {
			gpuCluster.Spec.DCGMExporter = &nvidiav1.DCGMExporterSpec{}
		}
		gpuCluster.Spec.DCGMExporter.Enabled = new(enabled)
	})
	framework.ExpectNoError(err, "failed to set compute domains and the DCGM exporter enabled=%t", enabled)
}

func kubeletPluginPodTemplate(ctx context.Context, clients clusterClients) (*corev1.PodTemplateSpec, error) {
	daemonSet, err := clients.daemonSets.Get(ctx, tcfg.namespace, draKubeletPluginApp)
	if err != nil {
		return nil, err
	}
	return &daemonSet.Spec.Template, nil
}

func draControllerPodTemplate(ctx context.Context, clients clusterClients) (*corev1.PodTemplateSpec, error) {
	deployment, err := clients.kube.AppsV1().Deployments(tcfg.namespace).Get(ctx, draControllerApp, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return &deployment.Spec.Template, nil
}

// containerEnv returns the env of the container named containerName, or nil when there is no such container.
func containerEnv(containers []corev1.Container, containerName string) []corev1.EnvVar {
	for _, container := range containers {
		if container.Name == containerName {
			return container.Env
		}
	}
	return nil
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, 0, len(containers))
	for _, container := range containers {
		names = append(names, container.Name)
	}
	return names
}

func haveEnvVar(envVar nvidiav1.EnvVar) gomegatypes.GomegaMatcher {
	return gomega.Equal(corev1.EnvVar{Name: envVar.Name, Value: envVar.Value})
}

// haveFeatureGateEnabled matches the FEATURE_GATES env var, a comma-separated list of Name=bool pairs.
func haveFeatureGateEnabled(featureGate string) gomegatypes.GomegaMatcher {
	return gomega.And(
		gomega.HaveField("Name", featureGatesEnvName),
		gomega.HaveField("Value", gomega.WithTransform(func(featureGates string) []string {
			return strings.Split(featureGates, ",")
		}, gomega.ContainElement(featureGate+"=true"))),
	)
}

func haveTrueErrorCondition(reason, messageSubstring string) gomegatypes.GomegaMatcher {
	return gomega.And(
		gomega.Not(gomega.BeNil()),
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reason),
		gomega.HaveField("Message", gomega.ContainSubstring(messageSubstring)),
	)
}

// waitForDRAContainerEnv waits until container containerName has an env var matching envMatcher, first in
// the pod template returned by getPodTemplate and then in every pod labeled app=app, and those pods are ready.
func waitForDRAContainerEnv(ctx context.Context, clients clusterClients, app, containerName string,
	getPodTemplate func(context.Context, clusterClients) (*corev1.PodTemplateSpec, error), envMatcher gomegatypes.GomegaMatcher) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, daemonSetUpdateTimeout, fmt.Sprintf("waiting for the env of container %s in the %s pod template", containerName, app), func(g gomega.Gomega) {
		podTemplate, err := getPodTemplate(ctx, clients)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(containerEnv(podTemplate.Spec.Containers, containerName)).To(gomega.ContainElement(envMatcher), "container %s", containerName)
	})
	waitFor(ctx, operandReadyTimeout, fmt.Sprintf("waiting for the %s pods to run container %s with the new env", app, containerName), func(g gomega.Gomega) {
		pods, err := clients.pods.List(ctx, tcfg.namespace, appSelector(app))
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(pods).NotTo(gomega.BeEmpty(), "no pods match %q", appSelector(app))
		for _, pod := range pods {
			g.Expect(containerEnv(pod.Spec.Containers, containerName)).To(gomega.ContainElement(envMatcher), "container %s of pod %s", containerName, pod.Name)
			g.Expect(helpers.IsPodReady(&pod)).To(gomega.BeTrue(), "pod %s is not ready", pod.Name)
			g.Expect(pod.DeletionTimestamp).To(gomega.BeNil(), "pod %s is terminating", pod.Name)
		}
	})
}

func waitForDRADriverEnv(ctx context.Context, clients clusterClients, kubeletPluginEnv, controllerEnv nvidiav1.EnvVar) {
	ginkgo.GinkgoHelper()
	waitForDRAContainerEnv(ctx, clients, draKubeletPluginApp, draKubeletPluginGPUsContainer, kubeletPluginPodTemplate, haveEnvVar(kubeletPluginEnv))
	waitForDRAContainerEnv(ctx, clients, draControllerApp, draControllerContainer, draControllerPodTemplate, haveEnvVar(controllerEnv))
}

// waitForKubeletPluginContainers waits until the kubelet plugin pod template and every kubelet plugin pod
// have exactly the containers expectedContainerNames, and those pods are ready.
func waitForKubeletPluginContainers(ctx context.Context, clients clusterClients, expectedContainerNames ...string) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, daemonSetUpdateTimeout, fmt.Sprintf("waiting for the %s pod template to have containers %v", draKubeletPluginApp, expectedContainerNames), func(g gomega.Gomega) {
		podTemplate, err := kubeletPluginPodTemplate(ctx, clients)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(containerNames(podTemplate.Spec.Containers)).To(gomega.ConsistOf(expectedContainerNames))
	})
	waitFor(ctx, operandReadyTimeout, fmt.Sprintf("waiting for the %s pods to run containers %v", draKubeletPluginApp, expectedContainerNames), func(g gomega.Gomega) {
		pods, err := clients.pods.List(ctx, tcfg.namespace, appSelector(draKubeletPluginApp))
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(pods).NotTo(gomega.BeEmpty(), "no pods match %q", appSelector(draKubeletPluginApp))
		for _, pod := range pods {
			g.Expect(containerNames(pod.Spec.Containers)).To(gomega.ConsistOf(expectedContainerNames), "pod %s", pod.Name)
			g.Expect(helpers.IsPodReady(&pod)).To(gomega.BeTrue(), "pod %s is not ready", pod.Name)
			g.Expect(pod.DeletionTimestamp).To(gomega.BeNil(), "pod %s is terminating", pod.Name)
		}
	})
}

func waitForDRAOperandPodLabels(ctx context.Context, clients clusterClients, expectedLabels map[string]string) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, operandReadyTimeout, "waiting for the custom labels on the DRA operand pods", func(g gomega.Gomega) {
		for _, selector := range labeledDRAOperandSelectors {
			pods, err := clients.pods.List(ctx, tcfg.namespace, selector)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(pods).NotTo(gomega.BeEmpty(), "no pods match %q", selector)
			for _, pod := range pods {
				for key, value := range expectedLabels {
					g.Expect(pod.Labels).To(gomega.HaveKeyWithValue(key, value), "pod %s", pod.Name)
				}
			}
		}
	})
}

func deleteSecondGPUCluster(ctx context.Context, clients clusterClients) {
	ginkgo.GinkgoHelper()
	err := clients.gpuClusters.Delete(ctx, secondGPUClusterName)
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete GPUCluster %s", secondGPUClusterName)
	waitForNotFound(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for GPUCluster %s to be deleted", secondGPUClusterName), func(ctx context.Context) error {
		_, err := clients.gpuClusters.Get(ctx, secondGPUClusterName)
		return err
	})
}

// clusterPolicyFromChart renders the ClusterPolicy of the chart under test. driver.nvidiaDriverCRD.enabled
// keeps the NVIDIADriver controller from disabling the default NVIDIADriver while the ClusterPolicy exists.
func clusterPolicyFromChart(ctx context.Context, install *operatorInstall) *nvidiav1.ClusterPolicy {
	ginkgo.GinkgoHelper()
	manifests, err := install.operator.Template(ctx, append(operatorChartValues(),
		"clusterPolicy.deployCR=true",
		"driver.nvidiaDriverCRD.enabled=true",
	))
	framework.ExpectNoError(err, "failed to render chart %s", tcfg.helmChart)

	manifestReader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(manifests)))
	for {
		manifest, err := manifestReader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		framework.ExpectNoError(err, "failed to split the manifests rendered from chart %s", tcfg.helmChart)

		var typeMeta metav1.TypeMeta
		framework.ExpectNoError(yaml.Unmarshal(manifest, &typeMeta), "failed to decode a manifest rendered from chart %s", tcfg.helmChart)
		if typeMeta.Kind != nvidiav1.ClusterPolicyCRDName {
			continue
		}
		clusterPolicy := &nvidiav1.ClusterPolicy{}
		framework.ExpectNoError(yaml.Unmarshal(manifest, clusterPolicy), "failed to decode the ClusterPolicy rendered from chart %s", tcfg.helmChart)
		return clusterPolicy
	}
	ginkgo.Fail(fmt.Sprintf("chart %s rendered no ClusterPolicy", tcfg.helmChart))
	return nil
}

func deleteClusterPolicy(ctx context.Context, clients clusterClients, name string) {
	ginkgo.GinkgoHelper()
	err := clients.clusterPolicies.Delete(ctx, name)
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete ClusterPolicy %s", name)
	waitForNotFound(ctx, gpuClusterReconcileTimeout, fmt.Sprintf("waiting for ClusterPolicy %s to be deleted", name), func(ctx context.Context) error {
		_, err := clients.clusterPolicies.Get(ctx, name)
		return err
	})
}
