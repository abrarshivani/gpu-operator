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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
	"github.com/NVIDIA/gpu-operator/internal/image"
	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	e2elog "github.com/NVIDIA/gpu-operator/tests/e2e/framework/logs"
	"github.com/NVIDIA/gpu-operator/tests/e2e/helpers"
)

const (
	claimWorkloadDrainTimeout = 15 * time.Minute
	crdEstablishedTimeout     = time.Minute
	// Longer than claimWorkloadDrainTimeout, so a workload that ends on its own cannot pass as drained.
	driverChangeWorkloadDuration = 30 * time.Minute

	driverChangeWorkloadName = e2eTestPrefix + "-driver-change"

	gpuOperatorCRDGroup                 = "nvidia.com"
	gpuOperatorApp                      = "gpu-operator"
	gpuOperatorContainer                = "gpu-operator"
	draKubeletPluginValidationContainer = "driver-validation"
	draValidatorContainer               = "nvidia-dra-validator"
)

var nvidiaSMIDriverVersionCommand = []string{"nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader"}

var _ = ginkgo.Describe(e2eTestPrefix+"-gpucluster-driver-change", ginkgo.Label("gpucluster", "single-gpu", "disruptive"), ginkgo.Ordered, ginkgo.Serial, func() {
	var defaultDriverVersion string

	ginkgo.BeforeAll(func(ctx context.Context) {
		defaultDriverVersion = chartDefaultDriverVersion(ctx)
		gomega.Expect(defaultDriverVersion).NotTo(gomega.Equal(tcfg.targetDriverVersion),
			"-target-driver-version is the chart default driver version, so changing to the chart default would not change the driver")
	})

	install := newOperatorInstall(gpuClusterChartValues(fmt.Sprintf("driver.version=%s", tcfg.targetDriverVersion))...)

	ginkgo.It("brings up the DRA operands on the previous driver branch", func(ctx context.Context) {
		waitForGPUClusterReady(ctx, install.clients, operandReadyTimeout)
		waitForDRAOperandsReady(ctx, install.clients)
	})

	ginkgo.It("reports the previous driver version through nvidia-smi in a claim pod", func(ctx context.Context) {
		driverVersion := nvidiaSMIDriverVersion(ctx, install.clients, e2eTestPrefix+"-previous-driver-version")
		gomega.Expect(driverVersion).To(gomega.Equal(tcfg.targetDriverVersion))
	})

	ginkgo.It("drains a running claim workload before restarting the driver on the chart default version", func(ctx context.Context) {
		clients := install.clients
		// The upgrade controller does not delete GPU pods without a controller unless
		// gpuPodDeletion.force is set, and fails the node upgrade instead, so the workload runs as a Job.
		claimName := driverChangeWorkloadName + "-gpu"
		createGPUResourceClaim(ctx, clients, claimName)
		createWorkloadJob(ctx, clients, newLongRunningCUDAJob(driverChangeWorkloadName, claimName, driverChangeWorkloadDuration))
		workloadPod := waitForJobPodRunning(ctx, clients, driverChangeWorkloadName)
		expectClaimAllocatedByGPUDriver(ctx, clients, claimName)

		waitForPodsReady(ctx, clients, helpers.DriverComponentSelector)
		restartWatch := watchDriverRestart(ctx, clients, &workloadPod, defaultDriverVersion)

		err := clients.drivers.UpdateDriverVersion(ctx, consts.DefaultNVIDIADriverName, defaultDriverVersion)
		framework.ExpectNoError(err, "failed to update the version of NVIDIADriver/%s to %s", consts.DefaultNVIDIADriverName, defaultDriverVersion)

		restartWatch.expectWorkloadDrainedBeforeDriverRestart(ctx)
	})

	ginkgo.It("rolls the driver out to the chart default version", func(ctx context.Context) {
		clients := install.clients
		listDefaultDriverDaemonSets := func(ctx context.Context) ([]appsv1.DaemonSet, error) {
			return clients.drivers.OwnedDaemonSets(ctx, tcfg.namespace, consts.DefaultNVIDIADriverName)
		}
		waitForDriverImageVersion(ctx, daemonSetUpdateTimeout, listDefaultDriverDaemonSets, defaultDriverVersion)
		waitForDriverPodsOnVersion(ctx, clients, defaultDriverVersion)
		waitForDriverUpgradeDone(ctx, clients)
		waitForGPUClusterReady(ctx, clients, operandReadyTimeout)
		waitForDRAOperandsReady(ctx, clients)
	})

	ginkgo.It("reports the chart default driver version through nvidia-smi in a claim pod", func(ctx context.Context) {
		driverVersion := nvidiaSMIDriverVersion(ctx, install.clients, e2eTestPrefix+"-default-driver-version")
		gomega.Expect(driverVersion).To(gomega.Equal(defaultDriverVersion))
	})

	ginkgo.It("leaves every GPU node schedulable", func(ctx context.Context) {
		gpuNodes, err := install.clients.nodes.ListGPUNodes(ctx)
		framework.ExpectNoError(err, "failed to list the GPU nodes")
		gomega.Expect(gpuNodes).NotTo(gomega.BeEmpty(), "no GPU nodes found")
		for _, node := range gpuNodes {
			gomega.Expect(node.Spec.Unschedulable).To(gomega.BeFalse(), "node %s is cordoned", node.Name)
		}
	})

	ginkgo.It("republishes the GPU in a ResourceSlice", func(ctx context.Context) {
		waitForGPUResourceSlices(ctx, install.clients)
	})

	ginkgo.It("runs a GPU workload with a new claim", func(ctx context.Context) {
		runGPUClaimWorkload(ctx, install.clients)
	})
})

