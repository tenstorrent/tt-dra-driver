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
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

const testNodeName = "node-1"

// resourcesWith builds the shape the tenstorrent profile publishes: one pool
// for this node holding one slice with the named devices.
func resourcesWith(names ...string) resourceslice.DriverResources {
	devices := make([]resourceapi.Device, 0, len(names))
	for _, name := range names {
		devices = append(devices, resourceapi.Device{Name: name})
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			testNodeName: {Slices: []resourceslice.Slice{{Devices: devices}}},
		},
	}
}

// newTestState builds just the parts of DeviceState that the resource-set
// bookkeeping touches, so these tests need no CDI root or checkpoint dir.
func newTestState(initial resourceslice.DriverResources) *DeviceState {
	return &DeviceState{
		nodeName:        testNodeName,
		driverResources: initial,
		allocatable:     allocatableFrom(initial, testNodeName),
	}
}

func allocatableNames(s *DeviceState) []string {
	names := make([]string, 0, len(s.allocatable))
	for name := range s.allocatable {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestSetResources(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initial     resourceslice.DriverResources
		update      resourceslice.DriverResources
		wantChanged bool
		wantDevices []string
	}{
		{
			// A watch re-sends a full snapshot whenever it reconnects, so
			// the common update describes what is already published.
			name:        "identical resources are not a change",
			initial:     resourcesWith("chip-0", "chip-1"),
			update:      resourcesWith("chip-0", "chip-1"),
			wantChanged: false,
			wantDevices: []string{"chip-0", "chip-1"},
		},
		{
			name:        "added device",
			initial:     resourcesWith("chip-0"),
			update:      resourcesWith("chip-0", "chip-1"),
			wantChanged: true,
			wantDevices: []string{"chip-0", "chip-1"},
		},
		{
			name:        "removed device",
			initial:     resourcesWith("chip-0", "chip-1"),
			update:      resourcesWith("chip-0"),
			wantChanged: true,
			wantDevices: []string{"chip-0"},
		},
		{
			name:        "reordered devices",
			initial:     resourcesWith("chip-0", "chip-1"),
			update:      resourcesWith("chip-1", "chip-0"),
			wantChanged: true,
			wantDevices: []string{"chip-0", "chip-1"},
		},
		{
			name:        "all devices gone",
			initial:     resourcesWith("chip-0"),
			update:      resourcesWith(),
			wantChanged: true,
			wantDevices: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newTestState(tc.initial)

			if changed := state.SetResources(tc.update); changed != tc.wantChanged {
				t.Errorf("SetResources reported changed=%v, want %v", changed, tc.wantChanged)
			}
			if got := allocatableNames(state); !slices.Equal(got, tc.wantDevices) {
				t.Errorf("allocatable devices are %v, want %v", got, tc.wantDevices)
			}
			if got := state.AllocatableCount(); got != len(tc.wantDevices) {
				t.Errorf("AllocatableCount is %d, want %d", got, len(tc.wantDevices))
			}
		})
	}
}

// fakeDeviceWatch is a profiles.DeviceWatch driven by the test.
type fakeDeviceWatch struct {
	updates chan resourceslice.DriverResources
	err     error
}

func newFakeDeviceWatch() *fakeDeviceWatch {
	return &fakeDeviceWatch{updates: make(chan resourceslice.DriverResources, 1)}
}

func (w *fakeDeviceWatch) Updates() <-chan resourceslice.DriverResources { return w.updates }
func (w *fakeDeviceWatch) Err() error                                    { return w.err }

// stop closes the watch as if it had finished with err.
func (w *fakeDeviceWatch) stop(err error) {
	w.err = err
	close(w.updates)
}

func TestWaitForInitialDevices(t *testing.T) {
	t.Run("returns the first reported resources", func(t *testing.T) {
		watch := newFakeDeviceWatch()
		watch.updates <- resourcesWith("chip-0")

		got, err := waitForInitialDevices(context.Background(), watch, time.Minute)
		if err != nil {
			t.Fatalf("waitForInitialDevices: unexpected error: %v", err)
		}
		if len(got.Pools[testNodeName].Slices[0].Devices) != 1 {
			t.Errorf("got %+v, want the single reported device", got.Pools)
		}
	})

	t.Run("fails when the watch dies first", func(t *testing.T) {
		watch := newFakeDeviceWatch()
		wantErr := errors.New("agent does not implement WatchTopology")
		watch.stop(wantErr)

		_, err := waitForInitialDevices(context.Background(), watch, time.Minute)
		if !errors.Is(err, wantErr) {
			t.Errorf("waitForInitialDevices returned %v, want %v", err, wantErr)
		}
	})

	t.Run("fails when the watch closes without reporting", func(t *testing.T) {
		watch := newFakeDeviceWatch()
		watch.stop(nil)

		if _, err := waitForInitialDevices(context.Background(), watch, time.Minute); err == nil {
			t.Error("waitForInitialDevices succeeded on a watch that reported nothing, want an error")
		}
	})

	t.Run("gives up once the timeout expires", func(t *testing.T) {
		// A watch that never reports: the agent is up but has not finished
		// discovery, or is not there at all.
		watch := newFakeDeviceWatch()

		start := time.Now()
		_, err := waitForInitialDevices(context.Background(), watch, 50*time.Millisecond)
		if err == nil {
			t.Fatal("waitForInitialDevices succeeded without any devices, want a timeout error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("waitForInitialDevices took %v to give up, want roughly the 50ms timeout", elapsed)
		}
	})

	t.Run("returns when the caller goes away", func(t *testing.T) {
		watch := newFakeDeviceWatch()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := waitForInitialDevices(ctx, watch, time.Minute); !errors.Is(err, context.Canceled) {
			t.Errorf("waitForInitialDevices returned %v, want context.Canceled", err)
		}
	})
}

// compile-time check that the fake satisfies the interface the driver uses.
var _ profiles.DeviceWatch = (*fakeDeviceWatch)(nil)
