// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package healthsync

import (
	"fmt"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/hashicorp/consul-ecs/awsutil"
	"github.com/hashicorp/consul-ecs/config"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/go-multierror"
)

// checkStatus holds the computed status for a Consul health check.
type checkStatus struct {
	consulStatus string // Consul health status (api.HealthPassing or api.HealthCritical)
	output       string // the message to display in Consul
}

// containerHealth holds the health status for a container.
type containerHealth struct {
	ecsStatus string // ecs.HealthStatusHealthy or ecs.HealthStatusUnhealthy
	missing   bool   // true if container not found in task metadata
}

// fetchHealthChecks fetches the Consul health checks for both the service
// and proxy registrations
func (c *Command) fetchHealthChecks(consulClient *api.Client, taskMeta awsutil.ECSTaskMeta) (map[string]*api.HealthCheck, error) {
	serviceName := c.constructServiceName(taskMeta.Family)
	serviceID := makeServiceID(serviceName, taskMeta.TaskID())
	proxySvcID, proxySvcName := makeProxySvcIDAndName(serviceID, serviceName)

	healthCheckMap := make(map[string]*api.HealthCheck)
	var queryOpts *api.QueryOptions
	if c.config.IsGateway() {
		queryOpts = &api.QueryOptions{
			Namespace: c.config.Gateway.Namespace,
			Partition: c.config.Gateway.Partition,
		}
	} else {
		queryOpts = &api.QueryOptions{
			Namespace: c.config.Service.Namespace,
			Partition: c.config.Service.Partition,
		}
	}

	checks, err := getServiceHealthChecks(consulClient, serviceName, serviceID, queryOpts)
	if err != nil {
		return nil, err
	}

	for _, check := range checks {
		healthCheckMap[check.CheckID] = check
	}

	if c.config.IsGateway() {
		return healthCheckMap, nil
	}

	// Get the health checks associated with the sidecar
	checks, err = getServiceHealthChecks(consulClient, proxySvcName, proxySvcID, queryOpts)
	if err != nil {
		return nil, err
	}

	if len(checks) != 1 {
		return nil, fmt.Errorf("only one check should be associated with the sidecar proxy service")
	}

	healthCheckMap[checks[0].CheckID] = checks[0]

	return healthCheckMap, nil
}

// setChecksCritical sets checks for all of the containers to critical.
// Used during graceful shutdown (SIGTERM).
func (c *Command) setChecksCritical(consulClient *api.Client, taskMeta awsutil.ECSTaskMeta, clusterARN string, containerNames []string) error {
	var result error

	serviceName := c.constructServiceName(taskMeta.Family)
	serviceID := makeServiceID(serviceName, taskMeta.TaskID())

	// Create a map with all containers as unhealthy to get all check IDs
	containerStatuses := make(map[string]containerHealth)
	for _, name := range containerNames {
		containerStatuses[name] = containerHealth{ecsStatus: ecs.HealthStatusUnhealthy, missing: false}
	}

	// Use computeCheckStatuses to get all check IDs
	checkStatuses := c.computeCheckStatuses(serviceID, containerNames, containerStatuses)

	// Update all checks to critical with shutdown message
	for checkID := range checkStatuses {
		err := c.updateConsulHealthStatus(consulClient, checkID, clusterARN, checkStatus{
			consulStatus: api.HealthCritical,
			output:       "Graceful shutdown in progress",
		})
		if err != nil {
			c.log.Warn("failed to set Consul health status to critical", "err", err, "checkID", checkID)
			result = multierror.Append(result, err)
		} else {
			c.log.Info("set Consul health status to critical", "checkID", checkID)
		}
	}

	return result
}


// computeOverallDataplaneHealth computes the aggregate health status.
// Returns UNHEALTHY if any container is unhealthy or missing.
func computeOverallDataplaneHealth(containerStatuses map[string]containerHealth) string {
	if len(containerStatuses) == 0 {
		// This should not be possible in practice since containerNames always
		// includes at least the dataplane container. Treat as unhealthy to be safe.
		return ecs.HealthStatusUnhealthy
	}

	for _, health := range containerStatuses {
		if health.ecsStatus != ecs.HealthStatusHealthy {
			return ecs.HealthStatusUnhealthy
		}
	}
	return ecs.HealthStatusHealthy
}

