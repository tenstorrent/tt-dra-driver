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

package fabricmanager

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// recvTimeout bounds how long a test waits for a snapshot that should
// already be on its way. It only has to be longer than the in-memory
// round-trip; a regression that stalls the watch fails the test instead of
// hanging the suite.
const recvTimeout = 10 * time.Second

// watchAttempt scripts what the fake agent does the nth time a client opens
// a watch, which is how the reconnect paths get exercised.
type watchAttempt struct {
	// err fails the stream immediately with this error.
	err error
	// msgs are sent in order before hold/EOF is applied.
	msgs []*agentpb.WatchTopologyResponse
	// hold keeps the stream open (and silent) after msgs until the client
	// goes away, which is what a healthy idle watch looks like.
	hold bool
}

// fakeAgent serves WatchTopology from a script. The last entry repeats once
// the script runs out, so a trailing {hold: true} means "stay connected and
// quiet from now on".
type fakeAgent struct {
	agentpb.UnimplementedAgentServiceServer

	script []watchAttempt

	mu       sync.Mutex
	attempts int
}

func (f *fakeAgent) WatchTopology(_ *agentpb.WatchTopologyRequest, stream grpc.ServerStreamingServer[agentpb.WatchTopologyResponse]) error {
	f.mu.Lock()
	n := f.attempts
	f.attempts++
	f.mu.Unlock()

	attempt := f.script[min(n, len(f.script)-1)]

	if attempt.err != nil {
		return attempt.err
	}
	for _, msg := range attempt.msgs {
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	if attempt.hold {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	return nil
}

func (f *fakeAgent) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// startFakeAgent runs agent on an in-memory listener and returns a client
// wired to it. Both are torn down when the test ends.
func startFakeAgent(t *testing.T, agent *fakeAgent, opts ...ClientOption) *AgentClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(server, agent)
	go func() {
		_ = server.Serve(lis)
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatalf("build client for fake agent: %v", err)
	}

	client := NewAgentClient(conn, opts...)
	t.Cleanup(func() {
		_ = client.Close()
		server.Stop()
		_ = lis.Close()
	})
	return client
}

// fastReconnect keeps the reconnect paths quick enough to test.
func fastReconnect() ClientOption {
	return WithReconnectBackoff(time.Millisecond, 2*time.Millisecond)
}

func okMsg(uniqueID, version uint64) *agentpb.WatchTopologyResponse {
	return &agentpb.WatchTopologyResponse{
		Status:  agentpb.GetTopologyStatus_TOPOLOGY_OK,
		Version: version,
		PhysicalTopology: &topologypb.HostPhysicalTopology{
			Asics: []*topologypb.AsicInfo{{UniqueId: uniqueID, IsMmioCapable: true}},
		},
	}
}

func notDiscoveredMsg(version uint64) *agentpb.WatchTopologyResponse {
	return &agentpb.WatchTopologyResponse{
		Status:  agentpb.GetTopologyStatus_TOPOLOGY_NOT_DISCOVERED,
		Version: version,
	}
}

func discoveryErrorMsg(reason string, version uint64) *agentpb.WatchTopologyResponse {
	return &agentpb.WatchTopologyResponse{
		Status:  agentpb.GetTopologyStatus_TOPOLOGY_OK,
		Version: version,
		PhysicalTopology: &topologypb.HostPhysicalTopology{
			DiscoveryError: reason,
		},
	}
}

// recvSnapshot returns the next snapshot, failing the test if the watch
// closes or goes quiet instead.
func recvSnapshot(t *testing.T, watch TopologyWatch) Snapshot {
	t.Helper()
	select {
	case snapshot, ok := <-watch.Updates():
		if !ok {
			t.Fatalf("watch closed while a snapshot was expected: %v", watch.Err())
		}
		return snapshot
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for a snapshot")
		return Snapshot{}
	}
}

// awaitClosed drains the watch and returns the error it stopped with.
func awaitClosed(t *testing.T, watch TopologyWatch) error {
	t.Helper()
	for {
		select {
		case _, ok := <-watch.Updates():
			if !ok {
				return watch.Err()
			}
		case <-time.After(recvTimeout):
			t.Fatal("timed out waiting for the watch to stop")
			return nil
		}
	}
}

// TestWatchTopologyDeliversInitialAndChangedSnapshots covers the happy path:
// the snapshot the agent sends on connect, followed by one per change.
func TestWatchTopologyDeliversInitialAndChangedSnapshots(t *testing.T) {
	client := startFakeAgent(t, &fakeAgent{script: []watchAttempt{{
		msgs: []*agentpb.WatchTopologyResponse{okMsg(1, 7), okMsg(2, 8)},
		hold: true,
	}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := client.WatchTopology(ctx)

	first := recvSnapshot(t, watch)
	if !first.Usable() {
		t.Fatalf("first snapshot is not usable: %+v", first)
	}
	if got := first.Topology.GetAsics()[0].GetUniqueId(); got != 1 {
		t.Errorf("first snapshot has ASIC %d, want 1", got)
	}
	if first.Version != 7 {
		t.Errorf("first snapshot has version %d, want 7", first.Version)
	}

	second := recvSnapshot(t, watch)
	if got := second.Topology.GetAsics()[0].GetUniqueId(); got != 2 {
		t.Errorf("second snapshot has ASIC %d, want 2", got)
	}
}

// TestWatchTopologyNotUsableSnapshots checks that states carrying no
// topology are still delivered — the consumer needs to know the agent is
// alive — but are marked so it does not mistake them for an empty host.
func TestWatchTopologyNotUsableSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name               string
		msg                *agentpb.WatchTopologyResponse
		wantDiscoveryError string
	}{
		{
			name: "discovery not finished",
			msg:  notDiscoveredMsg(1),
		},
		{
			name:               "discovery failed",
			msg:                discoveryErrorMsg("sysfs fallback validation failed", 2),
			wantDiscoveryError: "sysfs fallback validation failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := startFakeAgent(t, &fakeAgent{script: []watchAttempt{{
				msgs: []*agentpb.WatchTopologyResponse{tc.msg},
				hold: true,
			}}})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			snapshot := recvSnapshot(t, client.WatchTopology(ctx))
			if snapshot.Usable() {
				t.Errorf("snapshot %+v reports itself usable", snapshot)
			}
			if got := snapshot.DiscoveryError(); got != tc.wantDiscoveryError {
				t.Errorf("DiscoveryError is %q, want %q", got, tc.wantDiscoveryError)
			}
		})
	}
}

