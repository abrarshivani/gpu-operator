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
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	e2eoperands "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operands"
	e2eoperator "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operator"
)

const (
	recentEventCount = 20

	deviceTypeAttributeName resourcev1.QualifiedName = "type"
)

func logDRAFailureDiagnostics(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoWriter.Printf("Collecting DRA diagnostics after the failure\n")
	logWorkloadResourceClaims(ctx, clients)
	logDeviceClasses(ctx, clients)
	logGPUResourceSlices(ctx, clients)
	logGPUClusterStatus(ctx, clients)
	logRecentNamespaceEvents(ctx, clients, e2eoperator.WorkloadNamespace)
	logRecentNamespaceEvents(ctx, clients, e2eoperator.TestContext.OperatorNamespace)
}

func logWorkloadResourceClaims(ctx context.Context, clients e2eoperator.Clients) {
	resourceClaims, err := clients.Kube.ResourceV1().ResourceClaims(e2eoperator.WorkloadNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		ginkgo.GinkgoWriter.Printf("Could not list the ResourceClaims in namespace %s: %v\n", e2eoperator.WorkloadNamespace, err)
		return
	}
	ginkgo.GinkgoWriter.Printf("Found %d ResourceClaim(s) in namespace %s\n", len(resourceClaims.Items), e2eoperator.WorkloadNamespace)
	for _, resourceClaim := range resourceClaims.Items {
		ginkgo.GinkgoWriter.Printf("ResourceClaim %s is %s and %s\n", resourceClaim.Name,
			describeClaimAllocation(resourceClaim.Status.Allocation), describeClaimReservation(resourceClaim.Status.ReservedFor))
	}
}

func describeClaimAllocation(allocation *resourcev1.AllocationResult) string {
	if allocation == nil {
		return "not allocated"
	}
	allocatedDevices := make([]string, 0, len(allocation.Devices.Results))
	for _, deviceAllocation := range allocation.Devices.Results {
		allocatedDevices = append(allocatedDevices, deviceAllocation.Driver+"/"+deviceAllocation.Pool+"/"+deviceAllocation.Device)
	}
	return "allocated " + strings.Join(allocatedDevices, ", ")
}

func describeClaimReservation(consumers []resourcev1.ResourceClaimConsumerReference) string {
	if len(consumers) == 0 {
		return "not reserved"
	}
	consumerNames := make([]string, 0, len(consumers))
	for _, consumer := range consumers {
		consumerNames = append(consumerNames, consumer.Resource+"/"+consumer.Name)
	}
	return "reserved for " + strings.Join(consumerNames, ", ")
}

func logDeviceClasses(ctx context.Context, clients e2eoperator.Clients) {
	for _, deviceClassName := range draDeviceClassNames {
		deviceClass, err := clients.Kube.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			ginkgo.GinkgoWriter.Printf("DeviceClass %s does not exist\n", deviceClassName)
		case err != nil:
			ginkgo.GinkgoWriter.Printf("Could not get DeviceClass %s: %v\n", deviceClassName, err)
		default:
			ginkgo.GinkgoWriter.Printf("DeviceClass %s %s\n", deviceClassName, describeDeviceClassSelectors(deviceClass.Spec.Selectors))
		}
	}
}

func describeDeviceClassSelectors(selectors []resourcev1.DeviceSelector) string {
	celExpressions := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		if selector.CEL != nil {
			celExpressions = append(celExpressions, selector.CEL.Expression)
		}
	}
	if len(celExpressions) == 0 {
		return "has no CEL selector"
	}
	return "selects devices matching " + strings.Join(celExpressions, " and ")
}

