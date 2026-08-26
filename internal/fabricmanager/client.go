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
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	agentpb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// DefaultFirstSnapshotTimeout bounds how long a newly opened watch waits for
// the initial snapshot that the agent promises to write immediately. Because
// grpc.NewClient connects lazily, an agent that is down or wedged is only
// detected on the call itself; without this bound a wedged agent that
// accepts the stream and then goes quiet would stall the watch forever. It
// deliberately does not apply to later messages: a healthy watch is silent
// for as long as the topology is stable.
const DefaultFirstSnapshotTimeout = 10 * time.Second

// Reconnect pacing for a watch that keeps dropping. The watch is per-node
// and there is exactly one client per agent, so there is no herd to spread
// out and no jitter is applied.
const (
	DefaultReconnectInitialDelay = 1 * time.Second
	DefaultReconnectMaxDelay     = 30 * time.Second
)

// ErrAgentUnavailable wraps failures that mean "the agent is not answering
// right now": the connection could not be established (the agent is down,
// restarting or not yet scheduled), it broke mid-watch, or the agent
// accepted a watch and never sent the initial snapshot. It is a transient
// condition — a watch reconnects on its own — and is exposed so that
// callers can tell it apart from a permanent failure. The underlying error
// is wrapped alongside it and stays inspectable with errors.As /
// status.FromError.
var ErrAgentUnavailable = errors.New("fabric manager agent: unavailable")

// ErrWatchUnsupported reports that the agent does not serve WatchTopology.
// This means the agent predates the streaming API and must be upgraded; no
// amount of retrying will help, so a watch fails with this rather than
// reconnecting forever.
var ErrWatchUnsupported = errors.New("fabric manager agent: WatchTopology is not implemented; upgrade the fabric manager agent")

// errNoFirstSnapshot is the cancellation cause used when the initial
// snapshot does not arrive in time. It never escapes this package
// unwrapped; callers see ErrAgentUnavailable.
var errNoFirstSnapshot = errors.New("agent accepted the topology watch but did not send the initial snapshot")

// defaultKeepalive keeps a long-lived watch honest. Without it a silently
// dead connection — agent OOM-killed, node network blip, a middlebox
// dropping an idle flow — is indistinguishable from a watch with nothing to
// report, and the driver would keep serving a topology that stopped being
// updated. The ping interval is deliberately conservative: gRPC servers
// reject pings more frequent than their keepalive EnforcementPolicy.MinTime
// (5 minutes by default) with ENHANCE_YOUR_CALM, which would break the very
// connection this is meant to protect. Lower it only in step with the
// agent's own enforcement policy.
var defaultKeepalive = keepalive.ClientParameters{
	Time:    5 * time.Minute,
	Timeout: 20 * time.Second,
}

// Snapshot is one point-in-time view of the agent's topology state. Every
// snapshot is complete rather than a delta, so a consumer only ever needs
// to keep the newest one it received.
type Snapshot struct {
	// Status is the agent's topology status at the instant of the snapshot.
	Status agentpb.GetTopologyStatus

	// Topology is the discovered host topology. It is nil unless Status is
	// TOPOLOGY_OK, and carries an empty ASIC list when discovery failed (see
	// DiscoveryError).
	Topology *topologypb.HostPhysicalTopology

	// Version is the agent's generation counter for this state. It is
	// strictly increasing within one watch but is not contiguous, and it
	// resets when the agent restarts, so it is only meaningful for ordering
	// and logging within a single connection.
	Version uint64
}

// DiscoveryError returns the reason the agent could not build a usable
// topology, or "" when discovery succeeded.
func (s Snapshot) DiscoveryError() string {
	return s.Topology.GetDiscoveryError()
}

// Usable reports whether the snapshot carries a topology the driver can act
// on, i.e. discovery has completed and did not fail. A snapshot that is not
// usable carries no information about the devices on the host, so consumers
// should keep whatever they last acted on rather than treating it as an
// empty host.
func (s Snapshot) Usable() bool {
	return s.Topology != nil && s.Topology.GetDiscoveryError() == ""
}

// TopologyClient is the subset of the fabric manager agent's API surface
// that the DRA kubelet plugin depends on. Splitting out a small interface
// keeps the profile code testable without spinning up a real gRPC server.
type TopologyClient interface {
	// WatchTopology starts watching the host's physical topology. It returns
	// immediately; snapshots arrive on the watch's channel.
	WatchTopology(ctx context.Context) TopologyWatch
}

