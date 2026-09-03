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
	"fmt"
	"slices"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/ptr"

	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

const testNodeName = "node-1"

// Device class names mirroring the ones the Helm chart installs, including
// their CEL selectors, so the tests exercise the same chip/tray split that
// a real cluster gets.
const (
	chipClassName = "tenstorrent.com"
	trayClassName = "tray.tenstorrent.com"
)

// fakeTopology is a fabricmanager.TopologyClient serving a canned topology.
type fakeTopology struct {
	topology *topologypb.HostPhysicalTopology
	err      error
}

func (f *fakeTopology) GetTopology(context.Context) (*topologypb.HostPhysicalTopology, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.topology, nil
}

// asic builds one AsicInfo. Fields that no test asserts on are left at
// their zero value.
func asic(chipID, trayID, asicLocation, boardType uint32, mmio bool) *topologypb.AsicInfo {
	return &topologypb.AsicInfo{
		UniqueId:      uint64(1000 + chipID),
		BoardType:     boardType,
		TrayId:        trayID,
		AsicLocation:  asicLocation,
		ChipArch:      "wormhole_b0",
		MemoryBytes:   12 << 30,
		ChipId:        chipID,
		IsMmioCapable: mmio,
		DeviceNodeId:  chipID,
	}
}

// testTopology describes a host with two trays:
//
//   - tray 0: an n300, i.e. one MMIO chip (0) that adopts a non-MMIO
//     remote sibling (1). One chip device.
//   - tray 1: a board with two MMIO chips (2 and 3). Two chip devices.
//
// That covers both interesting tray shapes: a tray whose device count
// equals one, and a tray that subsumes several chip devices.
func testTopology() *topologypb.HostPhysicalTopology {
	const n300, p300 = 4, 7
	return &topologypb.HostPhysicalTopology{
		Asics: []*topologypb.AsicInfo{
			asic(0, 0, 0, n300, true),
			asic(1, 0, 1, n300, false),
			asic(2, 1, 0, p300, true),
			asic(3, 1, 1, p300, true),
		},
	}
}

func enumerate(t *testing.T, opts Options) resourceslice.DriverResources {
	t.Helper()
	_, ctx := ktesting.NewTestContext(t)
	profile := NewProfile(testNodeName, &fakeTopology{topology: testTopology()}, opts)
	resources, err := profile.EnumerateDevices(ctx)
	if err != nil {
		t.Fatalf("EnumerateDevices: %v", err)
	}
	return resources
}

// devicesByName flattens every device of every slice in the node's pool.
func devicesByName(t *testing.T, resources resourceslice.DriverResources) map[string]resourceapi.Device {
	t.Helper()
	devices := make(map[string]resourceapi.Device)
	for _, slice := range resources.Pools[testNodeName].Slices {
		for _, device := range slice.Devices {
			if _, dup := devices[device.Name]; dup {
				t.Fatalf("device %q published more than once", device.Name)
			}
			devices[device.Name] = device
		}
	}
	return devices
}

func counterSetsByName(resources resourceslice.DriverResources) map[string]resourceapi.CounterSet {
	counterSets := make(map[string]resourceapi.CounterSet)
	for _, slice := range resources.Pools[testNodeName].Slices {
		for _, counterSet := range slice.SharedCounters {
			counterSets[counterSet.Name] = counterSet
		}
	}
	return counterSets
}

