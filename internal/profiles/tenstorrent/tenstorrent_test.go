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

package tenstorrent

import (
	"context"
	"errors"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"github.com/tenstorrent/tt-dra-driver/internal/fabricmanager"
	agentpb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

const (
	testNodeName = "node-1"
	recvTimeout  = 10 * time.Second
)

// fakeTopologyClient hands out a watch the test drives directly.
type fakeTopologyClient struct {
	watch *fakeTopologyWatch
}

func (c *fakeTopologyClient) WatchTopology(context.Context) fabricmanager.TopologyWatch {
	return c.watch
}

type fakeTopologyWatch struct {
	updates chan fabricmanager.Snapshot
	err     error
}

func newFakeTopologyWatch() *fakeTopologyWatch {
	return &fakeTopologyWatch{updates: make(chan fabricmanager.Snapshot)}
}

func (w *fakeTopologyWatch) Updates() <-chan fabricmanager.Snapshot { return w.updates }
func (w *fakeTopologyWatch) Err() error                             { return w.err }

func (w *fakeTopologyWatch) stop(err error) {
	w.err = err
	close(w.updates)
}

// mmioAsic builds an MMIO-capable ASIC on its own tray, which the profile
// turns into exactly one allocatable device.
func mmioAsic(chipID uint32) *topologypb.AsicInfo {
	return &topologypb.AsicInfo{
		UniqueId:      uint64(chipID) + 1000,
		TrayId:        chipID,
		ChipId:        chipID,
		DeviceNodeId:  chipID,
		ChipArch:      "blackhole",
		IsMmioCapable: true,
	}
}

func usableSnapshot(chipIDs ...uint32) fabricmanager.Snapshot {
	asics := make([]*topologypb.AsicInfo, 0, len(chipIDs))
	for _, chipID := range chipIDs {
		asics = append(asics, mmioAsic(chipID))
	}
	return fabricmanager.Snapshot{
		Status:   agentpb.GetTopologyStatus_TOPOLOGY_OK,
		Topology: &topologypb.HostPhysicalTopology{Asics: asics},
	}
}

func deviceNames(resources resourceslice.DriverResources) []string {
	var names []string
	for _, slice := range resources.Pools[testNodeName].Slices {
		for _, device := range slice.Devices {
			names = append(names, device.Name)
		}
	}
	return names
}

func recvResources(t *testing.T, watch profiles.DeviceWatch) resourceslice.DriverResources {
	t.Helper()
	select {
	case resources, ok := <-watch.Updates():
		if !ok {
			t.Fatal("device watch closed while resources were expected")
		}
		return resources
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for resources")
		return resourceslice.DriverResources{}
	}
}

// sendSnapshot hands a snapshot to the watch under test. The bound matters:
// the profile consumes snapshots one at a time, so a regression that stops
// skipping unusable snapshots would otherwise wedge the test instead of
// failing it.
func sendSnapshot(t *testing.T, watch *fakeTopologyWatch, snapshot fabricmanager.Snapshot) {
	t.Helper()
	select {
	case watch.updates <- snapshot:
	case <-time.After(recvTimeout):
		t.Fatal("timed out handing a snapshot to the profile: it is not consuming snapshots")
	}
}

// TestWatchDevicesReportsEveryChange checks that each usable topology
// becomes a fresh set of resources, which is the whole point of watching.
func TestWatchDevicesReportsEveryChange(t *testing.T) {
	topology := newFakeTopologyWatch()
	profile := NewProfile(testNodeName, &fakeTopologyClient{watch: topology})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := profile.WatchDevices(ctx)

	sendSnapshot(t, topology, usableSnapshot(0))
	if got := deviceNames(recvResources(t, watch)); len(got) != 1 {
		t.Fatalf("first report has devices %v, want exactly one", got)
	}

	sendSnapshot(t, topology, usableSnapshot(0, 1))
	second := deviceNames(recvResources(t, watch))
	if len(second) != 2 {
		t.Errorf("second report has devices %v, want two after the topology grew", second)
	}
}

// TestWatchDevicesSkipsUnusableSnapshots is the safety property: a snapshot
// that says nothing about the host must not be turned into an empty device
// set, or the driver would withdraw every device and evict the workloads
// using them.
func TestWatchDevicesSkipsUnusableSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot fabricmanager.Snapshot
	}{
		{
			name:     "discovery not finished",
			snapshot: fabricmanager.Snapshot{Status: agentpb.GetTopologyStatus_TOPOLOGY_NOT_DISCOVERED},
		},
		{
			name: "discovery failed",
			snapshot: fabricmanager.Snapshot{
				Status: agentpb.GetTopologyStatus_TOPOLOGY_OK,
				Topology: &topologypb.HostPhysicalTopology{
					DiscoveryError: "sysfs fallback validation failed",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			topology := newFakeTopologyWatch()
			profile := NewProfile(testNodeName, &fakeTopologyClient{watch: topology})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			watch := profile.WatchDevices(ctx)

			sendSnapshot(t, topology, tc.snapshot)
			// Follow it with a usable snapshot: receiving that one and not
			// an empty set proves the first was skipped rather than queued.
			sendSnapshot(t, topology, usableSnapshot(0))

			if got := deviceNames(recvResources(t, watch)); len(got) != 1 {
				t.Errorf("first report has devices %v, want the one from the usable snapshot", got)
			}
		})
	}
}