var _ = ginkgo.Describe(e2eTestPrefix+"-gpucluster-upgrade", ginkgo.Label("gpucluster", "single-gpu", "disruptive"), ginkgo.Ordered, ginkgo.Serial, func() {
	ginkgo.BeforeAll(func() {
		if tcfg.releaseHelmChart == "" {
			ginkgo.Skip("-release-helm-chart is not set, so there is no release to upgrade from")
		}
	})

	// CRDs outlive an uninstall and helm install leaves existing CRDs alone, so after another container
	// the release would run on the chart under test's CRDs and a missing CRD upgrade would go unnoticed.
	ginkgo.BeforeAll(func(ctx context.Context) {
		clients := newClusterClients()
		releaseOperator := newOperatorClient(tcfg.releaseHelmChart, tcfg.releaseHelmChartVersion)
		chartUnderTestOperator := newOperatorClient(tcfg.helmChart, tcfg.helmChartVersion)
		// The containers that follow install the chart under test, which keeps whatever CRDs it finds.
		ginkgo.DeferCleanup(applyChartCRDs, clients, chartUnderTestOperator, gpuOperatorCRDGroup)

		applyChartCRDs(ctx, clients, releaseOperator, gpuOperatorCRDGroup)
		expectCRDSchemasMatchChart(ctx, clients, releaseOperator, gpuOperatorCRDGroup)
		if maps.Equal(chartCRDSchemas(ctx, releaseOperator, gpuOperatorCRDGroup), chartCRDSchemas(ctx, chartUnderTestOperator, gpuOperatorCRDGroup)) {
			e2elog.Logf("WARN: charts %s and %s have the same %s CRD schemas, so a missing CRD upgrade cannot be detected",
				tcfg.releaseHelmChart, tcfg.helmChart, gpuOperatorCRDGroup)
		}
	})

	install := newReleaseInstall(gpuClusterChartValues()...)

	ginkgo.It("brings up the GPUCluster of the latest release", func(ctx context.Context) {
		waitForGPUClusterReady(ctx, install.clients, operandReadyTimeout)
	})

	ginkgo.It("runs a GPU workload with a claim on the latest release", func(ctx context.Context) {
		runGPUClaimWorkload(ctx, install.clients)
	})

	ginkgo.It("upgrades the release to the chart under test", func(ctx context.Context) {
		install.upgradeToChartUnderTest(ctx)
	})

	ginkgo.It("updates the nvidia.com CRDs to the chart under test", func(ctx context.Context) {
		expectCRDSchemasMatchChart(ctx, install.clients, install.operator, gpuOperatorCRDGroup)
	})

	ginkgo.It("runs the operator under test", func(ctx context.Context) {
		operatorImage := fmt.Sprintf("%s/%s:%s", tcfg.operatorRepository, tcfg.operatorImage, tcfg.operatorVersion)
		waitForContainerImages(ctx, install.clients, []containerImageExpectation{
			{selector: appSelector(gpuOperatorApp), containerName: gpuOperatorContainer, image: operatorImage},
		})
	})

	ginkgo.It("runs the driver version of the upgraded default NVIDIADriver", func(ctx context.Context) {
		clients := install.clients
		chartDriverVersion := chartUnderTestOperandValues(ctx).Driver.Version
		defaultDriver, err := clients.drivers.Get(ctx, consts.DefaultNVIDIADriverName)
		framework.ExpectNoError(err, "failed to get NVIDIADriver/%s", consts.DefaultNVIDIADriverName)
		gomega.Expect(defaultDriver.Spec.Version).To(gomega.Equal(chartDriverVersion),
			"NVIDIADriver/%s does not have the driver version of chart %s", consts.DefaultNVIDIADriverName, tcfg.helmChart)
		waitForDriverPodsOnVersion(ctx, clients, chartDriverVersion)
		waitForDriverUpgradeDone(ctx, clients)
	})

	ginkgo.It("runs the DRA operands on the images of the upgraded GPUCluster", func(ctx context.Context) {
		clients := install.clients
		operandValues := chartUnderTestOperandValues(ctx)
		gpuCluster, err := clients.gpuClusters.Get(ctx, gpuClusterName)
		framework.ExpectNoError(err, "failed to get GPUCluster/%s", gpuClusterName)

		draDriverSpec := gpuCluster.Spec.DRADriver
		gomega.Expect(chartImageValues{Repository: draDriverSpec.Repository, Image: draDriverSpec.Image, Version: draDriverSpec.Version}).
			To(gomega.Equal(operandValues.DRADriver), "GPUCluster/%s does not name the DRA driver image of chart %s", gpuClusterName, tcfg.helmChart)
		dcgmExporterSpec := gpuCluster.Spec.DCGMExporter
		gomega.Expect(dcgmExporterSpec).NotTo(gomega.BeNil(), "GPUCluster/%s has no DCGM exporter spec", gpuClusterName)
		gomega.Expect(chartImageValues{Repository: dcgmExporterSpec.Repository, Image: dcgmExporterSpec.Image, Version: dcgmExporterSpec.Version}).
			To(gomega.Equal(operandValues.DCGMExporter), "GPUCluster/%s does not name the DCGM exporter image of chart %s", gpuClusterName, tcfg.helmChart)

		waitForContainerImages(ctx, clients, draOperandImageExpectations(gpuCluster))
	})

	ginkgo.It("reports the GPUCluster ready after the upgrade", func(ctx context.Context) {
		waitForGPUClusterReady(ctx, install.clients, operandReadyTimeout)
	})

	ginkgo.It("runs a GPU workload with a claim after the upgrade", func(ctx context.Context) {
		runGPUClaimWorkload(ctx, install.clients)
	})
})