func deviceNames(devices map[string]resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func stringAttr(t *testing.T, device resourceapi.Device, name string) string {
	t.Helper()
	attr, ok := device.Attributes[resourceapi.QualifiedName(name)]
	if !ok || attr.StringValue == nil {
		t.Fatalf("device %q has no string attribute %q", device.Name, name)
	}
	return *attr.StringValue
}

func intAttr(t *testing.T, device resourceapi.Device, name string) int64 {
	t.Helper()
	attr, ok := device.Attributes[resourceapi.QualifiedName(name)]
	if !ok || attr.IntValue == nil {
		t.Fatalf("device %q has no int attribute %q", device.Name, name)
	}
	return *attr.IntValue
}

func TestEnumerateDevicesPublishesChipsAndTrays(t *testing.T) {
	resources := enumerate(t, Options{TrayDevices: true})
	devices := devicesByName(t, resources)

	want := []string{"tt-0", "tt-2", "tt-3", "tt-tray-0", "tt-tray-1"}
	if got := deviceNames(devices); !slices.Equal(got, want) {
		t.Fatalf("published devices = %v, want %v", got, want)
	}

	for _, name := range []string{"tt-0", "tt-2", "tt-3"} {
		if unit := stringAttr(t, devices[name], "unit"); unit != UnitChip {
			t.Errorf("device %q unit = %q, want %q", name, unit, UnitChip)
		}
	}
	for _, name := range []string{"tt-tray-0", "tt-tray-1"} {
		if unit := stringAttr(t, devices[name], "unit"); unit != UnitTray {
			t.Errorf("device %q unit = %q, want %q", name, unit, UnitTray)
		}
	}

	// Tray 0 is the n300: one chip device, two ASICs, and the board
	// identity of its MMIO chip.
	tray0 := devices["tt-tray-0"]
	if got := intAttr(t, tray0, "chipDeviceCount"); got != 1 {
		t.Errorf("tt-tray-0 chipDeviceCount = %d, want 1", got)
	}
	if got := intAttr(t, tray0, "chipCount"); got != 2 {
		t.Errorf("tt-tray-0 chipCount = %d, want 2", got)
	}
	if got := stringAttr(t, tray0, "boardName"); got != "n300" {
		t.Errorf("tt-tray-0 boardName = %q, want %q", got, "n300")
	}

	// Tray 1 has two MMIO chips, so its device covers two chip devices
	// and its memory is the sum of both.
	tray1 := devices["tt-tray-1"]
	if got := intAttr(t, tray1, "chipDeviceCount"); got != 2 {
		t.Errorf("tt-tray-1 chipDeviceCount = %d, want 2", got)
	}
	if got := intAttr(t, tray1, "chipCount"); got != 2 {
		t.Errorf("tt-tray-1 chipCount = %d, want 2", got)
	}
	wantMemory := int64(2 * (12 << 30))
	if got := capacityValue(tray1.Capacity["memory"]); got != wantMemory {
		t.Errorf("tt-tray-1 memory = %d, want %d", got, wantMemory)
	}
}

// quantityValue reads a counter's value. Quantity.Value has a pointer
// receiver and a value read straight out of a map is not addressable,
// hence the copy.
func quantityValue(counter resourceapi.Counter) int64 {
	value := counter.Value
	return value.Value()
}

// capacityValue reads a device capacity, copying it for the same reason as
// quantityValue.
func capacityValue(capacity resourceapi.DeviceCapacity) int64 {
	value := capacity.Value
	return value.Value()
}

// TestEnumerateDevicesCounters pins down the counter arithmetic that makes
// chips and trays mutually exclusive: one counter per tray, sized to the
// tray's chip devices, one unit consumed per chip and all of them by the
// tray.
func TestEnumerateDevicesCounters(t *testing.T) {
	resources := enumerate(t, Options{TrayDevices: true})
	devices := devicesByName(t, resources)

	counterSets := counterSetsByName(resources)
	set, ok := counterSets["tt-trays-0"]
	if !ok {
		t.Fatalf("counter sets = %v, want one named tt-trays-0", counterSets)
	}
	if got := quantityValue(set.Counters["tray-0"]); got != 1 {
		t.Errorf("counter tray-0 = %d, want 1 (one chip device on tray 0)", got)
	}
	if got := quantityValue(set.Counters["tray-1"]); got != 2 {
		t.Errorf("counter tray-1 = %d, want 2 (two chip devices on tray 1)", got)
	}

	// A slice may carry devices or shared counters, never both.
	for i, slice := range resources.Pools[testNodeName].Slices {
		if len(slice.Devices) > 0 && len(slice.SharedCounters) > 0 {
			t.Errorf("slice %d has both devices and shared counters", i)
		}
	}

	consumption := func(name string) map[string]int64 {
		t.Helper()
		got := make(map[string]int64)
		for _, dcc := range devices[name].ConsumesCounters {
			if dcc.CounterSet != "tt-trays-0" {
				t.Errorf("device %q consumes from counter set %q, want tt-trays-0", name, dcc.CounterSet)
			}
			for counter, value := range dcc.Counters {
				got[counter] = quantityValue(value)
			}
		}
		return got
	}

	for name, want := range map[string]map[string]int64{
		"tt-0":      {"tray-0": 1},
		"tt-2":      {"tray-1": 1},
		"tt-3":      {"tray-1": 1},
		"tt-tray-0": {"tray-0": 1},
		"tt-tray-1": {"tray-1": 2},
	} {
		got := consumption(name)
		if len(got) != len(want) {
			t.Errorf("device %q consumes %v, want %v", name, got, want)
			continue
		}
		for counter, value := range want {
			if got[counter] != value {
				t.Errorf("device %q consumes %v, want %v", name, got, want)
				break
			}
		}
	}
}

// TestEnumerateDevicesWithoutTrayDevices asserts the opt-out keeps the
// published slice exactly as it was before trays existed: chips only, and
// no partitionable-device fields that a cluster without the feature gate
// would choke on.
func TestEnumerateDevicesWithoutTrayDevices(t *testing.T) {
	resources := enumerate(t, Options{TrayDevices: false})
	devices := devicesByName(t, resources)

	want := []string{"tt-0", "tt-2", "tt-3"}
	if got := deviceNames(devices); !slices.Equal(got, want) {
		t.Fatalf("published devices = %v, want %v", got, want)
	}
	if counterSets := counterSetsByName(resources); len(counterSets) != 0 {
		t.Errorf("published counter sets = %v, want none", counterSets)
	}
	for name, device := range devices {
		if len(device.ConsumesCounters) != 0 {
			t.Errorf("device %q consumes counters %v, want none", name, device.ConsumesCounters)
		}
	}
	// The pool must still hold exactly one slice, as before.
	if got := len(resources.Pools[testNodeName].Slices); got != 1 {
		t.Errorf("slices = %d, want 1", got)
	}
}

func TestEnumerateDevicesTopologyError(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	profile := NewProfile(testNodeName, &fakeTopology{err: errors.New("agent down")}, Options{TrayDevices: true})
	if _, err := profile.EnumerateDevices(ctx); err == nil {
		t.Fatal("EnumerateDevices succeeded despite a failing topology client")
	}
}

func TestApplyConfig(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	profile := NewProfile(testNodeName, &fakeTopology{topology: testTopology()}, Options{TrayDevices: true})
	if _, err := profile.EnumerateDevices(ctx); err != nil {
		t.Fatalf("EnumerateDevices: %v", err)
	}

	results := []*resourceapi.DeviceRequestAllocationResult{
		{Request: "chip", Driver: DefaultDriverName, Pool: testNodeName, Device: "tt-0"},
		{Request: "tray", Driver: DefaultDriverName, Pool: testNodeName, Device: "tt-tray-1"},
	}
	edits, err := profile.ApplyConfig(nil, results)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	for device, wantPaths := range map[string][]string{
		// The n300's chip device exposes only its MMIO parent; the
		// remote sibling is reached through it.
		"tt-0": {"/dev/tenstorrent/0"},
		// The tray device exposes every MMIO chip on the tray.
		"tt-tray-1": {"/dev/tenstorrent/2", "/dev/tenstorrent/3"},
	} {
		edit, ok := edits[device]
		if !ok {
			t.Fatalf("no container edits for device %q", device)
		}
		var gotPaths []string
		for _, node := range edit.DeviceNodes {
			gotPaths = append(gotPaths, node.Path)
		}
		sort.Strings(gotPaths)
		if !slices.Equal(gotPaths, wantPaths) {
			t.Errorf("device %q exposes %v, want %v", device, gotPaths, wantPaths)
		}
	}
}

func TestApplyConfigUnknownDevice(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	profile := NewProfile(testNodeName, &fakeTopology{topology: testTopology()}, Options{TrayDevices: true})
	if _, err := profile.EnumerateDevices(ctx); err != nil {
		t.Fatalf("EnumerateDevices: %v", err)
	}
	_, err := profile.ApplyConfig(nil, []*resourceapi.DeviceRequestAllocationResult{
		{Request: "chip", Driver: DefaultDriverName, Pool: testNodeName, Device: "tt-nope"},
	})
	if err == nil {
		t.Fatal("ApplyConfig accepted a device that was never enumerated")
	}
}

// --- Allocation tests -------------------------------------------------
//
// These run the upstream DRA allocator over the ResourceSlices this
// profile publishes, which is where the chip/tray exclusion is actually
// enforced. They are the check that the counters mean what the profile
// intends them to mean.

// publishedSlices converts what EnumerateDevices returns into the
// ResourceSlice objects an apiserver would hold, which is what the
// allocator consumes.
func publishedSlices(resources resourceslice.DriverResources) []*resourceapi.ResourceSlice {
	pool := resources.Pools[testNodeName]
	var out []*resourceapi.ResourceSlice
	for i, slice := range pool.Slices {
		out = append(out, &resourceapi.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-slice-%d", testNodeName, i)},
			Spec: resourceapi.ResourceSliceSpec{
				Driver:   DefaultDriverName,
				NodeName: ptr.To(testNodeName),
				Pool: resourceapi.ResourcePool{
					Name:               testNodeName,
					Generation:         1,
					ResourceSliceCount: int64(len(pool.Slices)),
				},
				Devices:        slice.Devices,
				SharedCounters: slice.SharedCounters,
			},
		})
	}
	return out
}

