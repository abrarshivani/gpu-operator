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

package clusterinfo

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetOpenshiftDTKImages(t *testing.T) {
	testCases := []struct {
		description                 string
		handlers                    map[string]http.HandlerFunc
		expectedDriverToolkitImages map[string]string
	}{
		{
			description: "every usable tag becomes a map entry",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTag("410.84", "quay.io/openshift/driver-toolkit@sha256:aaa"),
					imageStreamTag("412.86", "quay.io/openshift/driver-toolkit@sha256:bbb"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{
				"410.84": "quay.io/openshift/driver-toolkit@sha256:aaa",
				"412.86": "quay.io/openshift/driver-toolkit@sha256:bbb",
			},
		},
		{
			description: "the floating latest tag is skipped",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTag("latest", "quay.io/openshift/driver-toolkit@sha256:zzz"),
					imageStreamTag("410.84", "quay.io/openshift/driver-toolkit@sha256:aaa"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{"410.84": "quay.io/openshift/driver-toolkit@sha256:aaa"},
		},
		{
			// Only the exact name is the floating tag; a release-specific name that merely starts
			// with "latest" is a real DriverToolkit build and has to survive the filter.
			description: "a tag whose name only begins with latest is kept",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTag("latest-rhel9", "quay.io/openshift/driver-toolkit@sha256:ccc"),
					imageStreamTag("410.84", "quay.io/openshift/driver-toolkit@sha256:aaa"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{
				"latest-rhel9": "quay.io/openshift/driver-toolkit@sha256:ccc",
				"410.84":       "quay.io/openshift/driver-toolkit@sha256:aaa",
			},
		},
		{
			// Nothing filters on From.Kind, so a tag pointing at another tag of the stream is kept
			// and its map entry is that tag reference rather than a pullable image.
			description: "a tag pointing at an ImageStreamTag is kept with that reference",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTagReferencingTag("410.84", "driver-toolkit:v4.10"),
					imageStreamTag("412.86", "quay.io/openshift/driver-toolkit@sha256:bbb"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{
				"410.84": "driver-toolkit:v4.10",
				"412.86": "quay.io/openshift/driver-toolkit@sha256:bbb",
			},
		},
		{
			description: "a tag without a From reference is skipped",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTagWithoutFrom("410.84"),
					imageStreamTag("412.86", "quay.io/openshift/driver-toolkit@sha256:bbb"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{"412.86": "quay.io/openshift/driver-toolkit@sha256:bbb"},
		},
		{
			description: "a tag with an empty name is skipped, see RHBZ#2015024",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTag("", "quay.io/openshift/driver-toolkit@sha256:zzz"),
					imageStreamTag("410.84", "quay.io/openshift/driver-toolkit@sha256:aaa"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{"410.84": "quay.io/openshift/driver-toolkit@sha256:aaa"},
		},
		{
			// The map is allocated lazily, so a stream whose tags are all filtered out yields nil
			// rather than an empty map.
			description: "only a latest tag yields a nil map",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTag("latest", "quay.io/openshift/driver-toolkit@sha256:zzz"),
				)),
			},
			expectedDriverToolkitImages: nil,
		},
		{
			description: "an ImageStream with no tags yields a nil map",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream()),
			},
			expectedDriverToolkitImages: nil,
		},
		{
			description: "a server error yields a nil map",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondServerError(),
			},
			expectedDriverToolkitImages: nil,
		},
		{
			description: "forbidden yields a nil map",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondForbidden(),
			},
			expectedDriverToolkitImages: nil,
		},
		{
			description: "duplicate tag names collapse to the last one",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTag("410.84", "quay.io/openshift/driver-toolkit@sha256:aaa"),
					imageStreamTag("410.84", "quay.io/openshift/driver-toolkit@sha256:bbb"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{"410.84": "quay.io/openshift/driver-toolkit@sha256:bbb"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			ctx := testContext(t)
			apiServer := newStubAPIServer(t, testCase.handlers)

			driverToolkitImages := getOpenshiftDTKImages(ctx, apiServer.restConfig)

			require.Equal(t, testCase.expectedDriverToolkitImages, driverToolkitImages)

			// Pins both consts.OpenshiftNamespace and the hard-coded "driver-toolkit" name.
			assertRequestedPathSequence(t, apiServer, []string{pathDriverToolkitImageStream})
		})
	}

	t.Run("client construction failure yields a nil map", func(t *testing.T) {
		require.Nil(t, getOpenshiftDTKImages(testContext(t), brokenRESTConfig()))
	})
}

func TestClusterInfoGetOpenshiftDriverToolkitImages(t *testing.T) {
	testCases := []struct {
		description                 string
		oneshot                     bool
		cachedDriverToolkitImages   map[string]string
		handlers                    map[string]http.HandlerFunc
		expectedDriverToolkitImages map[string]string
		expectedRequestPaths        []string
	}{
		{
			description:                 "oneshot returns the cached map without any API call",
			oneshot:                     true,
			cachedDriverToolkitImages:   map[string]string{"410.84": "quay.io/openshift/driver-toolkit@sha256:aaa"},
			expectedDriverToolkitImages: map[string]string{"410.84": "quay.io/openshift/driver-toolkit@sha256:aaa"},
		},
		{
			description:                 "oneshot returns a nil cached map",
			oneshot:                     true,
			cachedDriverToolkitImages:   nil,
			expectedDriverToolkitImages: nil,
		},
		{
			description: "non-oneshot queries the cluster",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondWithJSON(http.StatusOK, driverToolkitImageStream(
					imageStreamTag("410.84", "quay.io/openshift/driver-toolkit@sha256:aaa"),
					imageStreamTag("412.86", "quay.io/openshift/driver-toolkit@sha256:bbb"),
				)),
			},
			expectedDriverToolkitImages: map[string]string{
				"410.84": "quay.io/openshift/driver-toolkit@sha256:aaa",
				"412.86": "quay.io/openshift/driver-toolkit@sha256:bbb",
			},
			expectedRequestPaths: []string{pathDriverToolkitImageStream},
		},
		{
			description: "non-oneshot returns nil when the ImageStream is missing",
			handlers: map[string]http.HandlerFunc{
				pathDriverToolkitImageStream: respondNotFound(),
			},
			expectedDriverToolkitImages: nil,
			expectedRequestPaths:        []string{pathDriverToolkitImageStream},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			clusterInfoUnderTest := &clusterInfo{
				ctx:                          testContext(t),
				oneshot:                      testCase.oneshot,
				openshiftDriverToolkitImages: testCase.cachedDriverToolkitImages,
			}
			var apiServer *stubAPIServer
			if testCase.handlers != nil {
				apiServer = newStubAPIServer(t, testCase.handlers)
				clusterInfoUnderTest.config = apiServer.restConfig
			}

			driverToolkitImages := clusterInfoUnderTest.GetOpenshiftDriverToolkitImages()

			require.Equal(t, testCase.expectedDriverToolkitImages, driverToolkitImages)

			if apiServer != nil {
				assertRequestedPathSequence(t, apiServer, testCase.expectedRequestPaths)
			}
		})
	}
}
