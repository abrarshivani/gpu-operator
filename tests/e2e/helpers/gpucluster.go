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

package helpers

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	gpuclientset "github.com/NVIDIA/gpu-operator/api/versioned"
)

type GPUClusterClient struct {
	client gpuclientset.Interface
}

func NewGPUClusterClient(client gpuclientset.Interface) *GPUClusterClient {
	return &GPUClusterClient{
		client: client,
	}
}

func (h *GPUClusterClient) Get(ctx context.Context, name string) (*nvidiav1alpha1.GPUCluster, error) {
	return h.client.NvidiaV1alpha1().GPUClusters().Get(ctx, name, metav1.GetOptions{})
}

func (h *GPUClusterClient) List(ctx context.Context) (*nvidiav1alpha1.GPUClusterList, error) {
	return h.client.NvidiaV1alpha1().GPUClusters().List(ctx, metav1.ListOptions{})
}

func (h *GPUClusterClient) Create(ctx context.Context, gpuCluster *nvidiav1alpha1.GPUCluster) (*nvidiav1alpha1.GPUCluster, error) {
	return h.client.NvidiaV1alpha1().GPUClusters().Create(ctx, gpuCluster, metav1.CreateOptions{})
}

func (h *GPUClusterClient) Update(ctx context.Context, gpuCluster *nvidiav1alpha1.GPUCluster) (*nvidiav1alpha1.GPUCluster, error) {
	return h.client.NvidiaV1alpha1().GPUClusters().Update(ctx, gpuCluster, metav1.UpdateOptions{})
}

func (h *GPUClusterClient) Delete(ctx context.Context, name string) error {
	return h.client.NvidiaV1alpha1().GPUClusters().Delete(ctx, name, metav1.DeleteOptions{})
}

// Modify applies mutate to the GPUCluster and updates it, retrying on conflicts with the
// operator's concurrent status updates.
func (h *GPUClusterClient) Modify(ctx context.Context, name string, mutate func(*nvidiav1alpha1.GPUCluster)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		gpuCluster, err := h.Get(ctx, name)
		if err != nil {
			return err
		}

		mutate(gpuCluster)

		_, err = h.Update(ctx, gpuCluster)
		return err
	})
}
