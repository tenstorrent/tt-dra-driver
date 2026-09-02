/*
 * Copyright 2026 Tenstorrent USA, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package fabricmanager provides a thin client for the Tenstorrent
// Fabric Manager (TTFM) agent gRPC API. The DRA kubelet plugin consumes
// this client to discover the ASICs available on the host it runs on.
package fabricmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"

	agentpb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// ErrTopologyNotReady is returned by GetTopology when the agent reports
// TOPOLOGY_NOT_DISCOVERED. It signals that the agent process is up and
// reachable but has not finished its initial topology discovery yet, so the
// caller should retry rather than treat the response as a final answer.
var ErrTopologyNotReady = errors.New("fabric manager agent: topology not yet discovered")

// TopologyClient is the read-only subset of the fabric manager agent's API
// surface that the DRA kubelet plugin depends on. Splitting out a small
// interface keeps the profile code testable without spinning up a real gRPC
// server.
type TopologyClient interface {
	// GetTopology fetches the physical topology of the host the agent runs
	// on. The call returns ErrTopologyNotReady when the agent is up but has
	// not yet completed initial topology discovery, and a wrapped error for
	// any other RPC or unexpected-status failure.
	GetTopology(ctx context.Context) (*topologypb.HostPhysicalTopology, error)
}

// AgentClient is a TopologyClient backed by a gRPC connection to a
// fabric-manager agent. Use Dial to construct one; use Close to release the
// underlying connection.
type AgentClient struct {
	conn   *grpc.ClientConn
	client agentpb.AgentServiceClient
}

// Dial establishes a connection to the fabric manager agent at address
// ("host:port"). Additional gRPC dial options may be passed; when none are
// supplied an insecure transport is used, matching the deployment model
// where the agent and the kubelet plugin run on the same node and share a
// trust domain.
func Dial(address string, opts ...grpc.DialOption) (*AgentClient, error) {
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial fabric manager agent at %q: %w", address, err)
	}
	return NewAgentClient(conn), nil
}

// NewAgentClient builds an AgentClient on top of an existing gRPC client
// connection. Useful in tests where the connection is wired up to an
// in-memory gRPC server.
func NewAgentClient(conn *grpc.ClientConn) *AgentClient {
	return &AgentClient{
		conn:   conn,
		client: agentpb.NewAgentServiceClient(conn),
	}
}

// GetTopology implements TopologyClient.
//
// The call carries no deadline of its own, so how long it takes is bounded
// only by the caller's context. Because grpc.NewClient connects lazily, this
// is also where an agent that is down, unreachable or wedged first shows up,
// which is why the elapsed time is logged for every outcome and repeated in
// the returned error.
func (c *AgentClient) GetTopology(ctx context.Context) (*topologypb.HostPhysicalTopology, error) {
	logger := klog.FromContext(ctx).WithValues("agentAddress", c.target())
	logger.V(4).Info("Calling GetTopology on the fabric manager agent")

	start := time.Now()
	resp, err := c.client.GetTopology(ctx, &agentpb.GetTopologyRequest{})
	duration := time.Since(start)
	if err != nil {
		logger.V(2).Info("GetTopology call to the fabric manager agent failed", "duration", duration, "err", err)
		return nil, fmt.Errorf("call GetTopology (after %s): %w", duration.Round(time.Millisecond), err)
	}

	logger.V(2).Info("GetTopology call to the fabric manager agent returned",
		"duration", duration,
		"status", resp.GetStatus(),
		"numASICs", len(resp.GetPhysicalTopology().GetAsics()),
	)

	switch resp.GetStatus() {
	case agentpb.GetTopologyStatus_TOPOLOGY_OK:
		return resp.GetPhysicalTopology(), nil
	case agentpb.GetTopologyStatus_TOPOLOGY_NOT_DISCOVERED:
		return nil, ErrTopologyNotReady
	default:
		return nil, fmt.Errorf("fabric manager agent reported topology status %s", resp.GetStatus())
	}
}

// target reports the address the client was built against, for log context.
func (c *AgentClient) target() string {
	if c == nil || c.conn == nil {
		return ""
	}
	return c.conn.Target()
}

// Close releases the underlying gRPC connection.
func (c *AgentClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
