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
	"fmt"
	"slices"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// The fake Blackhole Galaxy rev C below is built from the descriptor's tray
// and host groupings rather than from its mesh groupings, so it is an
// independent check on the mesh definitions in mesh.go rather than a
// restatement of them.
//
// Each tray is a 2x4 of ASICs, and the four trays are arranged 2x2 — tray 2
// and tray 4 on the top row, tray 1 and tray 3 below — making the host one
// 4x8 grid. Chips are linked to their orthogonal neighbours in that grid,
// which is the only topology information the mesh code gets.
//
// The horizontal 4x2 meshes should then come out as the grid's four
// column-pairs. That is what the tests assert, and it is a conclusion the
// production code never sees: it works from quadrants of named trays.

const (
	galaxyBlackholeBoardType = 10
	blackholeArch            = "blackhole"
)

// trayRows gives each tray's ASIC locations in row-major order, two rows of
// four, as the descriptor's tray_1..tray_4 groupings lay them out. The
// trays are rotations of one another, which is why the locations run in
// different directions.
var trayRows = map[uint32][2][4]uint32{
	1: {{5, 6, 7, 8}, {1, 2, 3, 4}},
	2: {{1, 2, 3, 4}, {5, 6, 7, 8}},
	3: {{8, 7, 6, 5}, {4, 3, 2, 1}},
	4: {{4, 3, 2, 1}, {8, 7, 6, 5}},
}

// hostTrays places the trays on the host's 2x2, top row first, as the
// descriptor's galaxy_hosts grouping does.
var hostTrays = [2][2]uint32{
	{2, 4},
	{1, 3},
}

// galaxyChip identifies one ASIC of the fake galaxy.
type galaxyChip struct {
	trayID       uint32
	asicLocation uint32
}

// galaxyGrid is the host's 4x8 of ASICs, built by placing each tray's rows
// at its position on the host's 2x2.
func galaxyGrid() [4][8]galaxyChip {
	var grid [4][8]galaxyChip
	for trayRow, row := range hostTrays {
		for trayCol, trayID := range row {
			rows := trayRows[trayID]
			for r, locations := range rows {
				for c, location := range locations {
					grid[trayRow*2+r][trayCol*4+c] = galaxyChip{
						trayID:       trayID,
						asicLocation: location,
					}
				}
			}
		}
	}
	return grid
}

// chipIDFor is the host-local chip id the fake agent reports for an ASIC.
// Any stable assignment would do; this one keeps a tray's chips contiguous,
// as a real host's tend to be.
func chipIDFor(chip galaxyChip) uint32 {
	return (chip.trayID-1)*8 + (chip.asicLocation - 1)
}

func uniqueIDFor(chip galaxyChip) uint64 {
	return 1000 + uint64(chipIDFor(chip))
}

// bhGalaxyRevCTopology builds the 32-ASIC host. locationBase says whether
// the agent numbers ASIC locations from 1, as the descriptor does, or from
// 0; the driver has to cope with either.
func bhGalaxyRevCTopology(locationBase uint32) *topologypb.HostPhysicalTopology {
	grid := galaxyGrid()

	topo := &topologypb.HostPhysicalTopology{
		Quirks: &topologypb.HostTopologyQuirks{IsBhGlxRevC: true},
	}
	for _, row := range grid {
		for _, chip := range row {
			topo.Asics = append(topo.Asics, &topologypb.AsicInfo{
				UniqueId:      uniqueIDFor(chip),
				BoardType:     galaxyBlackholeBoardType,
				TrayId:        chip.trayID,
				AsicLocation:  chip.asicLocation - 1 + locationBase,
				ChipArch:      blackholeArch,
				MemoryBytes:   32 << 30,
				ChipId:        chipIDFor(chip),
				IsMmioCapable: true,
				DeviceNodeId:  chipIDFor(chip),
			})
		}
	}

	// Link every chip to its orthogonal neighbours in the 4x8 grid. This
	// is the whole of what the mesh code can lean on to decide whether a
	// grouping describes this host.
	link := func(a, b galaxyChip) {
		topo.LocalConnections = append(topo.LocalConnections, &topologypb.LocalEthConnection{
			SrcAsicId: uniqueIDFor(a),
			DstAsicId: uniqueIDFor(b),
		})
	}
	for r := range grid {
		for c := range grid[r] {
			if c+1 < len(grid[r]) {
				link(grid[r][c], grid[r][c+1])
			}
			if r+1 < len(grid) {
				link(grid[r][c], grid[r+1][c])
			}
		}
	}
	return topo
}