// chartDefaultDriverVersion renders the chart under test and returns the version of its default NVIDIADriver.
func chartDefaultDriverVersion(ctx context.Context) string {
	ginkgo.GinkgoHelper()
	chartValues := []string{"driver.nvidiaDriverCRD.enabled=true", "driver.nvidiaDriverCRD.deployDefaultCR=true"}
	manifests, err := newOperatorClient(tcfg.helmChart, tcfg.helmChartVersion).Template(ctx, chartValues)
	framework.ExpectNoError(err, "failed to render chart %s with %v", tcfg.helmChart, chartValues)

	var defaultDriverVersions []string
	for manifest := range strings.SplitSeq(manifests, "\n---") {
		if !strings.Contains(manifest, "kind: NVIDIADriver") {
			continue
		}
		var driver nvidiav1alpha1.NVIDIADriver
		framework.ExpectNoError(yaml.Unmarshal([]byte(manifest), &driver), "failed to decode a rendered NVIDIADriver")
		if driver.Name == consts.DefaultNVIDIADriverName {
			defaultDriverVersions = append(defaultDriverVersions, driver.Spec.Version)
		}
	}
	gomega.Expect(defaultDriverVersions).To(gomega.HaveLen(1), "chart %s does not render exactly one NVIDIADriver/%s", tcfg.helmChart, consts.DefaultNVIDIADriverName)
	gomega.Expect(defaultDriverVersions[0]).NotTo(gomega.BeEmpty(), "NVIDIADriver/%s has no version", consts.DefaultNVIDIADriverName)
	return defaultDriverVersions[0]
}

// nvidiaSMIDriverVersion returns the driver version that nvidia-smi reports in a pod with a claim for one GPU.
func nvidiaSMIDriverVersion(ctx context.Context, clients clusterClients, podName string) string {
	ginkgo.GinkgoHelper()
	return strings.TrimSpace(runClaimPod(ctx, clients, podName, nvidiaSMIDriverVersionCommand))
}