// computeCheckStatuses computes the desired Consul health status and output message for each check.
// Returns a map of checkID -> checkStatus containing both Consul status and output message.
func (c *Command) computeCheckStatuses(serviceID string, containerNames []string, containerStatuses map[string]containerHealth) map[string]checkStatus {
	checkStatuses := make(map[string]checkStatus)

	// Overall dataplane health is the aggregate of all container statuses
	overallECSHealth := computeOverallDataplaneHealth(containerStatuses)
	overallConsulHealth := ecsHealthToConsulHealth(overallECSHealth)
	dataplaneOutput := fmt.Sprintf("Aggregate ECS health status is %q", overallECSHealth)

	for _, name := range containerNames {
		if name == config.ConsulDataplaneContainerName {
			// Dataplane container maps to overall health on service check
			serviceCheckID := constructCheckID(serviceID, name)
			checkStatuses[serviceCheckID] = checkStatus{
				consulStatus: overallConsulHealth,
				output:       dataplaneOutput,
			}

			// Non-gateways also have a proxy check
			if !c.config.IsGateway() {
				proxySvcID, _ := makeProxySvcIDAndName(serviceID, "")
				proxyCheckID := constructCheckID(proxySvcID, name)
				checkStatuses[proxyCheckID] = checkStatus{
					consulStatus: overallConsulHealth,
					output:       dataplaneOutput,
				}
			}
		} else {
			// Non-dataplane containers map directly to their individual check
			checkID := constructCheckID(serviceID, name)
			health := containerStatuses[name]

			var output string
			if health.missing {
				output = fmt.Sprintf("Container %q not found in ECS task metadata", name)
			} else {
				output = fmt.Sprintf("ECS health status is %q for container %q", health.ecsStatus, checkID)
			}

			checkStatuses[checkID] = checkStatus{
				consulStatus: ecsHealthToConsulHealth(health.ecsStatus),
				output:       output,
			}
		}
	}

	return checkStatuses
}

// syncChecks fetches ECS task metadata and updates Consul health checks
// for the specified containers. Checks are only updated when their status
// has changed since the last invocation.
func (c *Command) syncChecks(consulClient *api.Client,
	previousStatuses map[string]checkStatus,
	clusterARN string,
	containerNames []string) map[string]checkStatus {

	// Phase 1: Gather current container state
	taskMeta, err := awsutil.ECSTaskMetadata()
	if err != nil {
		c.log.Error("unable to get task metadata", "err", err)
		return previousStatuses
	}

	serviceName := c.constructServiceName(taskMeta.Family)
	serviceID := makeServiceID(serviceName, taskMeta.TaskID())
	containerStatuses := getContainerHealthStatuses(containerNames, taskMeta)

	// Phase 2: Turn ecs container health into consul checks
	currentStatuses := c.computeCheckStatuses(serviceID, containerNames, containerStatuses)

	// Phase 3: Update Consul for any checks that have changed
	for checkID, status := range currentStatuses {
		previousStatus := previousStatuses[checkID]
		if status == previousStatus {
			continue
		}

		err := c.updateConsulHealthStatus(consulClient, checkID, clusterARN, status)
		if err != nil {
			c.log.Warn("failed to update Consul health status", "err", err, "checkID", checkID)
			// Keep the previous status on error so we retry next cycle
			currentStatuses[checkID] = previousStatus
		} else {
			c.log.Info("health check updated in Consul", "checkID", checkID, "status", status.consulStatus)
		}
	}

	// Phase 4: Return current status
	return currentStatuses
}

func (c *Command) updateConsulHealthStatus(consulClient *api.Client, checkID string, clusterARN string, status checkStatus) error {
	check, ok := c.checks[checkID]
	if !ok {
		return fmt.Errorf("unable to find check with ID %s", checkID)
	}

	check.Status = status.consulStatus
	check.Output = status.output
	c.checks[checkID] = check

	updateCheckReq := &api.CatalogRegistration{
		Node:           clusterARN,
		SkipNodeUpdate: true,
		Checks:         api.HealthChecks{check},
	}

	_, err := consulClient.Catalog().Register(updateCheckReq, nil)
	return err
}

func getServiceHealthChecks(consulClient *api.Client, serviceName, serviceID string, opts *api.QueryOptions) (api.HealthChecks, error) {
	opts.Filter = fmt.Sprintf("ServiceID == `%s`", serviceID)
	checks, _, err := consulClient.Health().Checks(serviceName, opts)
	if err != nil {
		return nil, err
	}

	return checks, nil
}

func constructCheckID(serviceID, containerName string) string {
	return fmt.Sprintf("%s-%s", serviceID, containerName)
}

// getContainerHealthStatuses builds a map of container name to health status.
// Missing containers are marked as unhealthy with missing=true.
func getContainerHealthStatuses(containerNames []string, taskMeta awsutil.ECSTaskMeta) map[string]containerHealth {
	statuses := make(map[string]containerHealth)

	// Build a lookup map from task metadata
	taskContainers := make(map[string]string)
	for _, container := range taskMeta.Containers {
		status := container.Health.Status
		if status == "" {
			status = ecs.HealthStatusUnknown
		}
		taskContainers[container.Name] = status
	}

	// Map each requested container to its status
	for _, name := range containerNames {
		if status, found := taskContainers[name]; found {
			statuses[name] = containerHealth{ecsStatus: status, missing: false}
		} else {
			statuses[name] = containerHealth{ecsStatus: ecs.HealthStatusUnhealthy, missing: true}
		}
	}

	return statuses
}

func ecsHealthToConsulHealth(ecsHealth string) string {
	// `HEALTHY`, `UNHEALTHY`, and `UNKNOWN` are the valid ECS health statuses.
	// This assumes that the only passing status is `HEALTHY`
	if ecsHealth != ecs.HealthStatusHealthy {
		return api.HealthCritical
	}
	return api.HealthPassing
}
