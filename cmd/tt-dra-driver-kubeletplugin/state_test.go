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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

const (
	testNodeName   = "test-node"
	testDriverName = "tenstorrent.com"
	testProfile    = "tenstorrent"
)

// fakeProfile reports a fixed set of devices and exposes
// /dev/tenstorrent/<device> for each one it is asked to prepare, so that the
// CDI specs written during a test carry per-device content the way the real
// profile's do.
type fakeProfile struct {
	deviceNames []string
}

// resources is what this profile's watch reports, and what the tests hand to
// NewDeviceState directly rather than going through a watch.
func (p fakeProfile) resources() resourceslice.DriverResources {
	return resourcesWith(p.deviceNames...)
}

// WatchDevices reports the fixed set once and then stays open until ctx is
// cancelled, which is all the DeviceState tests need from a watch.
func (p fakeProfile) WatchDevices(ctx context.Context) profiles.DeviceWatch {
	watch := newFakeDeviceWatch()
	watch.updates <- p.resources()
	go func() {
		<-ctx.Done()
		watch.stop(nil)
	}()
	return watch
}

func (fakeProfile) CommonContainerEdits() *cdiapi.ContainerEdits { return nil }

func (fakeProfile) SchemeBuilder() runtime.SchemeBuilder { return runtime.NewSchemeBuilder() }

func (fakeProfile) Validate(runtime.Object) error { return nil }

func (fakeProfile) ApplyConfig(_ runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	edits := make(profiles.PerDeviceCDIContainerEdits, len(results))
	for _, result := range results {
		edits[result.Device] = &cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{
				DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/tenstorrent/" + result.Device}},
			},
		}
	}
	return edits, nil
}

// newTestConfig returns a Config backed by temporary directories, standing in
// for the CDI root and the kubelet plugin directory. Both paths are returned
// in the Config so that a test can build a second DeviceState over the same
// directories, which is how a driver restart is simulated.
func newTestConfig(t *testing.T, deviceNames ...string) *Config {
	t.Helper()

	cdiRoot := t.TempDir()
	pluginsDir := t.TempDir()
	config := &Config{
		flags: &Flags{
			nodeName:                    testNodeName,
			cdiRoot:                     cdiRoot,
			kubeletPluginsDirectoryPath: pluginsDir,
			profile:                     testProfile,
			driverName:                  testDriverName,
		},
		profile: fakeProfile{deviceNames: deviceNames},
	}

	if err := os.MkdirAll(config.DriverPluginPath(), 0750); err != nil {
		t.Fatalf("create the driver plugin path: %v", err)
	}
	return config
}

// restart returns a copy of config that keeps its directories but enumerates
// the given devices, modelling a driver that comes back up after the node
// rebooted. Pass no devices to model an enumeration that lost them all.
func restart(config *Config, deviceNames ...string) *Config {
	restarted := *config
	flags := *config.flags
	restarted.flags = &flags
	restarted.profile = fakeProfile{deviceNames: deviceNames}
	return &restarted
}

func newTestDeviceState(t *testing.T, config *Config) *DeviceState {
	t.Helper()

	profile, ok := config.profile.(fakeProfile)
	if !ok {
		t.Fatalf("the test config carries a %T, want a fakeProfile", config.profile)
	}

	state, err := NewDeviceState(context.Background(), config, profile.resources())
	if err != nil {
		t.Fatalf("NewDeviceState: %v", err)
	}
	return state
}

func newTestClaim(uid string, deviceNames ...string) *resourceapi.ResourceClaim {
	results := make([]resourceapi.DeviceRequestAllocationResult, 0, len(deviceNames))
	for i, name := range deviceNames {
		results = append(results, resourceapi.DeviceRequestAllocationResult{
			Request: fmt.Sprintf("req-%d", i),
			Driver:  testDriverName,
			Pool:    testNodeName,
			Device:  name,
		})
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Name:      "claim-" + uid,
			Namespace: "default",
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		},
	}
}

// specPath spells out the file name the container runtime resolves a CDI
// device against, rather than deriving it, so that a change to the naming
// shows up here as a failing test.
func specPath(config *Config, transientID string) string {
	return filepath.Join(config.flags.cdiRoot,
		fmt.Sprintf("k8s.%s-%s_%s.yaml", testDriverName, testProfile, transientID))
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func assertExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to exist, stat returned %v", path, err)
	}
}

