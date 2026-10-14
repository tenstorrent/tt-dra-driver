// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

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

package main

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// fakeAgent is an in-process stand-in for the per-node Tenstorrent Fabric
// Manager agent.
//
// It serves the real AgentService over a real gRPC connection on loopback, so
// a test that points the driver at its address exercises the production
// fabricmanager client and the whole topology-to-ResourceSlice conversion
// without a Tenstorrent chip on the host. The agent address is already a
// driver flag, so nothing about this requires a test-only code path in the
// driver.
type fakeAgent struct {
	agentpb.UnimplementedAgentServiceServer

	// addr is the "host:port" the agent listens on, for --fabric-manager-agent-address.
	addr string

	mu       sync.Mutex
	topology *topologypb.HostPhysicalTopology
	notReady bool
	rpcErr   error
	version  uint64
	watches  int

	// subs holds one entry per open WatchTopology stream. Every state change
	// fans a fresh snapshot out to all of them, the way the real agent
	// re-reports to each watcher.
	subs map[*watchStream]struct{}
}

// watchStream is the agent's side of one open WatchTopology stream.
type watchStream struct {
	// queue carries the snapshots the stream has yet to send. It is buffered
	// so that a test changing the topology never blocks on a stream the driver
	// has not drained yet.
	queue chan *agentpb.WatchTopologyResponse

	// fail ends the stream with an error, standing in for the agent dying or
	// refusing to serve a watch it had already accepted.
	fail chan error
}

// startFakeAgent serves the agent API on an unused loopback port for the
// duration of the test.
func startFakeAgent(t *testing.T, topology *topologypb.HostPhysicalTopology) *fakeAgent {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the fake fabric manager agent: %v", err)
	}

	agent := &fakeAgent{
		addr:     listener.Addr().String(),
		topology: topology,
		subs:     make(map[*watchStream]struct{}),
	}

	server := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(server, agent)
	go func() {
		// Serve returns when Stop is called from the cleanup below, which is
		// the normal end of every test using the agent.
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	return agent
}

// WatchTopology implements the subset of AgentService the driver calls.
//
// Each stream opens with a snapshot of the agent's current state, as the
// protocol promises, and then carries one more for every change a test makes
// afterwards.
func (a *fakeAgent) WatchTopology(_ *agentpb.WatchTopologyRequest, stream agentpb.AgentService_WatchTopologyServer) error {
	sub, err := a.subscribe()
	if err != nil {
		return err
	}
	defer a.unsubscribe(sub)

	for {
		select {
		case resp := <-sub.queue:
			if err := stream.Send(resp); err != nil {
				return err
			}
		case err := <-sub.fail:
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// subscribe registers one stream and seeds it with the current state, or
// reports the failure a test asked every stream to fail with.
func (a *fakeAgent) subscribe() (*watchStream, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.watches++
	if a.rpcErr != nil {
		return nil, a.rpcErr
	}

	sub := &watchStream{
		queue: make(chan *agentpb.WatchTopologyResponse, 16),
		fail:  make(chan error, 1),
	}
	sub.queue <- a.snapshotLocked()
	a.subs[sub] = struct{}{}
	return sub, nil
}

func (a *fakeAgent) unsubscribe(sub *watchStream) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.subs, sub)
}

// snapshotLocked renders the agent's current state as one stream message.
func (a *fakeAgent) snapshotLocked() *agentpb.WatchTopologyResponse {
	if a.notReady {
		return &agentpb.WatchTopologyResponse{
			Status:  agentpb.GetTopologyStatus_TOPOLOGY_NOT_DISCOVERED,
			Version: a.version,
		}
	}
	return &agentpb.WatchTopologyResponse{
		Status:           agentpb.GetTopologyStatus_TOPOLOGY_OK,
		PhysicalTopology: a.topology,
		Version:          a.version,
	}
}

// broadcastLocked fans the current state out to every open stream.
func (a *fakeAgent) broadcastLocked() {
	a.version++
	for sub := range a.subs {
		sub.queue <- a.snapshotLocked()
	}
}

// setTopology replaces what the agent reports, telling every open watch about
// the change. Combined with harness.restart this models hardware that changed
// while the driver was down; on a running driver it models a hot-plug.
func (a *fakeAgent) setTopology(topology *topologypb.HostPhysicalTopology) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.topology = topology
	a.broadcastLocked()
}