// TestWatchDevicesTracksDeviceMapping checks that ApplyConfig can serve a
// device that only showed up after a topology change, and stops serving one
// that went away.
func TestWatchDevicesTracksDeviceMapping(t *testing.T) {
	topology := newFakeTopologyWatch()
	profile := NewProfile(testNodeName, &fakeTopologyClient{watch: topology})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := profile.WatchDevices(ctx)

	sendSnapshot(t, topology, usableSnapshot(0))
	first := deviceNames(recvResources(t, watch))

	sendSnapshot(t, topology, usableSnapshot(1))
	second := deviceNames(recvResources(t, watch))

	if _, err := profile.ApplyConfig(nil, []*resourceapi.DeviceRequestAllocationResult{{Device: second[0]}}); err != nil {
		t.Errorf("ApplyConfig for the newly reported device %q failed: %v", second[0], err)
	}
	if _, err := profile.ApplyConfig(nil, []*resourceapi.DeviceRequestAllocationResult{{Device: first[0]}}); err == nil {
		t.Errorf("ApplyConfig for the withdrawn device %q succeeded, want an error", first[0])
	}
}

// TestWatchDevicesPropagatesWatchFailure checks that a topology watch which
// gives up closes the device watch with the same reason, so the driver can
// act on it.
func TestWatchDevicesPropagatesWatchFailure(t *testing.T) {
	topology := newFakeTopologyWatch()
	profile := NewProfile(testNodeName, &fakeTopologyClient{watch: topology})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := profile.WatchDevices(ctx)

	wantErr := fabricmanager.ErrWatchUnsupported
	topology.stop(wantErr)

	select {
	case _, ok := <-watch.Updates():
		if ok {
			t.Fatal("device watch reported resources, want it to close")
		}
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for the device watch to close")
	}
	if err := watch.Err(); !errors.Is(err, wantErr) {
		t.Errorf("device watch stopped with %v, want %v", err, wantErr)
	}
}

// TestWatchDevicesWithoutClient covers the misconfigured case: no agent
// client at all must fail through the same channel as any other watch
// failure rather than panicking.
func TestWatchDevicesWithoutClient(t *testing.T) {
	profile := NewProfile(testNodeName, nil)

	watch := profile.WatchDevices(context.Background())
	if _, ok := <-watch.Updates(); ok {
		t.Fatal("watch reported resources without an agent client")
	}
	if watch.Err() == nil {
		t.Error("watch stopped without an error, want one naming the missing client")
	}
}
