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
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentpb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// TopologyClient is the read-only subset of the fabric manager agent's API
// surface that the DRA kubelet plugin depends on. Splitting out a small
// interface keeps the profile code testable without spinning up a real gRPC
// server.
type TopologyClient interface {
	// GetTopology fetches the physical topology of the host the agent runs
	// on. The call returns an error when the agent has not yet discovered a
	// topology or when the RPC itself fails.
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
func (c *AgentClient) GetTopology(ctx context.Context) (*topologypb.HostPhysicalTopology, error) {
	resp, err := c.client.GetTopology(ctx, &agentpb.GetTopologyRequest{})
	if err != nil {
		return nil, fmt.Errorf("call GetTopology: %w", err)
	}
	if resp.GetStatus() != agentpb.GetTopologyStatus_TOPOLOGY_OK {
		return nil, fmt.Errorf("fabric manager agent reported topology status %s", resp.GetStatus())
	}
	return resp.GetPhysicalTopology(), nil
}

// Close releases the underlying gRPC connection.
func (c *AgentClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