func TestPrepareWritesClaimSpecFile(t *testing.T) {
	config := newTestConfig(t, "tt-0", "tt-1")
	state := newTestDeviceState(t, config)

	devices, err := state.Prepare(context.Background(), newTestClaim("claim-uid", "tt-0"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(devices) != 1 || devices[0].GetDeviceName() != "tt-0" {
		t.Fatalf("unexpected prepared devices: %+v", devices)
	}

	assertExists(t, specPath(config, cdiCommonDeviceName))
	spec := readFile(t, specPath(config, "claim-uid"))
	for _, want := range []string{"claim-uid-tt-0", "/dev/tenstorrent/tt-0"} {
		if !strings.Contains(spec, want) {
			t.Errorf("claim spec does not contain %q:\n%s", want, spec)
		}
	}
}

// TestPrepareRewritesClaimSpecFileLostWithTheCDIRoot covers the node reboot in
// https://github.com/tenstorrent/tt-dra-driver/issues/33: the CDI root is a
// tmpfs and comes back empty, while the checkpoint under the kubelet plugin
// directory still records the claim as prepared. The kubelet calls
// NodePrepareResources again for every claim it restores, so this is the call
// that has to put the spec file back.
func TestPrepareRewritesClaimSpecFileLostWithTheCDIRoot(t *testing.T) {
	config := newTestConfig(t, "tt-0", "tt-1")
	claim := newTestClaim("claim-uid", "tt-1")

	state := newTestDeviceState(t, config)
	devices, err := state.Prepare(context.Background(), claim)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	before := readFile(t, specPath(config, "claim-uid"))

	// Reboot: the CDI root is emptied, the checkpoint survives.
	entries, err := os.ReadDir(config.flags.cdiRoot)
	if err != nil {
		t.Fatalf("read the CDI root: %v", err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(config.flags.cdiRoot, entry.Name())); err != nil {
			t.Fatalf("empty the CDI root: %v", err)
		}
	}

	restarted := newTestDeviceState(t, restart(config, "tt-0", "tt-1"))
	rePrepared, err := restarted.Prepare(context.Background(), claim)
	if err != nil {
		t.Fatalf("Prepare after restart: %v", err)
	}

	if after := readFile(t, specPath(config, "claim-uid")); after != before {
		t.Errorf("claim spec was not restored identically:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	assertExists(t, specPath(config, cdiCommonDeviceName))

	if len(rePrepared) != len(devices) {
		t.Fatalf("got %d devices after restart, want %d", len(rePrepared), len(devices))
	}
	for i := range devices {
		if got, want := rePrepared[i].GetCdiDeviceIds(), devices[i].GetCdiDeviceIds(); !slices.Equal(got, want) {
			t.Errorf("CDI device IDs changed across the restart: got %v, want %v", got, want)
		}
	}
}

// TestPrepareRewritesClaimSpecFileDeletedUnderIt is the same recovery within a
// single process: Prepare must not trust that a checkpointed claim still has
// its spec on disk.
func TestPrepareRewritesClaimSpecFileDeletedUnderIt(t *testing.T) {
	config := newTestConfig(t, "tt-0")
	state := newTestDeviceState(t, config)
	claim := newTestClaim("claim-uid", "tt-0")

	if _, err := state.Prepare(context.Background(), claim); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	before := readFile(t, specPath(config, "claim-uid"))

	if err := os.Remove(specPath(config, "claim-uid")); err != nil {
		t.Fatalf("remove the claim spec: %v", err)
	}

	if _, err := state.Prepare(context.Background(), claim); err != nil {
		t.Fatalf("Prepare again: %v", err)
	}
	if after := readFile(t, specPath(config, "claim-uid")); after != before {
		t.Errorf("claim spec was not rewritten identically:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestPrepareRejectsCheckpointedDeviceThatIsNoLongerAllocatable guards the
// other half of the reboot: the checkpoint outlives the enumeration, so a chip
// that did not come back leaves an entry naming a device the driver can no
// longer see. Regenerating its spec would inject a device node that is not
// there, so Prepare reports the device by name instead.
func TestPrepareRejectsCheckpointedDeviceThatIsNoLongerAllocatable(t *testing.T) {
	config := newTestConfig(t, "tt-0", "tt-1")
	claim := newTestClaim("claim-uid", "tt-1")

	state := newTestDeviceState(t, config)
	if _, err := state.Prepare(context.Background(), claim); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.Remove(specPath(config, "claim-uid")); err != nil {
		t.Fatalf("remove the claim spec: %v", err)
	}

	// tt-1 does not come back after the reboot.
	restarted := newTestDeviceState(t, restart(config, "tt-0"))
	_, err := restarted.Prepare(context.Background(), claim)
	if err == nil {
		t.Fatal("Prepare succeeded for a device that is no longer allocatable")
	}
	if !strings.Contains(err.Error(), "tt-1") {
		t.Errorf("error does not name the missing device: %v", err)
	}

	assertNotExists(t, specPath(config, "claim-uid"))
}

func TestNewDeviceStateRegeneratesClaimSpecFilesFromCheckpoint(t *testing.T) {
	config := newTestConfig(t, "tt-0", "tt-1")
	state := newTestDeviceState(t, config)

	for uid, device := range map[string]string{"claim-a": "tt-0", "claim-b": "tt-1"} {
		if _, err := state.Prepare(context.Background(), newTestClaim(uid, device)); err != nil {
			t.Fatalf("Prepare %s: %v", uid, err)
		}
	}
	want := map[string]string{
		"claim-a": readFile(t, specPath(config, "claim-a")),
		"claim-b": readFile(t, specPath(config, "claim-b")),
	}

	if err := os.RemoveAll(config.flags.cdiRoot); err != nil {
		t.Fatalf("empty the CDI root: %v", err)
	}
	if err := os.MkdirAll(config.flags.cdiRoot, 0750); err != nil {
		t.Fatalf("recreate the CDI root: %v", err)
	}

	// The specs must be back before the kubelet asks for anything, so this
	// asserts on NewDeviceState alone, with no Prepare call after it.
	newTestDeviceState(t, restart(config, "tt-0", "tt-1"))

	assertExists(t, specPath(config, cdiCommonDeviceName))
	for uid, content := range want {
		if got := readFile(t, specPath(config, uid)); got != content {
			t.Errorf("spec for %s was not regenerated identically:\nwant:\n%s\ngot:\n%s", uid, content, got)
		}
	}
}

// TestNewDeviceStateSkipsClaimsWhoseDevicesAreGone checks that one unusable
// claim does not stop the driver from starting, or from restoring the specs of
// the claims around it.
func TestNewDeviceStateSkipsClaimsWhoseDevicesAreGone(t *testing.T) {
	config := newTestConfig(t, "tt-0", "tt-1")
	state := newTestDeviceState(t, config)

	for uid, device := range map[string]string{"claim-a": "tt-0", "claim-b": "tt-1"} {
		if _, err := state.Prepare(context.Background(), newTestClaim(uid, device)); err != nil {
			t.Fatalf("Prepare %s: %v", uid, err)
		}
	}
	for _, uid := range []string{"claim-a", "claim-b"} {
		if err := os.Remove(specPath(config, uid)); err != nil {
			t.Fatalf("remove the spec for %s: %v", uid, err)
		}
	}

	newTestDeviceState(t, restart(config, "tt-0"))

	assertExists(t, specPath(config, "claim-a"))
	assertNotExists(t, specPath(config, "claim-b"))
}

// TestNewDeviceStatePrunesOrphanedClaimSpecFiles covers a driver that died
// between writing a spec and checkpointing the claim, and makes sure the
// cleanup stays within the driver's own vendor and class.
func TestNewDeviceStatePrunesOrphanedClaimSpecFiles(t *testing.T) {
	config := newTestConfig(t, "tt-0")
	state := newTestDeviceState(t, config)

	if _, err := state.Prepare(context.Background(), newTestClaim("claim-uid", "tt-0")); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	orphan := specPath(config, "orphan-uid")
	foreign := filepath.Join(config.flags.cdiRoot, "nvidia.com-gpu_other.yaml")
	for _, path := range []string{orphan, foreign} {
		if err := os.WriteFile(path, []byte("cdiVersion: 0.3.0\nkind: vendor.com/class\ndevices: []\n"), 0600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	newTestDeviceState(t, restart(config, "tt-0"))

	assertNotExists(t, orphan)
	assertExists(t, foreign)
	assertExists(t, specPath(config, "claim-uid"))
	assertExists(t, specPath(config, cdiCommonDeviceName))
}

func TestUnprepareRemovesClaimSpecFile(t *testing.T) {
	config := newTestConfig(t, "tt-0")
	state := newTestDeviceState(t, config)

	if _, err := state.Prepare(context.Background(), newTestClaim("claim-uid", "tt-0")); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := state.Unprepare(context.Background(), "claim-uid"); err != nil {
		t.Fatalf("Unprepare: %v", err)
	}

	assertNotExists(t, specPath(config, "claim-uid"))
	assertExists(t, specPath(config, cdiCommonDeviceName))

	// A second Unprepare, and an Unprepare of a claim whose spec the reboot
	// already took, must both be no-ops rather than errors.
	if err := state.Unprepare(context.Background(), "claim-uid"); err != nil {
		t.Errorf("Unprepare of an unprepared claim: %v", err)
	}
}

func TestUnprepareToleratesMissingClaimSpecFile(t *testing.T) {
	config := newTestConfig(t, "tt-0")
	state := newTestDeviceState(t, config)

	if _, err := state.Prepare(context.Background(), newTestClaim("claim-uid", "tt-0")); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.Remove(specPath(config, "claim-uid")); err != nil {
		t.Fatalf("remove the claim spec: %v", err)
	}

	if err := state.Unprepare(context.Background(), "claim-uid"); err != nil {
		t.Errorf("Unprepare with the spec file already gone: %v", err)
	}
}

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