// setDiscovering makes the agent report TOPOLOGY_NOT_DISCOVERED, as it does
// between coming up and finishing its first discovery pass. Clearing it
// reports the topology normally.
func (a *fakeAgent) setDiscovering(discovering bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.notReady = discovering
	a.broadcastLocked()
}

// setRPCError makes every stream opened from now on fail at the transport
// level until it is cleared with nil. Streams already open are left alone.
func (a *fakeAgent) setRPCError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.rpcErr = err
}

// breakWatches ends every open stream with err, which is how a running
// driver's watch sees the agent go away: an Unavailable is the agent dying or
// restarting and the client reconnects through it, while anything else is a
// failure reconnecting cannot fix.
//
// Streams opened afterwards are served normally unless setRPCError says
// otherwise, so a test can model an agent that comes back.
func (a *fakeAgent) breakWatches(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.breakWatchesLocked(err)
}

// restartWith models the agent process restarting onto a different topology:
// every open stream is dropped with the Unavailable a dying gRPC server sends
// its clients, and the agent answers the next watch with topology.
//
// The new topology is deliberately not broadcast to the streams being
// dropped, so the only way a driver can learn about it is the initial
// snapshot of a stream it re-established itself. That makes an assertion on
// the published devices an assertion about the reconnect.
func (a *fakeAgent) restartWith(topology *topologypb.HostPhysicalTopology) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.topology = topology
	// A restarted agent counts from zero again, which the client is expected
	// to tolerate because versions only order states within one stream.
	a.version = 0
	a.breakWatchesLocked(status.Error(codes.Unavailable, "fabric manager agent restarting"))
}

func (a *fakeAgent) breakWatchesLocked(err error) {
	for sub := range a.subs {
		select {
		case sub.fail <- err:
		default:
			// Already breaking; the first error is the one that ends it.
		}
	}
}

// watchCount reports how many WatchTopology streams the agent has been asked
// for, whether or not it served them.
func (a *fakeAgent) watchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.watches
}

// Board type enum values as reported by the fabric manager agent, mirroring
// the subset of UMD's BoardType that the profile maps to a board name.
const (
	boardTypeN150           = 3
	boardTypeN300           = 4
	boardTypeP150           = 6
	boardTypeGalaxyWormhole = 9
)

// Architecture strings as the agent reports them. These are passed through to
// the chipArch attribute verbatim, so the fixtures use the form the proto
// documents rather than the shortened form a selector might expect.
const (
	archWormhole  = "wormhole_b0"
	archBlackhole = "blackhole"
)

// Per-ASIC DRAM, as advertised by the agent.
const (
	wormholeChipMemory  = 12 << 30
	blackholeChipMemory = 32 << 30
)

// deviceNodeIDOffset deliberately separates an ASIC's device node id from its
// chip id in every fixture.
//
// They are different things: the chip id is the agent's host-local index,
// while the device node id is the N in /dev/tenstorrent/N that the kernel
// driver assigned, and the proto calls the latter the authoritative mapping.
// Fixtures where the two happen to be equal cannot tell the difference, so a
// profile that injected the wrong one would still look correct. Keeping them
// apart means the CDI assertions pin the right field.
const deviceNodeIDOffset = 100

// asicSpec is the subset of AsicInfo that the Tenstorrent profile reads. It
// keeps the topology fixtures below readable.
type asicSpec struct {
	chipID       uint32
	trayID       uint32
	asicLocation uint32
	boardType    uint32
	arch         string
	memoryBytes  uint64
	mmio         bool
}