// wantMeshChipIDs returns the chip ids of the grid's four column-pairs, in
// row-major order: the meshes a horizontal 4x2 should resolve to.
func wantMeshChipIDs() [][]uint32 {
	grid := galaxyGrid()
	var meshes [][]uint32
	for pair := 0; pair < 4; pair++ {
		var ids []uint32
		for r := range grid {
			for _, c := range []int{pair * 2, pair*2 + 1} {
				ids = append(ids, chipIDFor(grid[r][c]))
			}
		}
		meshes = append(meshes, ids)
	}
	return meshes
}

// meshDevicesOf returns the published mesh devices keyed by name.
func meshDevicesOf(devices map[string]resourceapi.Device) map[string]resourceapi.Device {
	meshes := make(map[string]resourceapi.Device)
	for name, device := range devices {
		if attr, ok := device.Attributes["unit"]; ok && attr.StringValue != nil && *attr.StringValue == UnitMesh {
			meshes[name] = device
		}
	}
	return meshes
}

// chipIDsOfMesh reads back the published mesh order.
func chipIDsOfMesh(t *testing.T, device resourceapi.Device) []uint32 {
	t.Helper()
	var ids []uint32
	for _, field := range strings.Split(stringAttr(t, device, "chipIDs"), ",") {
		var id uint32
		if _, err := fmt.Sscanf(field, "%d", &id); err != nil {
			t.Fatalf("device %q has unreadable chipIDs %q", device.Name, field)
		}
		ids = append(ids, id)
	}
	return ids
}

func meshOptions() Options {
	return Options{TrayDevices: true, MeshDevices: true}
}

// TestMeshDevicesOnBlackholeGalaxyRevC is the main assertion: the four
// horizontal 4x2 groupings resolve to the four column-pairs of the host's
// 4x8 grid, each in mesh coordinate order.
func TestMeshDevicesOnBlackholeGalaxyRevC(t *testing.T) {
	_, resources := publishFrom(t, meshOptions(), bhGalaxyRevCTopology(1))
	devices := devicesByName(t, resources)
	meshes := meshDevicesOf(devices)

	if len(meshes) != 4 {
		t.Fatalf("published mesh devices %v, want four", sortedDeviceNames(meshes))
	}

	want := wantMeshChipIDs()
	for _, wantIDs := range want {
		name := deviceNameForMesh("4x2h", wantIDs[0])
		device, ok := meshes[name]
		if !ok {
			t.Errorf("no mesh device %q; published %v", name, sortedDeviceNames(meshes))
			continue
		}
		if got := chipIDsOfMesh(t, device); !slices.Equal(got, wantIDs) {
			t.Errorf("mesh %q covers chips %v, want %v in mesh order", name, got, wantIDs)
		}
		if got := stringAttr(t, device, "meshName"); got != "4x2_Mesh_horizontal" {
			t.Errorf("mesh %q meshName = %q", name, got)
		}
		if got := stringAttr(t, device, "meshShape"); got != "4x2" {
			t.Errorf("mesh %q meshShape = %q, want 4x2", name, got)
		}
		if got := intAttr(t, device, "meshRows"); got != 4 {
			t.Errorf("mesh %q meshRows = %d, want 4", name, got)
		}
		if got := intAttr(t, device, "meshCols"); got != 2 {
			t.Errorf("mesh %q meshCols = %d, want 2", name, got)
		}
		if got := intAttr(t, device, "chipCount"); got != 8 {
			t.Errorf("mesh %q chipCount = %d, want 8", name, got)
		}
		// Every horizontal 4x2 straddles exactly two trays.
		if got := strings.Split(stringAttr(t, device, "trayIDs"), ","); len(got) != 2 {
			t.Errorf("mesh %q spans trays %v, want two", name, got)
		}
		if got := stringAttr(t, device, "boardName"); got != "galaxy-blackhole" {
			t.Errorf("mesh %q boardName = %q", name, got)
		}
	}
}

// TestMeshDevicesAcceptZeroBasedLocations covers the agent that numbers
// ASIC locations from 0 rather than from 1 as the descriptor does. The same
// meshes must come out either way.
func TestMeshDevicesAcceptZeroBasedLocations(t *testing.T) {
	_, resources := publishFrom(t, meshOptions(), bhGalaxyRevCTopology(0))
	meshes := meshDevicesOf(devicesByName(t, resources))

	if len(meshes) != 4 {
		t.Fatalf("published mesh devices %v, want four", sortedDeviceNames(meshes))
	}
	for _, wantIDs := range wantMeshChipIDs() {
		name := deviceNameForMesh("4x2h", wantIDs[0])
		device, ok := meshes[name]
		if !ok {
			t.Errorf("no mesh device %q; published %v", name, sortedDeviceNames(meshes))
			continue
		}
		if got := chipIDsOfMesh(t, device); !slices.Equal(got, wantIDs) {
			t.Errorf("mesh %q covers chips %v, want %v", name, got, wantIDs)
		}
	}
}

