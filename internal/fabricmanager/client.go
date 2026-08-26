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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	agentpb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// DefaultRPCTimeout bounds a single RPC to the agent. Because grpc.NewClient
// connects lazily, an agent that is down or wedged is only detected on the
// call itself; without a deadline a wedged agent would block the caller
// forever. Ten seconds is far longer than a healthy GetTopology takes while
// still failing fast enough for the retry loop above it to make progress.
const DefaultRPCTimeout = 10 * time.Second

// ErrTopologyNotReady is returned by GetTopology when the agent reports
// TOPOLOGY_NOT_DISCOVERED. It signals that the agent process is up and
// reachable but has not finished its initial topology discovery yet, so the
// caller should retry rather than treat the response as a final answer.
var ErrTopologyNotReady = errors.New("fabric manager agent: topology not yet discovered")

// ErrAgentUnavailable wraps RPC failures that mean "the agent is not
// answering right now": the connection could not be established (the agent
// is down, restarting or not yet scheduled) or the call exceeded its
// deadline (the agent is wedged). Like ErrTopologyNotReady it is a transient
// condition, so callers should retry instead of failing outright. The
// underlying gRPC error is wrapped alongside it and remains inspectable with
// errors.As / status.FromError.
var ErrAgentUnavailable = errors.New("fabric manager agent: unavailable")

// TopologyClient is the read-only subset of the fabric manager agent's API
// surface that the DRA kubelet plugin depends on. Splitting out a small
// interface keeps the profile code testable without spinning up a real gRPC
// server.
type TopologyClient interface {
	// GetTopology fetches the physical topology of the host the agent runs
	// on. The call returns ErrTopologyNotReady when the agent is up but has
	// not yet completed initial topology discovery, ErrAgentUnavailable when
	// the agent is unreachable or does not answer within the per-call
	// deadline, and a wrapped error for any other RPC or unexpected-status
	// failure. The first two are transient and worth retrying.
	GetTopology(ctx context.Context) (*topologypb.HostPhysicalTopology, error)
}

// AgentClient is a TopologyClient backed by a gRPC connection to a
// fabric-manager agent. Use Dial to construct one; use Close to release the
// underlying connection.
type AgentClient struct {
	conn       *grpc.ClientConn
	client     agentpb.AgentServiceClient
	rpcTimeout time.Duration
}

// ClientOption customizes an AgentClient built by NewAgentClient.
type ClientOption func(*AgentClient)

// WithRPCTimeout overrides DefaultRPCTimeout as the per-call deadline. A
// non-positive duration disables the client-side deadline, leaving the
// caller's context as the only bound.
func WithRPCTimeout(d time.Duration) ClientOption {
	return func(c *AgentClient) {
		c.rpcTimeout = d
	}
}

// Dial establishes a connection to the fabric manager agent at address
// ("host:port"). Additional gRPC dial options may be passed; when none are
// supplied an insecure transport is used, matching the deployment model
// where the agent and the kubelet plugin run on the same node and share a
// trust domain.
//
// grpc.NewClient does not block on connectivity, so a successful Dial says
// nothing about the agent being reachable; the first RPC reports that as
// ErrAgentUnavailable.
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
func NewAgentClient(conn *grpc.ClientConn, opts ...ClientOption) *AgentClient {
	c := &AgentClient{
		conn:       conn,
		client:     agentpb.NewAgentServiceClient(conn),
		rpcTimeout: DefaultRPCTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// GetTopology implements TopologyClient.
func (c *AgentClient) GetTopology(ctx context.Context) (*topologypb.HostPhysicalTopology, error) {
	parent := ctx
	if c.rpcTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.rpcTimeout)
		defer cancel()
	}

	resp, err := c.client.GetTopology(ctx, &agentpb.GetTopologyRequest{})
	if err != nil {
		if transient(parent, err) {
			return nil, fmt.Errorf("call GetTopology: %w: %w", ErrAgentUnavailable, err)
		}
		return nil, fmt.Errorf("call GetTopology: %w", err)
	}
	switch resp.GetStatus() {
	case agentpb.GetTopologyStatus_TOPOLOGY_OK:
		return resp.GetPhysicalTopology(), nil
	case agentpb.GetTopologyStatus_TOPOLOGY_NOT_DISCOVERED:
		return nil, ErrTopologyNotReady
	default:
		return nil, fmt.Errorf("fabric manager agent reported topology status %s", resp.GetStatus())
	}
}

// transient reports whether an RPC error means the agent is temporarily not
// answering and the call is worth retrying. parent is the caller's context,
// before any per-call deadline was applied: when it is already done the
// failure belongs to the caller (shutdown, or a deadline the caller chose),
// not to the agent, and retrying would only stall the shutdown path.
func transient(parent context.Context, err error) bool {
	if parent.Err() != nil {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable:
		// No connection could be established, or an established one broke:
		// the agent is down, restarting or not yet scheduled.
		return true
	case codes.DeadlineExceeded:
		// The per-call deadline fired: the agent accepted the call but never
		// answered.
		return true
	default:
		return false
	}
}

// Close releases the underlying gRPC connection.
func (c *AgentClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
