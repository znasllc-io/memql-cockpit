package cli

import (
	"strings"

	"github.com/visionarys-io/memql-cockpit/cli/cluster"
	"github.com/visionarys-io/memql-cockpit/cli/dockerprobe"
	nodev1 "github.com/visionarys-io/memql/component/node/gen"
)

// memqlServiceNames is the set of docker compose service names that
// also register themselves as memQL nodes via v1:cluster:node. We
// skip these on the docker side to avoid duplicating the gRPC-
// reported row.
var memqlServiceNames = map[string]bool{
	"bff":       true,
	"voice":     true,
	"cognition": true,
	"agent":     true,
	"planner":   true,
}

// infraDisplayType remaps the compose service name to the
// topology's nodeType identifier. Renames the ones whose docker
// service name doesn't read well as a topology label.
var infraDisplayType = map[string]string{
	"nginx":       "lb",
	"postgres":    "database",
	"identity":    "identity",
	"redis":       "redis",
	"livekit":     "livekit",
	"voice-agent": "voice-agent",
}

// mergeInfraFromDocker augments existing (the gRPC-derived memql
// nodes) with the infrastructure containers found in the local
// docker daemon. Skips compose services that are already
// represented in existing (so we don't double up on BFF /
// COGNITION / etc.), and skips entirely when no docker daemon /
// compose project is reachable.
func mergeInfraFromDocker(existing []cluster.NodeInfo) []cluster.NodeInfo {
	containers, _ := dockerprobe.Containers()
	if len(containers) == 0 {
		return existing
	}
	for _, c := range containers {
		if memqlServiceNames[c.Service] {
			continue // covered by the gRPC clusterNodes feed already
		}
		displayType, ok := infraDisplayType[c.Service]
		if !ok {
			// Unknown service -- keep it but use the compose service
			// name verbatim so we don't drop new docker-compose
			// additions silently.
			displayType = c.Service
		}
		existing = append(existing, cluster.NodeInfo{
			ID:      c.Name,
			Name:    strings.ToUpper(displayType),
			Type:    displayType,
			Address: c.Name,
			Health:  dockerStateToHealth(c),
		})
	}
	return existing
}

// dockerStateToHealth maps a docker container state + health hint
// to the NodeHealthStatus enum the topology renderer uses for
// border colour. Mirrors the buckets in dockerprobe.iconForContainer
// but typed for the gRPC enum so the same green/amber/red logic
// applies in the topology pane.
func dockerStateToHealth(c dockerprobe.Container) nodev1.NodeHealthStatus {
	switch c.State {
	case "running":
		switch c.HealthHint {
		case "unhealthy":
			return nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED
		case "starting":
			return nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING
		default:
			return nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
		}
	case "paused", "restarting", "created":
		return nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED
	default:
		return nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE
	}
}