// TestMeshDevicesNeedTheRevCQuirk checks the gate. Rev B of this hardware
// is cabled differently, so without the agent saying rev C the groupings
// are not known to describe the host and nothing is published.
func TestMeshDevicesNeedTheRevCQuirk(t *testing.T) {
	topo := bhGalaxyRevCTopology(1)
	topo.Quirks = nil

	_, resources := publishFrom(t, meshOptions(), topo)
	if meshes := meshDevicesOf(devicesByName(t, resources)); len(meshes) != 0 {
		t.Errorf("published mesh devices %v without the rev-C quirk, want none", sortedDeviceNames(meshes))
	}
}

// TestMeshDevicesNeedTheLinkGraph covers the host whose topology carries no
// intra-host connections — an agent discovering through the sysfs fallback,
// say. The chips may be in the right slots, but nothing confirms they are
// wired as the grouping claims, so no mesh is published.
func TestMeshDevicesNeedTheLinkGraph(t *testing.T) {
	topo := bhGalaxyRevCTopology(1)
	topo.LocalConnections = nil

	_, resources := publishFrom(t, meshOptions(), topo)
	if meshes := meshDevicesOf(devicesByName(t, resources)); len(meshes) != 0 {
		t.Errorf("published mesh devices %v with no link graph to confirm them, want none", sortedDeviceNames(meshes))
	}
}

// TestMeshDevicesDropUnwiredMesh is the safety property: a host that is
// cabled differently from what the grouping describes must not get a device
// claiming that topology. Only the mesh that lost a link goes; the rest are
// unaffected.
func TestMeshDevicesDropUnwiredMesh(t *testing.T) {
	topo := bhGalaxyRevCTopology(1)

	// Cut the link between the first mesh's origin chip and the chip
	// beside it in the mesh.
	first := wantMeshChipIDs()[0]
	cut := func(a, b uint32) {
		kept := topo.LocalConnections[:0]
		for _, conn := range topo.LocalConnections {
			pair := linkKey(conn.GetSrcAsicId(), conn.GetDstAsicId())
			if pair == linkKey(1000+uint64(a), 1000+uint64(b)) {
				continue
			}
			kept = append(kept, conn)
		}
		topo.LocalConnections = kept
	}
	before := len(topo.LocalConnections)
	cut(first[0], first[1])
	if len(topo.LocalConnections) == before {
		t.Fatal("test cut no link; the fake topology does not connect the chips it should")
	}

	_, resources := publishFrom(t, meshOptions(), topo)
	meshes := meshDevicesOf(devicesByName(t, resources))

	if _, ok := meshes[deviceNameForMesh("4x2h", first[0])]; ok {
		t.Error("published a mesh device whose chips are not connected as the grouping describes")
	}
	if len(meshes) != 3 {
		t.Errorf("published mesh devices %v, want the other three to survive", sortedDeviceNames(meshes))
	}
}

// TestMeshDevicesSkipNonAnchorChips covers hardware where a mesh slot lands
// on a chip that is not separately allocatable. Such a chip has no
// character device of its own — it is reached through its MMIO parent — so
// a mesh covering it cannot be handed over as described.
func TestMeshDevicesSkipNonAnchorChips(t *testing.T) {
	topo := bhGalaxyRevCTopology(1)
	// Tray 2 location 1 is the origin of the first mesh.
	for _, asic := range topo.GetAsics() {
		if asic.GetTrayId() == 2 && asic.GetAsicLocation() == 1 {
			asic.IsMmioCapable = false
		}
	}

	_, resources := publishFrom(t, meshOptions(), topo)
	meshes := meshDevicesOf(devicesByName(t, resources))

	if len(meshes) != 3 {
		t.Errorf("published mesh devices %v, want the three that do not cover the non-MMIO chip", sortedDeviceNames(meshes))
	}
}

// TestMeshDevicesOptOut checks that the flag alone suppresses them.
func TestMeshDevicesOptOut(t *testing.T) {
	_, resources := publishFrom(t, Options{TrayDevices: true, MeshDevices: false}, bhGalaxyRevCTopology(1))
	if meshes := meshDevicesOf(devicesByName(t, resources)); len(meshes) != 0 {
		t.Errorf("published mesh devices %v with meshes disabled, want none", sortedDeviceNames(meshes))
	}
}

