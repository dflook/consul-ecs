// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package healthsync

import (
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/hashicorp/consul-ecs/awsutil"
	"github.com/hashicorp/consul-ecs/config"
	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/require"
)

func TestEcsHealthToConsulHealth(t *testing.T) {
	require.Equal(t, api.HealthPassing, ecsHealthToConsulHealth(ecs.HealthStatusHealthy))
	require.Equal(t, api.HealthCritical, ecsHealthToConsulHealth(ecs.HealthStatusUnknown))
	require.Equal(t, api.HealthCritical, ecsHealthToConsulHealth(ecs.HealthStatusUnhealthy))
	require.Equal(t, api.HealthCritical, ecsHealthToConsulHealth(""))
}

func TestGetContainerHealthStatuses(t *testing.T) {
	cases := map[string]struct {
		containerNames []string
		taskMeta       awsutil.ECSTaskMeta
		expected       map[string]containerHealth
	}{
		"all containers present and healthy": {
			containerNames: []string{"app", "sidecar"},
			taskMeta: awsutil.ECSTaskMeta{
				Containers: []awsutil.ECSTaskMetaContainer{
					{Name: "app", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusHealthy}},
					{Name: "sidecar", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusHealthy}},
				},
			},
			expected: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
		},
		"one container unhealthy": {
			containerNames: []string{"app", "sidecar"},
			taskMeta: awsutil.ECSTaskMeta{
				Containers: []awsutil.ECSTaskMetaContainer{
					{Name: "app", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusHealthy}},
					{Name: "sidecar", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusUnhealthy}},
				},
			},
			expected: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
			},
		},
		"container missing from metadata": {
			containerNames: []string{"app", "sidecar"},
			taskMeta: awsutil.ECSTaskMeta{
				Containers: []awsutil.ECSTaskMetaContainer{
					{Name: "app", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusHealthy}},
				},
			},
			expected: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusUnhealthy, missing: true},
			},
		},
		"all containers missing": {
			containerNames: []string{"app", "sidecar"},
			taskMeta:       awsutil.ECSTaskMeta{},
			expected: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusUnhealthy, missing: true},
				"sidecar": {ecsStatus: ecs.HealthStatusUnhealthy, missing: true},
			},
		},
		"empty container list": {
			containerNames: []string{},
			taskMeta: awsutil.ECSTaskMeta{
				Containers: []awsutil.ECSTaskMetaContainer{
					{Name: "app", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusHealthy}},
				},
			},
			expected: map[string]containerHealth{},
		},
		"extra containers in metadata ignored": {
			containerNames: []string{"app"},
			taskMeta: awsutil.ECSTaskMeta{
				Containers: []awsutil.ECSTaskMetaContainer{
					{Name: "app", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusHealthy}},
					{Name: "extra", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusHealthy}},
				},
			},
			expected: map[string]containerHealth{
				"app": {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
		},
		"unknown status preserved": {
			containerNames: []string{"app"},
			taskMeta: awsutil.ECSTaskMeta{
				Containers: []awsutil.ECSTaskMetaContainer{
					{Name: "app", Health: awsutil.ECSTaskMetaHealth{Status: ecs.HealthStatusUnknown}},
				},
			},
			expected: map[string]containerHealth{
				"app": {ecsStatus: ecs.HealthStatusUnknown, missing: false},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := getContainerHealthStatuses(tc.containerNames, tc.taskMeta)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestComputeOverallDataplaneHealth(t *testing.T) {
	cases := map[string]struct {
		containerStatuses map[string]containerHealth
		expected          string
	}{
		"all healthy": {
			containerStatuses: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expected: ecs.HealthStatusHealthy,
		},
		"one unhealthy": {
			containerStatuses: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
			},
			expected: ecs.HealthStatusUnhealthy,
		},
		"one unknown": {
			containerStatuses: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusUnknown, missing: false},
			},
			expected: ecs.HealthStatusUnhealthy,
		},
		"all unhealthy": {
			containerStatuses: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
			},
			expected: ecs.HealthStatusUnhealthy,
		},
		"one missing": {
			containerStatuses: map[string]containerHealth{
				"app":     {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				"sidecar": {ecsStatus: ecs.HealthStatusUnhealthy, missing: true},
			},
			expected: ecs.HealthStatusUnhealthy,
		},
		"empty map treated as unhealthy": {
			// This should not happen in practice since containerNames always
			// includes at least the dataplane container. Treated as unhealthy to be safe.
			containerStatuses: map[string]containerHealth{},
			expected:          ecs.HealthStatusUnhealthy,
		},
		"single healthy": {
			containerStatuses: map[string]containerHealth{
				"app": {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expected: ecs.HealthStatusHealthy,
		},
		"single unhealthy": {
			containerStatuses: map[string]containerHealth{
				"app": {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
			},
			expected: ecs.HealthStatusUnhealthy,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := computeOverallDataplaneHealth(tc.containerStatuses)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestComputeCheckStatuses(t *testing.T) {
	const (
		serviceID          = "test-service-12345"
		dataplaneContainer = config.ConsulDataplaneContainerName
	)

	// Expected check IDs for non-gateway
	serviceCheckID := constructCheckID(serviceID, dataplaneContainer)
	proxySvcID, _ := makeProxySvcIDAndName(serviceID, "")
	proxyCheckID := constructCheckID(proxySvcID, dataplaneContainer)
	appCheckID := constructCheckID(serviceID, "app")

	cases := map[string]struct {
		isGateway              bool
		containerNames         []string
		containerStatuses      map[string]containerHealth
		expectedConsulStatuses map[string]string
		expectedOutputs        map[string]string
	}{
		"non-gateway all healthy": {
			isGateway:      false,
			containerNames: []string{"app", dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				"app":              {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				dataplaneContainer: {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				appCheckID:     api.HealthPassing,
				serviceCheckID: api.HealthPassing,
				proxyCheckID:   api.HealthPassing,
			},
			expectedOutputs: map[string]string{
				appCheckID:     fmt.Sprintf("ECS health status is %q for container %q", ecs.HealthStatusHealthy, appCheckID),
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusHealthy),
				proxyCheckID:   fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusHealthy),
			},
		},
		"non-gateway app unhealthy affects overall health": {
			isGateway:      false,
			containerNames: []string{"app", dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				"app":              {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
				dataplaneContainer: {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				appCheckID:     api.HealthCritical,
				serviceCheckID: api.HealthCritical,
				proxyCheckID:   api.HealthCritical,
			},
			expectedOutputs: map[string]string{
				appCheckID:     fmt.Sprintf("ECS health status is %q for container %q", ecs.HealthStatusUnhealthy, appCheckID),
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusUnhealthy),
				proxyCheckID:   fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusUnhealthy),
			},
		},
		"non-gateway app missing affects overall health": {
			isGateway:      false,
			containerNames: []string{"app", dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				"app":              {ecsStatus: ecs.HealthStatusUnhealthy, missing: true},
				dataplaneContainer: {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				appCheckID:     api.HealthCritical,
				serviceCheckID: api.HealthCritical,
				proxyCheckID:   api.HealthCritical,
			},
			expectedOutputs: map[string]string{
				appCheckID:     fmt.Sprintf("Container %q not found in ECS task metadata", "app"),
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusUnhealthy),
				proxyCheckID:   fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusUnhealthy),
			},
		},
		"non-gateway dataplane unhealthy": {
			isGateway:      false,
			containerNames: []string{"app", dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				"app":              {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				dataplaneContainer: {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				appCheckID:     api.HealthPassing,
				serviceCheckID: api.HealthCritical,
				proxyCheckID:   api.HealthCritical,
			},
			expectedOutputs: map[string]string{
				appCheckID:     fmt.Sprintf("ECS health status is %q for container %q", ecs.HealthStatusHealthy, appCheckID),
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusUnhealthy),
				proxyCheckID:   fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusUnhealthy),
			},
		},
		"non-gateway dataplane only": {
			isGateway:      false,
			containerNames: []string{dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				dataplaneContainer: {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				serviceCheckID: api.HealthPassing,
				proxyCheckID:   api.HealthPassing,
			},
			expectedOutputs: map[string]string{
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusHealthy),
				proxyCheckID:   fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusHealthy),
			},
		},
		"gateway healthy": {
			isGateway:      true,
			containerNames: []string{dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				dataplaneContainer: {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				serviceCheckID: api.HealthPassing,
			},
			expectedOutputs: map[string]string{
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusHealthy),
			},
		},
		"gateway unhealthy": {
			isGateway:      true,
			containerNames: []string{dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				dataplaneContainer: {ecsStatus: ecs.HealthStatusUnhealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				serviceCheckID: api.HealthCritical,
			},
			expectedOutputs: map[string]string{
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusUnhealthy),
			},
		},
		"gateway no proxy check": {
			isGateway:      true,
			containerNames: []string{"app", dataplaneContainer},
			containerStatuses: map[string]containerHealth{
				"app":              {ecsStatus: ecs.HealthStatusHealthy, missing: false},
				dataplaneContainer: {ecsStatus: ecs.HealthStatusHealthy, missing: false},
			},
			expectedConsulStatuses: map[string]string{
				appCheckID:     api.HealthPassing,
				serviceCheckID: api.HealthPassing,
			},
			expectedOutputs: map[string]string{
				appCheckID:     fmt.Sprintf("ECS health status is %q for container %q", ecs.HealthStatusHealthy, appCheckID),
				serviceCheckID: fmt.Sprintf("Aggregate ECS health status is %q", ecs.HealthStatusHealthy),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := &Command{
				config: &config.Config{},
			}
			if tc.isGateway {
				cmd.config.Gateway = &config.GatewayRegistration{
					Kind: api.ServiceKindMeshGateway,
				}
			}

			result := cmd.computeCheckStatuses(serviceID, tc.containerNames, tc.containerStatuses)

			// Check consul statuses
			for checkID, expectedStatus := range tc.expectedConsulStatuses {
				require.Equal(t, expectedStatus, result[checkID].consulStatus, "consul status mismatch for %s", checkID)
			}

			// Check output messages
			for checkID, expectedOutput := range tc.expectedOutputs {
				require.Equal(t, expectedOutput, result[checkID].output, "output mismatch for %s", checkID)
			}

			// Verify no extra checks
			require.Len(t, result, len(tc.expectedConsulStatuses))
		})
	}
}
