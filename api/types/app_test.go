/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package types

import (
	"fmt"
	"testing"

	"github.com/gravitational/teleport/api/constants"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"
)

// TestAppPublicAddrValidation tests PublicAddr field validation to make sure that
// an app with internal "kube." ServerName prefix won't be created.
func TestAppPublicAddrValidation(t *testing.T) {
	type check func(t *testing.T, err error)

	hasNoErr := func() check {
		return func(t *testing.T, err error) {
			require.NoError(t, err)
		}
	}
	hasErrTypeBadParameter := func() check {
		return func(t *testing.T, err error) {
			require.IsType(t, &trace.BadParameterError{}, err.(*trace.TraceErr).OrigError())
		}
	}

	tests := []struct {
		name       string
		publicAddr string
		check      check
	}{
		{
			name:       "kubernetes app",
			publicAddr: "kubernetes.example.com:3080",
			check:      hasNoErr(),
		},
		{
			name:       "kubernetes app public addr without port",
			publicAddr: "kubernetes.example.com",
			check:      hasNoErr(),
		},
		{
			name:       "kubernetes app http",
			publicAddr: "http://kubernetes.example.com:3080",
			check:      hasNoErr(),
		},
		{
			name:       "kubernetes app https",
			publicAddr: "https://kubernetes.example.com:3080",
			check:      hasNoErr(),
		},
		{
			name:       "public address with internal kube ServerName prefix",
			publicAddr: "kube.example.com:3080",
			check:      hasErrTypeBadParameter(),
		},
		{
			name:       "https public address with internal kube ServerName prefix",
			publicAddr: "https://kube.example.com:3080",
			check:      hasErrTypeBadParameter(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAppV3(Metadata{
				Name: "TestApp",
			}, AppSpecV3{
				PublicAddr: tc.publicAddr,
				URI:        "localhost:3080",
			})
			tc.check(t, err)
		})
	}
}

func TestAppServerSorter(t *testing.T) {
	t.Parallel()

	testValsUnordered := []string{"d", "b", "a", "c"}

	makeServers := func(testVals []string, testField string) []AppServer {
		servers := make([]AppServer, len(testVals))
		for i := 0; i < len(testVals); i++ {
			testVal := testVals[i]
			var err error
			servers[i], err = NewAppServerV3(Metadata{
				Name: "_",
			}, AppServerSpecV3{
				HostID: "_",
				App: &AppV3{
					Metadata: Metadata{
						Name:        getTestVal(testField == ResourceMetadataName, testVal),
						Description: getTestVal(testField == ResourceSpecDescription, testVal),
					},
					Spec: AppSpecV3{
						URI:        "_",
						PublicAddr: getTestVal(testField == ResourceSpecPublicAddr, testVal),
					},
				},
			})
			require.NoError(t, err)
		}
		return servers
	}

	cases := []struct {
		name      string
		fieldName string
	}{
		{
			name:      "by name",
			fieldName: ResourceMetadataName,
		},
		{
			name:      "by description",
			fieldName: ResourceSpecDescription,
		},
		{
			name:      "by publicAddr",
			fieldName: ResourceSpecPublicAddr,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("%s desc", c.name), func(t *testing.T) {
			sortBy := SortBy{Field: c.fieldName, IsDesc: true}
			servers := AppServers(makeServers(testValsUnordered, c.fieldName))
			require.NoError(t, servers.SortByCustom(sortBy))
			targetVals, err := servers.GetFieldVals(c.fieldName)
			require.NoError(t, err)
			require.IsDecreasing(t, targetVals)
		})

		t.Run(fmt.Sprintf("%s asc", c.name), func(t *testing.T) {
			sortBy := SortBy{Field: c.fieldName}
			servers := AppServers(makeServers(testValsUnordered, c.fieldName))
			require.NoError(t, servers.SortByCustom(sortBy))
			targetVals, err := servers.GetFieldVals(c.fieldName)
			require.NoError(t, err)
			require.IsIncreasing(t, targetVals)
		})
	}

	// Test error.
	sortBy := SortBy{Field: "unsupported"}
	servers := makeServers(testValsUnordered, "does-not-matter")
	require.True(t, trace.IsNotImplemented(AppServers(servers).SortByCustom(sortBy)))
}

func TestApplicationGetAWSExternalID(t *testing.T) {
	roleARN := "arn:aws:iam::1234567890:role/test-role"

	tests := []struct {
		name               string
		appAWS             *AppAWS
		expectedExternalID string
	}{
		{
			name: "no AWS config",
		},
		{
			name:   "no external ID map",
			appAWS: &AppAWS{},
		},
		{
			name: "role ARN not found",
			appAWS: &AppAWS{
				ExternalIDMap: map[string]string{},
			},
		},
		{
			name: "role ARN found",
			appAWS: &AppAWS{
				ExternalIDMap: map[string]string{
					roleARN: "external-id-for-role",
				},
			},
			expectedExternalID: "external-id-for-role",
		},
		{
			name: "wildcard found",
			appAWS: &AppAWS{
				ExternalIDMap: map[string]string{
					Wildcard: "default-external-id",
				},
			},
			expectedExternalID: "default-external-id",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app, err := NewAppV3(Metadata{
				Name: "aws",
			}, AppSpecV3{
				URI: constants.AWSConsoleURL,
				AWS: test.appAWS,
			})
			require.NoError(t, err)

			require.Equal(t, test.expectedExternalID, app.GetAWSExternalID(roleARN))
		})
	}
}
