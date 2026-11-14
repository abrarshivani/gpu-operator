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

package operands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/NVIDIA/gpu-operator/tests/e2e/framework"
	e2eoperator "github.com/NVIDIA/gpu-operator/tests/e2e/framework/operator"
)

const (
	GPUDRADriverName   = "gpu.nvidia.com"
	GPUDeviceClassName = "gpu.nvidia.com"

	deviceClassCreationTimeout         = 2 * time.Minute
	deviceClassDeletionTimeout         = 2 * time.Minute
	gpuResourceSlicePublicationTimeout = 5 * time.Minute
	draResourceSliceCleanupTimeout     = 5 * time.Minute

	gpuDeviceType                                       = "gpu"
	gpuDeviceTypeAttributeName resourcev1.QualifiedName = "type"
)

func WaitForDeviceClass(ctx context.Context, clients e2eoperator.Clients, deviceClassName string) {
	ginkgo.GinkgoHelper()
	creationDescription := fmt.Sprintf("Waiting for DeviceClass %s to be created", deviceClassName)
	elapsed := framework.WaitFor(ctx, deviceClassCreationTimeout, creationDescription, func(g gomega.Gomega) {
		deviceClass, err := clients.Kube.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to get DeviceClass %s", deviceClassName)
		g.Expect(deviceClass.DeletionTimestamp).To(gomega.BeNil(), "DeviceClass %s is being deleted", deviceClassName)
	})
	ginkgo.GinkgoWriter.Printf("DeviceClass %s exists (%s)\n", deviceClassName, elapsed.Round(time.Second))
}

func WaitForDeviceClassDeleted(ctx context.Context, clients e2eoperator.Clients, deviceClassName string) {
	ginkgo.GinkgoHelper()
	framework.WaitForNotFound(ctx, deviceClassDeletionTimeout, "DeviceClass "+deviceClassName, func(ctx context.Context) error {
		_, err := clients.Kube.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
		return err
	})
}

func WaitForGPUResourceSlices(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoHelper()
	var gpuNodeCount int
	publicationDescription := "Waiting for the DRA driver to publish the GPUs of every GPU node"
	elapsed := framework.WaitFor(ctx, gpuResourceSlicePublicationTimeout, publicationDescription, func(g gomega.Gomega) {
		gpuNodes, err := clients.Nodes.ListGPUNodes(ctx)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to list the GPU nodes")
		if len(gpuNodes) == 0 {
			gomega.StopTrying("no GPU nodes found").Now()
		}
		devicesByNodeName, err := gpuResourceSliceDevicesByNode(ctx, clients)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to list the ResourceSlices of %s", GPUDRADriverName)
		for _, node := range gpuNodes {
			deviceTypes := gpuDeviceTypes(devicesByNodeName[node.Name])
			g.Expect(deviceTypes).To(gomega.ContainElement(gpuDeviceType), "%s", describeMissingGPUDevice(node.Name, deviceTypes))
		}
		gpuNodeCount = len(gpuNodes)
	})
	ginkgo.GinkgoWriter.Printf("The DRA driver publishes the GPUs of %d GPU node(s) (%s)\n", gpuNodeCount, elapsed.Round(time.Second))
}

func describeMissingGPUDevice(nodeName string, publishedDeviceTypes []string) string {
	if len(publishedDeviceTypes) == 0 {
		return fmt.Sprintf("node %s has no typed devices in a ResourceSlice yet; the DRA kubelet plugin has not published them", nodeName)
	}
	return fmt.Sprintf("node %s publishes no GPU yet, only device types %s", nodeName, strings.Join(publishedDeviceTypes, ", "))
}

func ListGPUResourceSlices(ctx context.Context, clients e2eoperator.Clients) ([]resourcev1.ResourceSlice, error) {
	resourceSlices, err := clients.Kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorDriver, GPUDRADriverName).String(),
	})
	if err != nil {
		return nil, err
	}
	return resourceSlices.Items, nil
}

func gpuResourceSliceDevicesByNode(ctx context.Context, clients e2eoperator.Clients) (map[string][]resourcev1.Device, error) {
	resourceSlices, err := ListGPUResourceSlices(ctx, clients)
	if err != nil {
		return nil, err
	}
	devicesByNodeName := map[string][]resourcev1.Device{}
	for _, resourceSlice := range resourceSlices {
		if resourceSlice.Spec.NodeName == nil {
			continue
		}
		nodeName := *resourceSlice.Spec.NodeName
		devicesByNodeName[nodeName] = append(devicesByNodeName[nodeName], resourceSlice.Spec.Devices...)
	}
	return devicesByNodeName, nil
}

func WaitForGPUResourceSlicesDeleted(ctx context.Context, clients e2eoperator.Clients) {
	ginkgo.GinkgoHelper()
	deletionDescription := fmt.Sprintf("Waiting for the ResourceSlices of DRA driver %s to be deleted", GPUDRADriverName)
	elapsed := framework.WaitFor(ctx, draResourceSliceCleanupTimeout, deletionDescription, func(g gomega.Gomega) {
		resourceSlices, err := ListGPUResourceSlices(ctx, clients)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to list the ResourceSlices of %s", GPUDRADriverName)
		g.Expect(resourceSlices).To(gomega.BeEmpty(), "ResourceSlices of DRA driver %s still exist", GPUDRADriverName)
	})
	ginkgo.GinkgoWriter.Printf("The DRA driver's ResourceSlices have been deleted (%s)\n", elapsed.Round(time.Second))
}

func gpuDeviceTypes(devices []resourcev1.Device) []string {
	deviceTypes := make([]string, 0, len(devices))
	for _, device := range devices {
		if deviceType := device.Attributes[gpuDeviceTypeAttributeName].StringValue; deviceType != nil {
			deviceTypes = append(deviceTypes, *deviceType)
		}
	}
	return deviceTypes
}
