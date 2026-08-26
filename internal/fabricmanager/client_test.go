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

// fakeAgent serves GetTopology from a canned response, error or handler.
type fakeAgent struct {
	agentpb.UnimplementedAgentServiceServer

	resp   *agentpb.GetTopologyResponse
	err    error
	block  bool          // never answer, until the call's context is done
	served chan struct{} // closed on first GetTopology, if non-nil
}

func (f *fakeAgent) GetTopology(ctx context.Context, _ *agentpb.GetTopologyRequest) (*agentpb.GetTopologyResponse, error) {
	if f.served != nil {
		close(f.served)
		f.served = nil
	}
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.resp, f.err
}

// startFakeAgent runs agent on an in-memory listener and returns a client
// wired to it. Both are torn down when the test ends.
func startFakeAgent(t *testing.T, agent *fakeAgent, opts ...ClientOption) *AgentClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(server, agent)
	go func() {
		// Serve returns ErrServerStopped-free on GracefulStop; anything else
		// surfaces as a failing RPC in the test body.
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

func TestGetTopologyOK(t *testing.T) {
	want := &topologypb.HostPhysicalTopology{
		Asics: []*topologypb.AsicInfo{{UniqueId: 42, IsMmioCapable: true}},
	}
	client := startFakeAgent(t, &fakeAgent{
		resp: &agentpb.GetTopologyResponse{
			Status:           agentpb.GetTopologyStatus_TOPOLOGY_OK,
			PhysicalTopology: want,
		},
	})

	got, err := client.GetTopology(context.Background())
	if err != nil {
		t.Fatalf("GetTopology: unexpected error: %v", err)
	}
	if len(got.GetAsics()) != 1 || got.GetAsics()[0].GetUniqueId() != 42 {
		t.Errorf("GetTopology returned %v, want the single ASIC with unique id 42", got.GetAsics())
	}
}

func TestGetTopologyNotDiscovered(t *testing.T) {
	client := startFakeAgent(t, &fakeAgent{
		resp: &agentpb.GetTopologyResponse{
			Status: agentpb.GetTopologyStatus_TOPOLOGY_NOT_DISCOVERED,
		},
	})

	_, err := client.GetTopology(context.Background())
	if !errors.Is(err, ErrTopologyNotReady) {
		t.Errorf("GetTopology returned %v, want ErrTopologyNotReady", err)
	}
	if errors.Is(err, ErrAgentUnavailable) {
		t.Errorf("GetTopology returned %v, which must not be ErrAgentUnavailable: the agent did answer", err)
	}
}

// TestGetTopologyUnavailable covers the agent being down or restarting: the
// RPC fails with codes.Unavailable and must be reported as transient so the
// caller retries instead of exiting.
func TestGetTopologyUnavailable(t *testing.T) {
	client := startFakeAgent(t, &fakeAgent{
		err: status.Error(codes.Unavailable, "agent restarting"),
	})

	_, err := client.GetTopology(context.Background())
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("GetTopology returned %v, want ErrAgentUnavailable", err)
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("wrapped error has code %v, want Unavailable to stay inspectable", code)
	}
}

// TestGetTopologyWedgedAgent is the case the per-call deadline exists for: a
// reachable agent that accepts the call and never answers must not block the
// caller indefinitely.
func TestGetTopologyWedgedAgent(t *testing.T) {
	client := startFakeAgent(t,
		&fakeAgent{block: true},
		WithRPCTimeout(100*time.Millisecond),
	)

	done := make(chan error, 1)
	go func() {
		_, err := client.GetTopology(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrAgentUnavailable) {
			t.Errorf("GetTopology returned %v, want ErrAgentUnavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("GetTopology did not return: the per-call deadline is not being applied")
	}
}

// TestGetTopologyCallerContextDone checks that a failure owned by the caller
// (shutdown, or a caller-chosen deadline) is not reported as a transient
// agent problem, so the retry loop above does not keep sleeping while the
// plugin is trying to exit.
func TestGetTopologyCallerContextDone(t *testing.T) {
	client := startFakeAgent(t, &fakeAgent{
		block:  true,
		served: make(chan struct{}),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetTopology(ctx)
	if err == nil {
		t.Fatal("GetTopology succeeded on a cancelled context, want an error")
	}
	if errors.Is(err, ErrAgentUnavailable) {
		t.Errorf("GetTopology returned %v; a cancelled caller context must not be reported as ErrAgentUnavailable", err)
	}
}

// TestGetTopologyPermanentError checks that errors which retrying cannot fix
// are surfaced immediately rather than wrapped as transient.
func TestGetTopologyPermanentError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent *fakeAgent
	}{
		{
			name:  "rpc error",
			agent: &fakeAgent{err: status.Error(codes.Internal, "boom")},
		},
		{
			// A status this client does not know about, e.g. one added to
			// the agent's proto after this build.
			name: "unexpected status",
			agent: &fakeAgent{resp: &agentpb.GetTopologyResponse{
				Status: agentpb.GetTopologyStatus(99),
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := startFakeAgent(t, tc.agent)

			_, err := client.GetTopology(context.Background())
			if err == nil {
				t.Fatal("GetTopology succeeded, want an error")
			}
			if errors.Is(err, ErrAgentUnavailable) || errors.Is(err, ErrTopologyNotReady) {
				t.Errorf("GetTopology returned %v, want a non-transient error", err)
			}
		})
	}
}

// TestDialDoesNotBlockOnUnreachableAgent documents that Dial succeeds even
// when nothing is listening: unreachability shows up on the first RPC.
func TestDialDoesNotBlockOnUnreachableAgent(t *testing.T) {
	client, err := Dial("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_, err = client.GetTopology(context.Background())
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Errorf("GetTopology against an unreachable agent returned %v, want ErrAgentUnavailable", err)
	}
}