// deviceClasses returns the chip and tray classes as the Helm chart
// defines them, so the selectors are covered too.
type deviceClasses []*resourceapi.DeviceClass

func (c deviceClasses) List() ([]*resourceapi.DeviceClass, error) { return c, nil }

func (c deviceClasses) Get(name string) (*resourceapi.DeviceClass, error) {
	for _, class := range c {
		if class.Name == name {
			return class, nil
		}
	}
	return nil, fmt.Errorf("device class %q not found", name)
}

func testDeviceClasses() deviceClasses {
	class := func(name, expression string) *resourceapi.DeviceClass {
		return &resourceapi.DeviceClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: resourceapi.DeviceClassSpec{
				Selectors: []resourceapi.DeviceSelector{
					{CEL: &resourceapi.CELDeviceSelector{Expression: expression}},
				},
			},
		}
	}
	// Kept character-for-character equivalent to the expressions in
	// helm/tt-dra-driver/templates/deviceclass.yaml, including the has()
	// guard that lets the chip class keep matching devices published
	// before the `unit` attribute existed.
	chip := fmt.Sprintf(
		`device.driver == '%[1]s' && (!has(device.attributes['%[1]s'].unit) || device.attributes['%[1]s'].unit == 'chip')`,
		DefaultDriverName)
	tray := fmt.Sprintf(
		`device.driver == '%[1]s' && has(device.attributes['%[1]s'].unit) && device.attributes['%[1]s'].unit == 'tray'`,
		DefaultDriverName)
	return deviceClasses{class(chipClassName, chip), class(trayClassName, tray)}
}