// newLongRunningCUDAJob returns a Job whose single pod keeps the GPU of ResourceClaim claimName busy for
// duration. A pod the driver upgrade deletes counts as failed, so with no retries it is not replaced.
func newLongRunningCUDAJob(name, claimName string, duration time.Duration) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gpuWorkloadNamespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32(0)),
			Template: corev1.PodTemplateSpec{
				Spec: newLongRunningCUDAPod(name, claimName, duration).Spec,
			},
		},
	}
}

// createWorkloadJob creates job and registers its deletion together with its pods.
func createWorkloadJob(ctx context.Context, clients clusterClients, job *batchv1.Job) {
	ginkgo.GinkgoHelper()
	_, err := clients.kube.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create job %s/%s", job.Namespace, job.Name)
	ginkgo.DeferCleanup(deleteWorkloadJob, clients, job.Name)
}

func deleteWorkloadJob(ctx context.Context, clients clusterClients, name string) {
	ginkgo.GinkgoHelper()
	jobs := clients.kube.BatchV1().Jobs(gpuWorkloadNamespace)
	err := jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationForeground)})
	framework.ExpectNoError(client.IgnoreNotFound(err), "failed to delete job %s/%s", gpuWorkloadNamespace, name)
	waitForNotFound(ctx, podDeletionTimeout, fmt.Sprintf("waiting for job %s/%s and its pods to be deleted", gpuWorkloadNamespace, name), func(ctx context.Context) error {
		_, err := jobs.Get(ctx, name, metav1.GetOptions{})
		return err
	})
}

// waitForJobPodRunning waits until job jobName has exactly one pod and that pod runs, and returns the pod.
func waitForJobPodRunning(ctx context.Context, clients clusterClients, jobName string) corev1.Pod {
	ginkgo.GinkgoHelper()
	jobPodSelector := fmt.Sprintf("%s=%s", batchv1.JobNameLabel, jobName)
	var runningPod corev1.Pod
	waitFor(ctx, gpuWorkloadTimeout, fmt.Sprintf("waiting for the pod of job %s/%s to run", gpuWorkloadNamespace, jobName), func(g gomega.Gomega) {
		jobPods, err := clients.pods.List(ctx, gpuWorkloadNamespace, jobPodSelector)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(jobPods).To(gomega.HaveLen(1))
		if phase := jobPods[0].Status.Phase; phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			gomega.StopTrying(fmt.Sprintf("pod %s/%s ended in phase %s", gpuWorkloadNamespace, jobPods[0].Name, phase)).Now()
		}
		g.Expect(jobPods[0].Status.Phase).To(gomega.Equal(corev1.PodRunning))
		runningPod = jobPods[0]
	})
	return runningPod
}

// isDriverPodOnVersion matches the driver image tags the operator builds: <version>-<os> and, for precompiled
// drivers, <version>-<kernel>-<os>. A substring match would take 580.12-ubuntu22.04 for version 580.1.
func isDriverPodOnVersion(pod *corev1.Pod, version string) bool {
	driverImageTag := imageTag(pod.Spec.Containers[0].Image)
	return driverImageTag == version || strings.HasPrefix(driverImageTag, version+"-")
}

// imageTag returns the tag of imageReference, ignoring any digest, or "" when it has no tag.
// The tag colon is the last one after the last '/', since a registry host may carry a port.
func imageTag(imageReference string) string {
	imageReference, _, _ = strings.Cut(imageReference, "@")
	tagSeparator := strings.LastIndex(imageReference, ":")
	if tagSeparator <= strings.LastIndex(imageReference, "/") {
		return ""
	}
	return imageReference[tagSeparator+1:]
}

// driverRestartWatch follows a GPU workload pod and the driver pods through a single pod watch, whose events
// arrive in the order the API server applied them. Polling cannot order a deletion it did not see.
type driverRestartWatch struct {
	workloadPod           *corev1.Pod
	newDriverVersion      string
	driverPodSelector     labels.Selector
	previousDriverPodUIDs map[types.UID]bool
	podWatch              watch.Interface
}