// asics expands specs into the AsicInfo list the agent serves.
func asics(specs ...asicSpec) []*topologypb.AsicInfo {
	out := make([]*topologypb.AsicInfo, 0, len(specs))
	for _, spec := range specs {
		asic := &topologypb.AsicInfo{
			// Deliberately above math.MaxInt64 so that the fixtures exercise
			// the profile's decision to render uniqueID as a string: an ASIC
			// ID squeezed into an int64 attribute would lose the high bit.
			UniqueId:      0xFF00_0000_0000_0000 | uint64(spec.chipID),
			BoardType:     spec.boardType,
			TrayId:        spec.trayID,
			AsicLocation:  spec.asicLocation,
			ChipArch:      spec.arch,
			MemoryBytes:   spec.memoryBytes,
			ChipId:        spec.chipID,
			IsMmioCapable: spec.mmio,
		}
		// Only MMIO-capable chips have a host-visible character device and a
		// PCI address. The profile depends on that split, so the fixtures must
		// not hand a device node to a remote chip.
		if spec.mmio {
			nodeID := spec.chipID + deviceNodeIDOffset
			asic.DeviceNodeId = nodeID
			asic.DeviceNodePath = fmt.Sprintf("/dev/tenstorrent/%d", nodeID)
			asic.PciAddress = fmt.Sprintf("0000:%02x:00.0", spec.chipID+1)
		}
		out = append(out, asic)
	}
	return out
}

func topologyOf(specs ...asicSpec) *topologypb.HostPhysicalTopology {
	return &topologypb.HostPhysicalTopology{Asics: asics(specs...)}
}

// n150Topology is a single-chip Wormhole board: one MMIO ASIC, one device.
func n150Topology() *topologypb.HostPhysicalTopology {
	return topologyOf(asicSpec{
		chipID: 0, trayID: 1, asicLocation: 0,
		boardType: boardTypeN150, arch: archWormhole,
		memoryBytes: wormholeChipMemory, mmio: true,
	})
}

// n300Topology is a dual-chip Wormhole board: an MMIO ASIC plus a remote
// sibling on the same tray, which the profile bundles into the MMIO parent so
// that one claim yields both chips.
func n300Topology() *topologypb.HostPhysicalTopology {
	return topologyOf(
		asicSpec{
			chipID: 0, trayID: 1, asicLocation: 0,
			boardType: boardTypeN300, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: true,
		},
		asicSpec{
			chipID: 1, trayID: 1, asicLocation: 1,
			boardType: boardTypeN300, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: false,
		},
	)
}

// p150Topology is a single-chip Blackhole board.
func p150Topology() *topologypb.HostPhysicalTopology {
	return topologyOf(asicSpec{
		chipID: 0, trayID: 1, asicLocation: 0,
		boardType: boardTypeP150, arch: archBlackhole,
		memoryBytes: blackholeChipMemory, mmio: true,
	})
}

// galaxyWormholeTopology is a 6U UBB where every one of the 32 chips is
// PCIe-MMIO, so no bundling happens and each chip surfaces as its own device.
func galaxyWormholeTopology() *topologypb.HostPhysicalTopology {
	specs := make([]asicSpec, 0, 32)
	for chipID := uint32(0); chipID < 32; chipID++ {
		specs = append(specs, asicSpec{
			chipID: chipID, trayID: 1, asicLocation: chipID,
			boardType: boardTypeGalaxyWormhole, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: true,
		})
	}
	return topologyOf(specs...)
}

// remoteOnlyTrayTopology has an n150 alongside a tray carrying nothing but
// remote chips. The remote-only tray has no MMIO peer to be opened through, so
// the profile must drop it rather than advertise devices a container could not
// use.
func remoteOnlyTrayTopology() *topologypb.HostPhysicalTopology {
	return topologyOf(
		asicSpec{
			chipID: 0, trayID: 1, asicLocation: 0,
			boardType: boardTypeN150, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: true,
		},
		asicSpec{
			chipID: 7, trayID: 2, asicLocation: 0,
			boardType: boardTypeN300, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: false,
		},
		asicSpec{
			chipID: 8, trayID: 2, asicLocation: 1,
			boardType: boardTypeN300, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: false,
		},
	)
}

// twoBoardTopology is a host with an n300 on one tray and an n150 on another,
// which is the smallest fixture that proves devices from separate trays stay
// separate and are ordered by chip id.
func twoBoardTopology() *topologypb.HostPhysicalTopology {
	return topologyOf(
		asicSpec{
			chipID: 0, trayID: 1, asicLocation: 0,
			boardType: boardTypeN300, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: true,
		},
		asicSpec{
			chipID: 1, trayID: 1, asicLocation: 1,
			boardType: boardTypeN300, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: false,
		},
		asicSpec{
			chipID: 2, trayID: 2, asicLocation: 0,
			boardType: boardTypeN150, arch: archWormhole,
			memoryBytes: wormholeChipMemory, mmio: true,
		},
	)
}