// chipSelector and traySelector pin a request to one specific chip or
// tray. Device names are not visible to CEL, so the pinning goes through
// the identifying attributes instead.
func chipSelector(chipID int) string {
	return fmt.Sprintf("device.attributes[%q].chipID == %d", DefaultDriverName, chipID)
}

func traySelector(trayID int) string {
	return fmt.Sprintf("device.attributes[%q].trayID == %d", DefaultDriverName, trayID)
}

// claimFor builds a claim for count devices of the given class, optionally
// narrowed by a CEL selector.
func claimFor(name, className string, count int64, selector string) *resourceapi.ResourceClaim {
	request := resourceapi.DeviceRequest{
		Name: "req",
		Exactly: &resourceapi.ExactDeviceRequest{
			DeviceClassName: className,
			AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
			Count:           count,
		},
	}
	if selector != "" {
		request.Exactly.Selectors = []resourceapi.DeviceSelector{
			{CEL: &resourceapi.CELDeviceSelector{Expression: selector}},
		}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{request}},
		},
	}
}

// allocate runs the allocator against the published slices with the given
// devices already allocated, and reports whether the claim could be
// satisfied.
func allocate(t *testing.T, resources resourceslice.DriverResources, allocated []string, claim *resourceapi.ResourceClaim) bool {
	t.Helper()
	_, ctx := ktesting.NewTestContext(t)

	allocatedDevices := sets.New[structured.DeviceID]()
	for _, device := range allocated {
		allocatedDevices.Insert(structured.MakeDeviceID(DefaultDriverName, testNodeName, device))
	}

	allocator, err := structured.NewAllocator(ctx,
		structured.Features{PartitionableDevices: true},
		structured.AllocatedState{
			AllocatedDevices:         allocatedDevices,
			AllocatedSharedDeviceIDs: sets.New[structured.SharedDeviceID](),
			AggregatedCapacity:       structured.NewConsumedCapacityCollection(),
		},
		testDeviceClasses(),
		publishedSlices(resources),
		cel.NewCache(10, cel.Features{}),
	)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName}}
	results, err := allocator.Allocate(ctx, node, []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	return len(results) > 0
}