// watchDriverRestart lists all pods and watches them from the resourceVersion of that list, so that no event
// after the listed state is missed. Call it before the driver version changes to newDriverVersion.
func watchDriverRestart(ctx context.Context, clients clusterClients, workloadPod *corev1.Pod, newDriverVersion string) *driverRestartWatch {
	ginkgo.GinkgoHelper()
	driverPodSelector, err := labels.Parse(helpers.DriverComponentSelector)
	framework.ExpectNoError(err, "failed to parse the driver pod selector %q", helpers.DriverComponentSelector)
	restartWatch := &driverRestartWatch{
		workloadPod:           workloadPod,
		newDriverVersion:      newDriverVersion,
		driverPodSelector:     driverPodSelector,
		previousDriverPodUIDs: make(map[types.UID]bool),
	}

	podList, err := clients.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "failed to list the pods")
	workloadPodListed := false
	for _, pod := range podList.Items {
		if pod.UID == workloadPod.UID {
			workloadPodListed = true
		} else if restartWatch.isDriverPod(&pod) {
			restartWatch.previousDriverPodUIDs[pod.UID] = true
		}
	}
	gomega.Expect(workloadPodListed).To(gomega.BeTrue(), "pod %s/%s is gone before the driver version changes", workloadPod.Namespace, workloadPod.Name)
	gomega.Expect(restartWatch.previousDriverPodUIDs).NotTo(gomega.BeEmpty(), "no driver pods found")
	for _, pod := range podList.Items {
		restartWatch.observePod(&pod, false)
	}

	restartWatch.podWatch, err = clients.kube.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{ResourceVersion: podList.ResourceVersion})
	framework.ExpectNoError(err, "failed to watch the pods from resourceVersion %s", podList.ResourceVersion)
	ginkgo.DeferCleanup(restartWatch.podWatch.Stop)
	return restartWatch
}

func (w *driverRestartWatch) isDriverPod(pod *corev1.Pod) bool {
	return pod.Namespace == tcfg.namespace && w.driverPodSelector.Matches(labels.Set(pod.Labels))
}

// expectWorkloadDrainedBeforeDriverRestart waits for the event that deletes the workload pod. It fails as soon as an
// earlier event deletes a previous driver pod, creates a driver pod on the new version, or ends the workload pod
// without a deletion.
func (w *driverRestartWatch) expectWorkloadDrainedBeforeDriverRestart(ctx context.Context) {
	ginkgo.GinkgoHelper()
	ginkgo.By(fmt.Sprintf("waiting for the driver upgrade to drain pod %s/%s before restarting the driver", w.workloadPod.Namespace, w.workloadPod.Name))
	drainCtx, cancelDrain := context.WithTimeout(ctx, claimWorkloadDrainTimeout)
	defer cancelDrain()

	// A watch that ends early is not resumed: a new list shows only the latest state of each pod, so an order
	// violation in the gap would go unseen.
	for {
		select {
		case <-drainCtx.Done():
			ginkgo.Fail(fmt.Sprintf("pod %s/%s was not drained within %s: %v", w.workloadPod.Namespace, w.workloadPod.Name, claimWorkloadDrainTimeout, drainCtx.Err()))
		case event, open := <-w.podWatch.ResultChan():
			if !open {
				ginkgo.Fail(fmt.Sprintf("the pod watch closed before pod %s/%s was drained, so the drain order is unknown", w.workloadPod.Namespace, w.workloadPod.Name))
			}
			switch event.Type {
			case watch.Error:
				ginkgo.Fail(fmt.Sprintf("the pod watch failed before pod %s/%s was drained, so the drain order is unknown: %v",
					w.workloadPod.Namespace, w.workloadPod.Name, apierrors.FromObject(event.Object)))
			case watch.Added, watch.Modified, watch.Deleted:
				pod, isPod := event.Object.(*corev1.Pod)
				gomega.Expect(isPod).To(gomega.BeTrue(), "the pod watch sent a %T", event.Object)
				if w.observePod(pod, event.Type == watch.Deleted) {
					return
				}
			}
		}
	}
}