// TopologyWatch is a live view of one agent's topology state.
type TopologyWatch interface {
	// Updates delivers the state as of the moment the watch connected and
	// then one snapshot per change, for as long as the watch runs. Dropped
	// connections are re-established transparently, and because the agent
	// re-sends a full snapshot on every new stream, a reconnect
	// re-synchronises the consumer instead of leaving it stale.
	//
	// The channel is closed when the watch stops: either the context passed
	// to WatchTopology was cancelled, or the watch failed in a way
	// reconnecting cannot fix. Err says which.
	Updates() <-chan Snapshot

	// Err returns the error that stopped the watch, or nil if it stopped
	// because its context was cancelled. It must only be read after Updates
	// has been observed closed.
	Err() error
}

// AgentClient is a TopologyClient backed by a gRPC connection to a
// fabric-manager agent. Use Dial to construct one; use Close to release the
// underlying connection.
type AgentClient struct {
	conn   *grpc.ClientConn
	client agentpb.AgentServiceClient

	firstSnapshotTimeout time.Duration
	reconnectInitial     time.Duration
	reconnectMax         time.Duration
}

// ClientOption customizes an AgentClient built by NewAgentClient.
type ClientOption func(*AgentClient)

// WithFirstSnapshotTimeout overrides DefaultFirstSnapshotTimeout. A
// non-positive duration disables the bound, leaving the caller's context as
// the only limit on how long a new watch waits for its first snapshot.
func WithFirstSnapshotTimeout(d time.Duration) ClientOption {
	return func(c *AgentClient) {
		c.firstSnapshotTimeout = d
	}
}

// WithReconnectBackoff overrides the delays used between watch reconnect
// attempts.
func WithReconnectBackoff(initial, max time.Duration) ClientOption {
	return func(c *AgentClient) {
		c.reconnectInitial = initial
		c.reconnectMax = max
	}
}