func TestAllocationExclusion(t *testing.T) {
	resources := enumerate(t, Options{TrayDevices: true})

	for _, tc := range []struct {
		name      string
		allocated []string
		claim     *resourceapi.ResourceClaim
		want      bool
	}{
		{
			name:  "tray is allocatable while nothing is in use",
			claim: claimFor("tray", trayClassName, 1, traySelector(1)),
			want:  true,
		},
		{
			name:      "tray is blocked by one of its chips",
			allocated: []string{"tt-2"},
			claim:     claimFor("tray", trayClassName, 1, traySelector(1)),
			want:      false,
		},
		{
			name:      "chip is blocked by its tray",
			allocated: []string{"tt-tray-1"},
			claim:     claimFor("chip", chipClassName, 1, chipSelector(3)),
			want:      false,
		},
		{
			name:      "sibling chip on a partially used tray stays allocatable",
			allocated: []string{"tt-2"},
			claim:     claimFor("chip", chipClassName, 1, chipSelector(3)),
			want:      true,
		},
		{
			name:      "chip on another tray is unaffected by an allocated tray",
			allocated: []string{"tt-tray-1"},
			claim:     claimFor("chip", chipClassName, 1, chipSelector(0)),
			want:      true,
		},
		{
			name:      "single-chip tray is blocked by its only chip",
			allocated: []string{"tt-0"},
			claim:     claimFor("tray", trayClassName, 1, traySelector(0)),
			want:      false,
		},
		{
			name:      "chip is blocked by its single-chip tray",
			allocated: []string{"tt-tray-0"},
			claim:     claimFor("chip", chipClassName, 1, chipSelector(0)),
			want:      false,
		},
		{
			name:  "every tray on the host can be claimed at once",
			claim: claimFor("trays", trayClassName, 2, ""),
			want:  true,
		},
		{
			name:      "both trays cannot be claimed while a chip is in use",
			allocated: []string{"tt-0"},
			claim:     claimFor("trays", trayClassName, 2, ""),
			want:      false,
		},
		{
			name:  "a chip class request never binds a tray device",
			claim: claimFor("chips", chipClassName, 3, ""),
			want:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := allocate(t, resources, tc.allocated, tc.claim); got != tc.want {
				t.Errorf("allocated=%v claim satisfied = %t, want %t", tc.allocated, got, tc.want)
			}
		})
	}
}

// TestAllocationChipClassIgnoresTrays checks the DeviceClass selectors:
// asking the chip class for every device on the host must bind the three
// chip devices and never a tray device.
func TestAllocationChipClassIgnoresTrays(t *testing.T) {
	resources := enumerate(t, Options{TrayDevices: true})
	_, ctx := ktesting.NewTestContext(t)

	allocator, err := structured.NewAllocator(ctx,
		structured.Features{PartitionableDevices: true},
		structured.AllocatedState{
			AllocatedDevices:         sets.New[structured.DeviceID](),
			AllocatedSharedDeviceIDs: sets.New[structured.SharedDeviceID](),
			AggregatedCapacity:       structured.NewConsumedCapacityCollection(),
		},
		testDeviceClasses(),
		publishedSlices(resources),
		cel.NewCache(10, cel.Features{}),
	)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName}}
	results, err := allocator.Allocate(ctx, node, []*resourceapi.ResourceClaim{
		claimFor("chips", chipClassName, 3, ""),
	})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}

	var got []string
	for _, result := range results[0].Devices.Results {
		got = append(got, result.Device)
	}
	sort.Strings(got)
	want := []string{"tt-0", "tt-2", "tt-3"}
	if !slices.Equal(got, want) {
		t.Errorf("allocated devices = %v, want %v", got, want)
	}
}