// observePod fails if the state of pod breaks the drain order, and reports whether it is the deleted workload pod.
func (w *driverRestartWatch) observePod(pod *corev1.Pod, podDeleted bool) (workloadPodDeleted bool) {
	ginkgo.GinkgoHelper()
	switch {
	case pod.UID == w.workloadPod.UID:
		if podDeleted {
			return true
		}
		if pod.DeletionTimestamp == nil && (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) {
			ginkgo.Fail(fmt.Sprintf("pod %s/%s ended in phase %s instead of being drained", pod.Namespace, pod.Name, pod.Status.Phase))
		}
	case !w.isDriverPod(pod):
	case w.previousDriverPodUIDs[pod.UID]:
		if podDeleted || pod.DeletionTimestamp != nil {
			ginkgo.Fail(fmt.Sprintf("driver pod %s was deleted while pod %s/%s was still on the node", pod.Name, w.workloadPod.Namespace, w.workloadPod.Name))
		}
	case isDriverPodOnVersion(pod, w.newDriverVersion):
		ginkgo.Fail(fmt.Sprintf("driver pod %s on version %s was created while pod %s/%s was still on the node",
			pod.Name, w.newDriverVersion, w.workloadPod.Namespace, w.workloadPod.Name))
	}
	return false
}

// waitForDriverPodsOnVersion waits until every driver pod runs version and is ready.
func waitForDriverPodsOnVersion(ctx context.Context, clients clusterClients, version string) {
	ginkgo.GinkgoHelper()
	waitFor(ctx, driverUpgradeTimeout, fmt.Sprintf("waiting for the driver pods to run version %s", version), func(g gomega.Gomega) {
		driverPods, err := clients.pods.List(ctx, tcfg.namespace, helpers.DriverComponentSelector)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(driverPods).NotTo(gomega.BeEmpty(), "no driver pods found")
		for _, driverPod := range driverPods {
			g.Expect(isDriverPodOnVersion(&driverPod, version)).To(gomega.BeTrue(), "pod %s runs %s", driverPod.Name, driverPod.Spec.Containers[0].Image)
			g.Expect(helpers.IsPodReady(&driverPod)).To(gomega.BeTrue(), "pod %s is not ready", driverPod.Name)
			g.Expect(driverPod.DeletionTimestamp).To(gomega.BeNil(), "pod %s is terminating", driverPod.Name)
		}
	})
}

// chartCRDsInGroup returns the CRDs of group that the chart of operator ships.
func chartCRDsInGroup(ctx context.Context, operator *helpers.OperatorClient, group string) []apiextensionsv1.CustomResourceDefinition {
	ginkgo.GinkgoHelper()
	crdManifests, err := operator.ShowCRDs(ctx)
	framework.ExpectNoError(err, "failed to read the CRDs of chart %s", operator.Chart)

	var groupCRDs []apiextensionsv1.CustomResourceDefinition
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(crdManifests), 4096)
	for {
		var crd apiextensionsv1.CustomResourceDefinition
		err := decoder.Decode(&crd)
		if errors.Is(err, io.EOF) {
			break
		}
		framework.ExpectNoError(err, "failed to decode the CRDs of chart %s", operator.Chart)
		if crd.Spec.Group == group {
			groupCRDs = append(groupCRDs, crd)
		}
	}
	return groupCRDs
}

// applyChartCRDs creates the chart's CRDs in group, or replaces the spec of the ones that exist, and waits
// until they are established.
func applyChartCRDs(ctx context.Context, clients clusterClients, operator *helpers.OperatorClient, group string) {
	ginkgo.GinkgoHelper()
	chartCRDs := chartCRDsInGroup(ctx, operator, group)
	gomega.Expect(chartCRDs).NotTo(gomega.BeEmpty(), "chart %s has no %s CRDs", operator.Chart, group)

	crds := clients.apiExtensions.ApiextensionsV1().CustomResourceDefinitions()
	for _, chartCRD := range chartCRDs {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			clusterCRD, err := crds.Get(ctx, chartCRD.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				_, err = crds.Create(ctx, &chartCRD, metav1.CreateOptions{})
				return err
			}
			if err != nil {
				return err
			}
			clusterCRD.Spec = chartCRD.Spec
			_, err = crds.Update(ctx, clusterCRD, metav1.UpdateOptions{})
			return err
		})
		framework.ExpectNoError(err, "failed to apply CRD %s of chart %s", chartCRD.Name, operator.Chart)

		waitFor(ctx, crdEstablishedTimeout, fmt.Sprintf("waiting for CRD %s to be established", chartCRD.Name), func(g gomega.Gomega) {
			clusterCRD, err := crds.Get(ctx, chartCRD.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(clusterCRD.Status.Conditions).To(gomega.ContainElement(gomega.And(
				gomega.HaveField("Type", apiextensionsv1.Established),
				gomega.HaveField("Status", apiextensionsv1.ConditionTrue),
			)), "CRD %s is not established", chartCRD.Name)
		})
	}
}

