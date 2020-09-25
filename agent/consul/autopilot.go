package consul

import (
	"context"
	"fmt"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/types"
	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"
	"github.com/hashicorp/serf/serf"
)

type AutopilotServerExt struct {
	ReadReplica    bool
	RedundancyZone string
	UpgradeVersion string
}

// AutopilotDelegate is a Consul delegate for autopilot operations.
type AutopilotDelegate struct {
	server *Server
}

func (d *AutopilotDelegate) AutopilotConfig() *autopilot.Config {
	return d.server.getOrCreateAutopilotConfig().ToAutopilotLibraryConfig()
}

func (d *AutopilotDelegate) KnownServers() map[raft.ServerID]*autopilot.Server {
	return d.server.autopilotServers()
}

func (d *AutopilotDelegate) FetchServerStats(ctx context.Context, servers map[raft.ServerID]*autopilot.Server) map[raft.ServerID]*autopilot.ServerStats {
	return d.server.statsFetcher.Fetch(ctx, servers)
}

func (d *AutopilotDelegate) NotifyState(state *autopilot.State) {
	// emit metrics if we are the leader regarding overall healthiness and the failure tolerance
	if d.server.raft.State() == raft.Leader {
		metrics.SetGauge([]string{"autopilot", "failure_tolerance"}, float32(state.FailureTolerance))
		if state.Healthy {
			metrics.SetGauge([]string{"autopilot", "healthy"}, 1)
		} else {
			metrics.SetGauge([]string{"autopilot", "healthy"}, 0)
		}
	}
}

func (d *AutopilotDelegate) RemoveFailedServer(srv *autopilot.Server) error {
	if err := d.server.serfLAN.RemoveFailedNode(srv.Name); err != nil {
		return fmt.Errorf("failed to remove server from the LAN serf instance: %w", err)
	}

	// the WAN serf instance has node names suffixed with .<datacenter> so when removing
	// from there we need to ensure that we recreate the proper node name.
	if err := d.server.serfWAN.RemoveFailedNode(srv.Name + "." + d.server.config.Datacenter); err != nil {
		return fmt.Errorf("failed to remove server from the WAN serf instance: %w", err)
	}

	return d.enterpriseRemoveFailedServer(srv)
}

func (s *Server) autopilotServers() map[raft.ServerID]*autopilot.Server {
	servers := make(map[raft.ServerID]*autopilot.Server)
	for _, member := range s.serfLAN.Members() {
		srv, err := s.autopilotServer(member)
		if err != nil {
			s.logger.Warn("Error parsing server info", "name", member.Name, "error", err)
			continue
		} else if srv == nil {
			// this member was a client
			continue
		}

		servers[srv.ID] = srv
	}

	return servers
}

func (s *Server) autopilotServer(m serf.Member) (*autopilot.Server, error) {
	ok, srv := metadata.IsConsulServer(m)
	if !ok {
		return nil, nil
	}

	return s.autopilotServerFromMetadata(srv)
}

func (s *Server) autopilotServerFromMetadata(srv *metadata.Server) (*autopilot.Server, error) {
	server := &autopilot.Server{
		Name:        srv.ShortName,
		ID:          raft.ServerID(srv.ID),
		Address:     raft.ServerAddress(srv.Addr.String()),
		Version:     srv.Build,
		RaftVersion: srv.RaftVersion,
		Ext: &AutopilotServerExt{
			ReadReplica: srv.NonVoter,
		},
	}

	switch srv.Status {
	case serf.StatusLeft:
		server.NodeStatus = autopilot.NodeLeft
	case serf.StatusAlive, serf.StatusLeaving:
		// we want to treat leaving as alive to prevent autopilot from
		// prematurely removing the node.
		server.NodeStatus = autopilot.NodeAlive
	case serf.StatusFailed:
		server.NodeStatus = autopilot.NodeFailed
	default:
		server.NodeStatus = autopilot.NodeUnknown
	}

	// populate the node meta if there is any. When a node first joins or if
	// there are ACL issues then this could be empty if the server has not
	// yet been able to register itself in the catalog
	_, node, err := s.fsm.State().GetNodeID(types.NodeID(srv.ID))
	if err != nil {
		return nil, fmt.Errorf("error retrieving node from state store: %w", err)
	}

	if node != nil {
		server.Meta = node.Meta
	}

	return server, nil
}