// TestMeshApplyConfig checks that allocating a mesh hands the container
// every chip in it, one character device per chip.
func TestMeshApplyConfig(t *testing.T) {
	profile, _ := publishFrom(t, meshOptions(), bhGalaxyRevCTopology(1))

	want := wantMeshChipIDs()[0]
	name := deviceNameForMesh("4x2h", want[0])
	edits, err := profile.ApplyConfig(nil, []*resourceapi.DeviceRequestAllocationResult{
		{Request: "mesh", Driver: DefaultDriverName, Pool: testNodeName, Device: name},
	})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	edit, ok := edits[name]
	if !ok {
		t.Fatalf("no container edits for mesh device %q", name)
	}
	var gotPaths []string
	for _, node := range edit.DeviceNodes {
		gotPaths = append(gotPaths, node.Path)
	}
	var wantPaths []string
	for _, chipID := range want {
		wantPaths = append(wantPaths, fmt.Sprintf("/dev/tenstorrent/%d", chipID))
	}
	slices.Sort(gotPaths)
	slices.Sort(wantPaths)
	if !slices.Equal(gotPaths, wantPaths) {
		t.Errorf("mesh %q exposes %v, want %v", name, gotPaths, wantPaths)
	}
}

// TestMeshAllocationExclusion runs the upstream allocator over the
// published slices, which is where a mesh staying exclusive with its chips,
// its trays and the meshes beside it is actually decided.
func TestMeshAllocationExclusion(t *testing.T) {
	_, resources := publishFrom(t, meshOptions(), bhGalaxyRevCTopology(1))

	meshes := wantMeshChipIDs()
	firstMesh := deviceNameForMesh("4x2h", meshes[0][0])
	secondMesh := deviceNameForMesh("4x2h", meshes[1][0])

	// Both meshes of trays 1 and 2 sit on those two trays; the other two
	// sit on trays 3 and 4.
	for _, tc := range []struct {
		name      string
		allocated []string
		claim     *resourceapi.ResourceClaim
		want      bool
	}{
		{
			name:  "mesh is allocatable while nothing is in use",
			claim: claimFor("mesh", meshClassName, 1, meshSelector(meshes[0][0])),
			want:  true,
		},
		{
			name:      "mesh is blocked by one of its chips",
			allocated: []string{deviceNameForChip(meshes[0][3])},
			claim:     claimFor("mesh", meshClassName, 1, meshSelector(meshes[0][0])),
			want:      false,
		},
		{
			name:      "chip is blocked by the mesh covering it",
			allocated: []string{firstMesh},
			claim:     claimFor("chip", chipClassName, 1, chipSelector(int(meshes[0][3]))),
			want:      false,
		},
		{
			name:      "a chip outside the mesh is unaffected",
			allocated: []string{firstMesh},
			claim:     claimFor("chip", chipClassName, 1, chipSelector(int(meshes[1][0]))),
			want:      true,
		},
		{
			name:      "the mesh beside it stays allocatable",
			allocated: []string{firstMesh},
			claim:     claimFor("mesh", meshClassName, 1, meshSelector(meshes[1][0])),
			want:      true,
		},
		{
			name:      "mesh is blocked by a tray it straddles",
			allocated: []string{deviceNameForTray(2)},
			claim:     claimFor("mesh", meshClassName, 1, meshSelector(meshes[0][0])),
			want:      false,
		},
		{
			name:      "tray is blocked by a mesh straddling it",
			allocated: []string{firstMesh},
			claim:     claimFor("tray", trayClassName, 1, traySelector(2)),
			want:      false,
		},
		{
			name:      "a tray the mesh does not touch is unaffected",
			allocated: []string{firstMesh},
			claim:     claimFor("tray", trayClassName, 1, traySelector(3)),
			want:      true,
		},
		{
			name:      "two disjoint meshes can be held at once",
			allocated: []string{firstMesh, secondMesh},
			claim:     claimFor("mesh", meshClassName, 1, meshSelector(meshes[2][0])),
			want:      true,
		},
		{
			name:  "every mesh on the host can be claimed at once",
			claim: claimFor("meshes", meshClassName, 4, ""),
			want:  true,
		},
		{
			name:  "a chip class request never binds a mesh",
			claim: claimFor("chips", chipClassName, 32, ""),
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

// TestJoinAttrListElidesOverlongValues covers the guard on attribute value
// length: one over-long value makes the apiserver reject the whole slice,
// so the list gives way instead.
func TestJoinAttrListElidesOverlongValues(t *testing.T) {
	var ids []string
	for i := 0; i < 32; i++ {
		ids = append(ids, fmt.Sprintf("%d", 18080982798508798832-uint64(i)))
	}

	got := joinAttrList(ids)
	if len(got) > resourceapi.DeviceAttributeMaxValueLength {
		t.Errorf("joined value is %d bytes, want at most %d", len(got), resourceapi.DeviceAttributeMaxValueLength)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("joined value %q does not show that it was elided", got)
	}
	if !strings.HasPrefix(got, ids[0]) {
		t.Errorf("joined value %q does not start with the first id", got)
	}

	short := []string{"1", "2", "3"}
	if got := joinAttrList(short); got != "1,2,3" {
		t.Errorf("joinAttrList(%v) = %q, want it left alone", short, got)
	}
}