// chartCRDSchemas returns the encoded schema of every served version of the chart's CRDs in group, keyed by
// CRD name and version.
func chartCRDSchemas(ctx context.Context, operator *helpers.OperatorClient, group string) map[string]string {
	ginkgo.GinkgoHelper()
	schemasByCRDVersion := make(map[string]string)
	for _, chartCRD := range chartCRDsInGroup(ctx, operator, group) {
		for _, chartVersion := range chartCRD.Spec.Versions {
			if !chartVersion.Served {
				continue
			}
			schema, err := json.Marshal(chartVersion.Schema)
			framework.ExpectNoError(err, "failed to encode the schema of CRD %s version %s in chart %s", chartCRD.Name, chartVersion.Name, operator.Chart)
			schemasByCRDVersion[chartCRD.Name+"/"+chartVersion.Name] = string(schema)
		}
	}
	return schemasByCRDVersion
}

// expectCRDSchemasMatchChart checks that every version of the chart's CRDs in group is in the cluster with
// the same served and storage flags, and that every served version has the same schema.
func expectCRDSchemasMatchChart(ctx context.Context, clients clusterClients, operator *helpers.OperatorClient, group string) {
	ginkgo.GinkgoHelper()
	chartCRDs := chartCRDsInGroup(ctx, operator, group)
	gomega.Expect(chartCRDs).NotTo(gomega.BeEmpty(), "chart %s has no %s CRDs", operator.Chart, group)

	for _, chartCRD := range chartCRDs {
		clusterCRD, err := clients.apiExtensions.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, chartCRD.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get CRD %s", chartCRD.Name)
		for _, chartVersion := range chartCRD.Spec.Versions {
			versionIndex := slices.IndexFunc(clusterCRD.Spec.Versions, func(clusterVersion apiextensionsv1.CustomResourceDefinitionVersion) bool {
				return clusterVersion.Name == chartVersion.Name
			})
			gomega.Expect(versionIndex).NotTo(gomega.Equal(-1), "CRD %s in the cluster does not have version %s", chartCRD.Name, chartVersion.Name)
			clusterVersion := clusterCRD.Spec.Versions[versionIndex]
			gomega.Expect(clusterVersion.Served).To(gomega.Equal(chartVersion.Served), "CRD %s version %s in the cluster has a different served flag than the chart", chartCRD.Name, chartVersion.Name)
			gomega.Expect(clusterVersion.Storage).To(gomega.Equal(chartVersion.Storage), "CRD %s version %s in the cluster has a different storage flag than the chart", chartCRD.Name, chartVersion.Name)
			if !chartVersion.Served {
				continue
			}

			chartSchema, err := json.Marshal(chartVersion.Schema)
			framework.ExpectNoError(err, "failed to encode the schema of CRD %s version %s in the chart", chartCRD.Name, chartVersion.Name)
			clusterSchema, err := json.Marshal(clusterVersion.Schema)
			framework.ExpectNoError(err, "failed to encode the schema of CRD %s version %s in the cluster", chartCRD.Name, chartVersion.Name)
			gomega.Expect(string(clusterSchema)).To(gomega.MatchJSON(chartSchema), "CRD %s version %s in the cluster differs from the chart", chartCRD.Name, chartVersion.Name)
		}
	}
}

type chartImageValues struct {
	Repository string `json:"repository"`
	Image      string `json:"image"`
	Version    string `json:"version"`
}

// chartOperandValues are the chart values that set the version of the default NVIDIADriver and the
// operand images of the GPUCluster.
type chartOperandValues struct {
	Driver struct {
		Version string `json:"version"`
	} `json:"driver"`
	DRADriver    chartImageValues `json:"draDriver"`
	DCGMExporter chartImageValues `json:"dcgmExporter"`
}

// chartUnderTestOperandValues returns the operand values that upgradeToChartUnderTest renders: the chart's
// defaults under -values-file, because --reset-then-reuse-values reapplies only the release's --set values,
// and none of those set an operand image or the driver version.
func chartUnderTestOperandValues(ctx context.Context) chartOperandValues {
	ginkgo.GinkgoHelper()
	defaultValues, err := newOperatorClient(tcfg.helmChart, tcfg.helmChartVersion).ShowValues(ctx)
	framework.ExpectNoError(err, "failed to read the values of chart %s", tcfg.helmChart)
	var operandValues chartOperandValues
	framework.ExpectNoError(yaml.Unmarshal([]byte(defaultValues), &operandValues), "failed to decode the values of chart %s", tcfg.helmChart)

	if tcfg.valuesFile != "" {
		valuesFileContent, err := os.ReadFile(tcfg.valuesFile)
		framework.ExpectNoError(err, "failed to read values file %s", tcfg.valuesFile)
		// Decoding into the chart's values keeps the fields the file leaves out, as helm does.
		framework.ExpectNoError(yaml.Unmarshal(valuesFileContent, &operandValues), "failed to decode values file %s", tcfg.valuesFile)
	}
	return operandValues
}