// TestWatchTopologyReconnects covers the failures that a watch must ride out
// on its own rather than surfacing to the driver.
func TestWatchTopologyReconnects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first watchAttempt
		opts  []ClientOption
	}{
		{
			name:  "agent is down",
			first: watchAttempt{err: status.Error(codes.Unavailable, "agent restarting")},
		},
		{
			name:  "agent is out of watch slots",
			first: watchAttempt{err: status.Error(codes.ResourceExhausted, "too many watches")},
		},
		{
			name:  "agent ends the stream cleanly",
			first: watchAttempt{},
		},
		{
			// The wedged-agent case: the stream is accepted but the promised
			// initial snapshot never arrives.
			name:  "agent accepts the watch and goes quiet",
			first: watchAttempt{hold: true},
			opts:  []ClientOption{WithFirstSnapshotTimeout(100 * time.Millisecond)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{script: []watchAttempt{
				tc.first,
				{msgs: []*agentpb.WatchTopologyResponse{okMsg(42, 1)}, hold: true},
			}}
			client := startFakeAgent(t, agent, append([]ClientOption{fastReconnect()}, tc.opts...)...)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			watch := client.WatchTopology(ctx)

			snapshot := recvSnapshot(t, watch)
			if got := snapshot.Topology.GetAsics()[0].GetUniqueId(); got != 42 {
				t.Errorf("snapshot has ASIC %d, want 42 from the second attempt", got)
			}
			// Receiving the second attempt's snapshot is itself the proof
			// that the watch reconnected instead of giving up; Err is only
			// readable once the watch has stopped.
			if attempts := agent.attemptCount(); attempts < 2 {
				t.Errorf("agent saw %d watch attempts, want at least 2 (a reconnect)", attempts)
			}
		})
	}
}

// TestWatchTopologyUnsupportedAgent covers an agent too old to serve the
// streaming API: reconnecting cannot fix it, so the watch has to stop and
// say why.
func TestWatchTopologyUnsupportedAgent(t *testing.T) {
	agent := &fakeAgent{script: []watchAttempt{{
		err: status.Error(codes.Unimplemented, "unknown method WatchTopology"),
	}}}
	client := startFakeAgent(t, agent, fastReconnect())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := awaitClosed(t, client.WatchTopology(ctx))
	if !errors.Is(err, ErrWatchUnsupported) {
		t.Errorf("watch stopped with %v, want ErrWatchUnsupported", err)
	}
	if attempts := agent.attemptCount(); attempts != 1 {
		t.Errorf("agent saw %d watch attempts, want exactly 1: a permanent failure must not be retried", attempts)
	}
}

// TestWatchTopologyPermanentError checks that an error retrying cannot fix
// stops the watch instead of spinning forever.
func TestWatchTopologyPermanentError(t *testing.T) {
	client := startFakeAgent(t, &fakeAgent{script: []watchAttempt{{
		err: status.Error(codes.Internal, "boom"),
	}}}, fastReconnect())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := awaitClosed(t, client.WatchTopology(ctx))
	if err == nil {
		t.Fatal("watch stopped without an error, want the agent's failure")
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("watch stopped with code %v, want Internal to stay inspectable", code)
	}
}

// TestWatchTopologyStopsOnContextCancel checks the shutdown path: cancelling
// the context is a clean stop, not a failure.
func TestWatchTopologyStopsOnContextCancel(t *testing.T) {
	client := startFakeAgent(t, &fakeAgent{script: []watchAttempt{{
		msgs: []*agentpb.WatchTopologyResponse{okMsg(1, 1)},
		hold: true,
	}}})

	ctx, cancel := context.WithCancel(context.Background())
	watch := client.WatchTopology(ctx)
	recvSnapshot(t, watch)
	cancel()

	if err := awaitClosed(t, watch); err != nil {
		t.Errorf("watch stopped with %v, want nil after its context was cancelled", err)
	}
}

// TestWatchTopologyUnreachableAgent documents that a lazily-connected client
// keeps retrying an agent that is not listening yet, rather than failing.
func TestWatchTopologyUnreachableAgent(t *testing.T) {
	client, err := Dial("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// No snapshot can arrive, and the watch must keep trying until its
	// context expires instead of reporting a permanent failure.
	if err := awaitClosed(t, client.WatchTopology(ctx)); err != nil {
		t.Errorf("watch stopped with %v, want nil: an unreachable agent is a transient condition", err)
	}
}

func TestReconnectBackoff(t *testing.T) {
	b := &reconnectBackoff{initial: time.Second, max: 4 * time.Second}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for i, expected := range want {
		if got := b.next(); got != expected {
			t.Errorf("delay %d is %v, want %v", i, got, expected)
		}
	}

	b.reset()
	if got := b.next(); got != time.Second {
		t.Errorf("delay after reset is %v, want the initial %v", got, time.Second)
	}
}