func logGPUResourceSlices(ctx context.Context, clients e2eoperator.Clients) {
	resourceSlices, err := e2eoperands.ListGPUResourceSlices(ctx, clients)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("Could not list the ResourceSlices of DRA driver %s: %v\n", e2eoperands.GPUDRADriverName, err)
		return
	}
	ginkgo.GinkgoWriter.Printf("Found %d ResourceSlice(s) of DRA driver %s\n", len(resourceSlices), e2eoperands.GPUDRADriverName)
	slices.SortFunc(resourceSlices, func(first, second resourcev1.ResourceSlice) int {
		byNode := strings.Compare(ptr.Deref(first.Spec.NodeName, ""), ptr.Deref(second.Spec.NodeName, ""))
		if byNode != 0 {
			return byNode
		}
		return strings.Compare(first.Name, second.Name)
	})
	for _, resourceSlice := range resourceSlices {
		nodeName := ptr.Deref(resourceSlice.Spec.NodeName, "<none>")
		if len(resourceSlice.Spec.Devices) == 0 {
			ginkgo.GinkgoWriter.Printf("Node %s publishes no devices in ResourceSlice %s\n", nodeName, resourceSlice.Name)
			continue
		}
		ginkgo.GinkgoWriter.Printf("Node %s publishes %d device(s) in ResourceSlice %s: %s\n", nodeName,
			len(resourceSlice.Spec.Devices), resourceSlice.Name, describeDeviceTypes(resourceSlice.Spec.Devices))
	}
}

func describeDeviceTypes(devices []resourcev1.Device) string {
	deviceDescriptions := make([]string, 0, len(devices))
	for _, device := range devices {
		deviceType := "unknown"
		if deviceTypeValue := device.Attributes[deviceTypeAttributeName].StringValue; deviceTypeValue != nil {
			deviceType = *deviceTypeValue
		}
		deviceDescriptions = append(deviceDescriptions, fmt.Sprintf("%s of type %s", device.Name, deviceType))
	}
	return strings.Join(deviceDescriptions, ", ")
}

func logGPUClusterStatus(ctx context.Context, clients e2eoperator.Clients) {
	gpuClusters, err := clients.GPUClusters.List(ctx)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("Could not list the GPUClusters: %v\n", err)
		return
	}
	if len(gpuClusters.Items) == 0 {
		ginkgo.GinkgoWriter.Printf("Found no GPUCluster\n")
	}
	for _, gpuCluster := range gpuClusters.Items {
		gpuClusterDescriptions := []string{fmt.Sprintf("GPUCluster %s is in state %q", gpuCluster.Name, gpuCluster.Status.State)}
		if gpuCluster.DeletionTimestamp != nil {
			gpuClusterDescriptions = append(gpuClusterDescriptions, describeDeletionState(&gpuCluster))
		}
		for _, condition := range gpuCluster.Status.Conditions {
			gpuClusterDescriptions = append(gpuClusterDescriptions,
				fmt.Sprintf("condition %s=%s (%s): %s", condition.Type, condition.Status, condition.Reason, condition.Message))
		}
		ginkgo.GinkgoWriter.Printf("%s\n", strings.Join(gpuClusterDescriptions, "; "))
	}
}

func logRecentNamespaceEvents(ctx context.Context, clients e2eoperator.Clients, namespace string) {
	events, err := clients.Kube.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		ginkgo.GinkgoWriter.Printf("Could not list the events in namespace %s: %v\n", namespace, err)
		return
	}
	recentEvents := events.Items
	slices.SortFunc(recentEvents, func(first, second corev1.Event) int {
		return eventTime(first).Compare(eventTime(second))
	})
	if len(recentEvents) > recentEventCount {
		recentEvents = recentEvents[len(recentEvents)-recentEventCount:]
	}
	ginkgo.GinkgoWriter.Printf("Showing the last %d of %d event(s) in namespace %s\n", len(recentEvents), len(events.Items), namespace)
	for _, event := range recentEvents {
		ginkgo.GinkgoWriter.Printf("At %s %s %s on %s %s: %s\n", eventTime(event).UTC().Format(time.TimeOnly),
			event.Type, event.Reason, event.InvolvedObject.Kind, event.InvolvedObject.Name, strings.TrimSpace(event.Message))
	}
}

func eventTime(event corev1.Event) time.Time {
	switch {
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	default:
		return event.CreationTimestamp.Time
	}
}