type containerImageExpectation struct {
	selector      string
	containerName string
	image         string
}

// draOperandImageExpectations returns the images that the DRA operand containers run for gpuCluster: the DRA
// driver and DCGM exporter images it names, and the validator under test.
func draOperandImageExpectations(gpuCluster *nvidiav1alpha1.GPUCluster) []containerImageExpectation {
	ginkgo.GinkgoHelper()
	draDriverSpec := gpuCluster.Spec.DRADriver
	draDriverImage, err := image.ImagePath(draDriverSpec.Repository, draDriverSpec.Image, draDriverSpec.Version, "")
	framework.ExpectNoError(err, "GPUCluster/%s does not name a DRA driver image", gpuCluster.Name)

	dcgmExporterSpec := gpuCluster.Spec.DCGMExporter
	gomega.Expect(dcgmExporterSpec).NotTo(gomega.BeNil(), "GPUCluster/%s has no DCGM exporter spec", gpuCluster.Name)
	dcgmExporterImage, err := image.ImagePath(dcgmExporterSpec.Repository, dcgmExporterSpec.Image, dcgmExporterSpec.Version, "")
	framework.ExpectNoError(err, "GPUCluster/%s does not name a DCGM exporter image", gpuCluster.Name)

	validatorImage := fmt.Sprintf("%s/%s:%s", tcfg.validatorRepository, tcfg.validatorImage, tcfg.validatorVersion)

	imageExpectations := []containerImageExpectation{
		{selector: appSelector(draKubeletPluginApp), containerName: draKubeletPluginGPUsContainer, image: draDriverImage},
		{selector: appSelector(draKubeletPluginApp), containerName: draKubeletPluginValidationContainer, image: validatorImage},
		{selector: appSelector(draDCGMExporterApp), containerName: draDCGMExporterContainer, image: dcgmExporterImage},
		{selector: draValidatorSelector, containerName: draValidatorContainer, image: validatorImage},
	}
	if draDriverSpec.IsComputeDomainsEnabled() {
		imageExpectations = append(imageExpectations,
			containerImageExpectation{selector: appSelector(draKubeletPluginApp), containerName: draKubeletPluginComputeContainer, image: draDriverImage},
			containerImageExpectation{selector: appSelector(draControllerApp), containerName: draControllerContainer, image: draDriverImage},
		)
	}
	return imageExpectations
}

// waitForContainerImages waits until, for every expectation, at least one pod matches its selector and every
// matching pod is ready, not terminating, and runs the expected image in the named container.
func waitForContainerImages(ctx context.Context, clients clusterClients, imageExpectations []containerImageExpectation) {
	ginkgo.GinkgoHelper()
	for _, imageExpectation := range imageExpectations {
		description := fmt.Sprintf("waiting for container %s of pods %q to run %s", imageExpectation.containerName, imageExpectation.selector, imageExpectation.image)
		waitFor(ctx, operandReadyTimeout, description, func(g gomega.Gomega) {
			pods, err := clients.pods.List(ctx, tcfg.namespace, imageExpectation.selector)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(pods).NotTo(gomega.BeEmpty(), "no pods match %q", imageExpectation.selector)
			for _, pod := range pods {
				containers := slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers)
				containerIndex := slices.IndexFunc(containers, func(container corev1.Container) bool {
					return container.Name == imageExpectation.containerName
				})
				g.Expect(containerIndex).NotTo(gomega.Equal(-1), "pod %s has no container %s", pod.Name, imageExpectation.containerName)
				g.Expect(containers[containerIndex].Image).To(gomega.Equal(imageExpectation.image), "pod %s container %s", pod.Name, imageExpectation.containerName)
				g.Expect(helpers.IsPodReady(&pod)).To(gomega.BeTrue(), "pod %s is not ready", pod.Name)
				g.Expect(pod.DeletionTimestamp).To(gomega.BeNil(), "pod %s is terminating", pod.Name)
			}
		})
	}
}