// Dial establishes a connection to the fabric manager agent at address
// ("host:port"). Additional gRPC dial options may be passed; when none are
// supplied an insecure transport plus defaultKeepalive is used, matching the
// deployment model where the agent and the kubelet plugin run on the same
// node and share a trust domain. A caller that supplies its own options
// takes over responsibility for both.
//
// grpc.NewClient does not block on connectivity, so a successful Dial says
// nothing about the agent being reachable; that only shows up once a watch
// tries to talk to it.
func Dial(address string, opts ...grpc.DialOption) (*AgentClient, error) {
	if len(opts) == 0 {
		opts = []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithKeepaliveParams(defaultKeepalive),
		}
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
		conn:                 conn,
		client:               agentpb.NewAgentServiceClient(conn),
		firstSnapshotTimeout: DefaultFirstSnapshotTimeout,
		reconnectInitial:     DefaultReconnectInitialDelay,
		reconnectMax:         DefaultReconnectMaxDelay,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WatchTopology implements TopologyClient.
func (c *AgentClient) WatchTopology(ctx context.Context) TopologyWatch {
	w := &agentWatch{updates: make(chan Snapshot)}
	go w.run(ctx, c)
	return w
}

// Close releases the underlying gRPC connection. Watches started from this
// client stop when the context they were given is cancelled, not when the
// connection is closed, so cancel them first.
func (c *AgentClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// agentWatch implements TopologyWatch on top of the agent's WatchTopology
// server stream, re-establishing the stream for as long as the failures are
// transient.
type agentWatch struct {
	updates chan Snapshot

	// err is written by run before it closes updates, and read by Err only
	// after the close has been observed. The channel close is what
	// synchronises the two.
	err error
}

// Updates implements TopologyWatch.
func (w *agentWatch) Updates() <-chan Snapshot { return w.updates }

// Err implements TopologyWatch.
func (w *agentWatch) Err() error { return w.err }

// run keeps a stream open for as long as the context lives, reconnecting
// with backoff after transient failures and stopping on anything else.
func (w *agentWatch) run(ctx context.Context, c *AgentClient) {
	defer close(w.updates)

	logger := klog.FromContext(ctx)
	backoff := &reconnectBackoff{initial: c.reconnectInitial, max: c.reconnectMax}

	for {
		err := w.stream(ctx, c, backoff)
		switch {
		case ctx.Err() != nil:
			// The consumer is shutting the watch down. Not a failure, so Err
			// stays nil.
			return
		case transient(ctx, err):
			delay := backoff.next()
			logger.Info("Fabric manager topology watch dropped; reconnecting", "err", err, "delay", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
		default:
			w.err = fmt.Errorf("watch fabric manager topology: %w", err)
			return
		}
	}
}

// stream opens one WatchTopology stream and forwards its snapshots until it
// breaks, returning the error that ended it.
func (w *agentWatch) stream(ctx context.Context, c *AgentClient, backoff *reconnectBackoff) error {
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// The protocol promises an immediate first snapshot, so that is the one
	// message worth putting a deadline on. Later silence is normal — it
	// means the topology has not changed — and is covered by transport
	// keepalives instead.
	var firstSnapshot *time.Timer
	if c.firstSnapshotTimeout > 0 {
		firstSnapshot = time.AfterFunc(c.firstSnapshotTimeout, func() {
			cancel(errNoFirstSnapshot)
		})
		defer firstSnapshot.Stop()
	}

	stream, err := c.client.WatchTopology(streamCtx, &agentpb.WatchTopologyRequest{})
	if err != nil {
		return watchError(streamCtx, err, false)
	}

	received := false
	for {
		resp, err := stream.Recv()
		if err != nil {
			return watchError(streamCtx, err, received)
		}
		// The agent is talking to us, so neither the first-snapshot deadline
		// nor the accumulated reconnect delay applies any more.
		if firstSnapshot != nil {
			firstSnapshot.Stop()
		}
		received = true
		backoff.reset()

		select {
		case w.updates <- snapshotOf(resp):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// watchError translates a stream failure into the error the reconnect loop
// classifies on. received guards against mistaking a late-firing
// first-snapshot timer for a stream that never produced anything: once a
// snapshot has arrived, the cancellation cause is no longer the explanation
// for the failure.
func watchError(streamCtx context.Context, err error, received bool) error {
	if !received && errors.Is(context.Cause(streamCtx), errNoFirstSnapshot) {
		return fmt.Errorf("%w: %w", ErrAgentUnavailable, errNoFirstSnapshot)
	}
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("%w: %w", ErrWatchUnsupported, err)
	}
	return err
}

// snapshotOf converts one stream message into a Snapshot. Statuses this
// build does not know about land as "not usable", which keeps a newer agent
// from breaking an older driver: the driver keeps serving the last topology
// it understood.
func snapshotOf(resp *agentpb.WatchTopologyResponse) Snapshot {
	s := Snapshot{
		Status:  resp.GetStatus(),
		Version: resp.GetVersion(),
	}
	if resp.GetStatus() == agentpb.GetTopologyStatus_TOPOLOGY_OK {
		s.Topology = resp.GetPhysicalTopology()
	}
	return s
}

// transient reports whether a watch failure is worth reconnecting after.
// parent is the context the watch was started with: when it is already done
// the failure belongs to the consumer (shutdown, or a deadline it chose),
// not to the agent, and retrying would only stall the shutdown path.
func transient(parent context.Context, err error) bool {
	if parent.Err() != nil {
		return false
	}
	// ErrAgentUnavailable covers the first-snapshot timeout, which surfaces
	// as a context cancellation rather than a status code. io.EOF means the
	// agent ended the stream cleanly, which it only does on shutdown.
	if errors.Is(err, ErrAgentUnavailable) || errors.Is(err, io.EOF) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable:
		// No connection could be established, or an established one broke:
		// the agent is down, restarting or not yet scheduled.
		return true
	case codes.ResourceExhausted:
		// The agent caps how many watches it serves at once, since each one
		// occupies a thread on its synchronous gRPC server. Another client
		// releasing its watch frees a slot, so this is worth waiting out.
		return true
	case codes.DeadlineExceeded:
		// A deadline on the stream expired. The client sets none itself, so
		// this is either the caller's or an intermediary's.
		return true
	default:
		return false
	}
}

// reconnectBackoff is a doubling delay, capped, reset whenever the watch
// makes progress.
type reconnectBackoff struct {
	initial time.Duration
	max     time.Duration
	current time.Duration
}

// next returns the delay to wait before the next reconnect attempt.
func (b *reconnectBackoff) next() time.Duration {
	switch {
	case b.current == 0:
		b.current = b.initial
	case b.current < b.max:
		b.current *= 2
	}
	if b.max > 0 && b.current > b.max {
		b.current = b.max
	}
	return b.current
}

// reset forgets the accumulated delay, so that a watch which stayed up for a
// while starts over from the shortest delay when it eventually drops.
func (b *reconnectBackoff) reset() {
	b.current = 0
}
