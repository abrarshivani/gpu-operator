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
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

type OperatorClient struct {
	Chart        string
	ChartVersion string
	Namespace    string
	Kubeconfig   string
}

type ChartOptions struct {
	ReleaseName string
	Timeout     time.Duration
	ValuesFile  string
	Wait        bool
	// ResetThenReuseValues and DisableOpenAPIValidation apply to Upgrade only. ResetThenReuseValues
	// applies the release's values over the new chart's defaults, where --reuse-values keeps the
	// previous chart's defaults.
	ResetThenReuseValues     bool
	DisableOpenAPIValidation bool
}

func (op *OperatorClient) Install(ctx context.Context, params []string, chartOpts ChartOptions) (string, error) {
	args, err := op.helmArgs("install", params, chartOpts)
	if err != nil {
		return "", err
	}

	if _, err := runHelm(ctx, args); err != nil {
		return "", fmt.Errorf("error installing operator: %w", err)
	}

	return chartOpts.ReleaseName, nil
}

func (op *OperatorClient) Upgrade(ctx context.Context, params []string, chartOpts ChartOptions) error {
	args, err := op.helmArgs("upgrade", params, chartOpts)
	if err != nil {
		return err
	}
	if chartOpts.ResetThenReuseValues {
		args = append(args, "--reset-then-reuse-values")
	} else {
		args = append(args, "--reuse-values")
	}
	if chartOpts.DisableOpenAPIValidation {
		args = append(args, "--disable-openapi-validation")
	}

	if _, err := runHelm(ctx, args); err != nil {
		return fmt.Errorf("error upgrading release %s: %w", chartOpts.ReleaseName, err)
	}

	return nil
}

// Template renders the chart locally and returns the manifests.
func (op *OperatorClient) Template(ctx context.Context, params []string) (string, error) {
	args, err := op.helmArgs("template", params, ChartOptions{ReleaseName: "gpu-operator"})
	if err != nil {
		return "", err
	}

	manifests, err := runHelm(ctx, args)
	if err != nil {
		return "", fmt.Errorf("error rendering chart: %w", err)
	}

	return manifests, nil
}

// ShowCRDs returns the CustomResourceDefinitions in the chart's crds directory.
func (op *OperatorClient) ShowCRDs(ctx context.Context) (string, error) {
	if op.Chart == "" {
		return "", fmt.Errorf("chart must be provided")
	}

	args := []string{"show", "crds", op.Chart}
	if op.ChartVersion != "" {
		args = append(args, "--version", op.ChartVersion)
	}

	crds, err := runHelm(ctx, args)
	if err != nil {
		return "", fmt.Errorf("error showing the CRDs of chart %s: %w", op.Chart, err)
	}

	return crds, nil
}

// ShowValues returns the chart's default values.
func (op *OperatorClient) ShowValues(ctx context.Context) (string, error) {
	if op.Chart == "" {
		return "", fmt.Errorf("chart must be provided")
	}

	args := []string{"show", "values", op.Chart}
	if op.ChartVersion != "" {
		args = append(args, "--version", op.ChartVersion)
	}

	values, err := runHelm(ctx, args)
	if err != nil {
		return "", fmt.Errorf("error showing the values of chart %s: %w", op.Chart, err)
	}

	return values, nil
}

func (op *OperatorClient) Uninstall(ctx context.Context, releaseName string) error {
	// Install can fail before helm records the release.
	args := []string{"uninstall", releaseName, "-n", op.Namespace, "--kubeconfig", op.Kubeconfig, "--ignore-not-found"}

	if _, err := runHelm(ctx, args); err != nil {
		return fmt.Errorf("error uninstalling release %s: %w", releaseName, err)
	}

	return nil
}

func (op *OperatorClient) helmArgs(action string, params []string, chartOpts ChartOptions) ([]string, error) {
	if op.Chart == "" {
		return nil, fmt.Errorf("chart must be provided")
	}
	if chartOpts.ReleaseName == "" {
		return nil, fmt.Errorf("release name must be provided")
	}

	args := []string{action, chartOpts.ReleaseName, op.Chart}
	args = append(args, "-n", op.Namespace, "--kubeconfig", op.Kubeconfig)

	if op.ChartVersion != "" {
		args = append(args, "--version", op.ChartVersion)
	}

	if chartOpts.ValuesFile != "" {
		args = append(args, "-f", chartOpts.ValuesFile)
	}

	for _, param := range params {
		args = append(args, "--set", param)
	}

	if chartOpts.Wait {
		args = append(args, "--wait")
	}

	if chartOpts.Timeout > 0 {
		args = append(args, "--timeout", chartOpts.Timeout.String())
	}

	return args, nil
}

func runHelm(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}

	return stdout.String(), nil
}
