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

package tenstorrent

import (
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// Mesh devices.
//
// A chip device is one ASIC and a tray device is one board, but a
// multi-chip workload asks for neither: it wants a set of chips wired into
// a specific rectangular topology, in a specific order, because that order
// is the coordinate system its mesh graph descriptor is built on.
//
// Which sets are valid is a property of how a platform is cabled, and it
// is already written down per platform in the Physical Grouping Descriptors
// the fabric manager selects from the physical system descriptor. The
// definitions below are one of those groupings, transcribed by hand:
// `4x2_Mesh_horizontal` on a Blackhole Galaxy rev C. Deriving them from the
// descriptors themselves is the next step; see
// design/mesh-resource-slices.md.
//
// Nothing here is published unless the agent reports the rev-C quirk, and
// no individual mesh is published unless its chips are both present on the
// host and actually linked to each other in the way the grouping claims.

// meshSlot is one position in a mesh, addressed the way a PGD addresses an
// ASIC: by tray and by location within that tray. asicLocation is 1-based,
// matching the descriptor's ASIC_LOCATION_N enum; what the agent reports on
// the wire is reconciled with it in meshIndex.
type meshSlot struct {
	trayID       uint32
	asicLocation uint32
}

// meshDefinition is one grouping from a PGD: a named mesh of a given shape
// whose slots are listed in row-major order.
type meshDefinition struct {
	// name is the grouping's name in the descriptor, published as the
	// `meshName` attribute so a claim can select on it.
	name string

	// slug is the short form used in device names. It has to stay stable:
	// it is part of the published device name.
	slug string

	rows int
	cols int

	// slots are the mesh positions in row-major order, which is also the
	// order the resolved chips are published in.
	slots []meshSlot
}

// shape is the "<rows>x<cols>" form published as the `meshShape` attribute.
func (d meshDefinition) shape() string {
	return fmt.Sprintf("%dx%d", d.rows, d.cols)
}

// bhGalaxyRevCMeshes returns the `4x2_Mesh_horizontal` groupings of a
// Blackhole Galaxy rev C.
//
// The descriptor lays the host out as a 2x2 of trays — tray 2 and tray 4 on
// the top row, tray 1 and tray 3 beneath them — and defines a horizontal
// 4x2 as a HALFTRAY_2 quadrant stacked on a HALFTRAY_1 quadrant. Each tray
// is a 2x4 of ASICs and contributes two quadrants, so a tray pairs with the
// tray below it, quadrant for column-aligned quadrant: tray 2 with tray 1,
// tray 4 with tray 3. That yields the four meshes below, each of them 4
// rows of 2 chips.
//
// The slot order within each quadrant is the descriptor's own, so the
// published chip order is the mesh coordinate order a runtime expects.
func bhGalaxyRevCMeshes() []meshDefinition {
	// mesh builds one definition from the two quadrants it stacks: the
	// HALFTRAY_2 quadrant on top, the HALFTRAY_1 quadrant below.
	mesh := func(topTray uint32, top [4]uint32, bottomTray uint32, bottom [4]uint32) meshDefinition {
		slots := make([]meshSlot, 0, 8)
		for _, loc := range top {
			slots = append(slots, meshSlot{trayID: topTray, asicLocation: loc})
		}
		for _, loc := range bottom {
			slots = append(slots, meshSlot{trayID: bottomTray, asicLocation: loc})
		}
		return meshDefinition{
			name:  "4x2_Mesh_horizontal",
			slug:  "4x2h",
			rows:  4,
			cols:  2,
			slots: slots,
		}
	}

	return []meshDefinition{
		mesh(2, [4]uint32{1, 2, 5, 6}, 1, [4]uint32{5, 6, 1, 2}),
		mesh(2, [4]uint32{3, 4, 7, 8}, 1, [4]uint32{7, 8, 3, 4}),
		mesh(4, [4]uint32{4, 3, 8, 7}, 3, [4]uint32{8, 7, 4, 3}),
		mesh(4, [4]uint32{2, 1, 6, 5}, 3, [4]uint32{6, 5, 2, 1}),
	}
}

// meshDefinitionsFor returns the hardcoded groupings that apply to the
// given host, or nil when none do.
//
// The only platform with transcribed groupings is the Blackhole Galaxy rev
// C, which the agent identifies with a quirk flag: rev B of the same
// hardware is cabled differently, so the flag — not the board type — is
// what makes these definitions correct.
func meshDefinitionsFor(topo *topologypb.HostPhysicalTopology) []meshDefinition {
	if !topo.GetQuirks().GetIsBhGlxRevC() {
		return nil
	}
	return bhGalaxyRevCMeshes()
}

// meshIndex resolves mesh slots to the bundles that can serve them.
type meshIndex struct {
	bySlot map[meshSlot]hostBundle

	// locationBase is the lowest asic_location the agent reported, used to
	// translate the descriptor's 1-based locations onto the wire values.
	locationBase uint32
}

// newMeshIndex indexes bundles by the tray and location of the MMIO chip
// anchoring them.
//
// Only bundle anchors are indexed. An ASIC that is bundled into another
// chip is not separately allocatable — its character device belongs to its
// MMIO parent — so a mesh slot that lands on one cannot be honoured, and
// the mesh is dropped rather than published with a chip it cannot hand over
// on its own.
//
// The descriptor numbers ASIC locations from 1. Whether the agent does too
// is a property of its discovery path rather than something it declares, so
// the base is taken from the lowest location reported and the descriptor's
// locations are shifted onto it.
func newMeshIndex(bundles []hostBundle) meshIndex {
	index := meshIndex{bySlot: make(map[meshSlot]hostBundle, len(bundles))}
	if len(bundles) == 0 {
		return index
	}

	base := bundles[0].mmio.GetAsicLocation()
	for _, bundle := range bundles {
		if loc := bundle.mmio.GetAsicLocation(); loc < base {
			base = loc
		}
	}
	index.locationBase = base

	for _, bundle := range bundles {
		index.bySlot[meshSlot{
			trayID:       bundle.mmio.GetTrayId(),
			asicLocation: bundle.mmio.GetAsicLocation(),
		}] = bundle
	}
	return index
}

// lookup returns the bundle serving a descriptor slot.
func (i meshIndex) lookup(slot meshSlot) (hostBundle, bool) {
	bundle, ok := i.bySlot[meshSlot{
		trayID: slot.trayID,
		// Descriptor locations start at 1; the wire may start at 0.
		asicLocation: slot.asicLocation - 1 + i.locationBase,
	}]
	return bundle, ok
}

// linkSet is the set of ASIC pairs the agent reports as directly connected
// on this host, with each pair held once regardless of direction.
type linkSet map[[2]uint64]struct{}

func newLinkSet(topo *topologypb.HostPhysicalTopology) linkSet {
	links := make(linkSet)
	for _, conn := range topo.GetLocalConnections() {
		links[linkKey(conn.GetSrcAsicId(), conn.GetDstAsicId())] = struct{}{}
	}
	return links
}

func (l linkSet) linked(a, b uint64) bool {
	_, ok := l[linkKey(a, b)]
	return ok
}

func linkKey(a, b uint64) [2]uint64 {
	if a > b {
		a, b = b, a
	}
	return [2]uint64{a, b}
}

// buildMeshDevices resolves every applicable mesh definition against the
// host and returns the devices to publish, along with what each resolves to
// for ApplyConfig.
//
// A definition that does not resolve — chips missing, chips not separately
// allocatable, chips not wired as the grouping claims, or an exclusion that
// cannot be declared — is skipped with a log rather than published. The
// descriptor describes a platform; this host is the evidence of whether it
// is that platform.
func buildMeshDevices(
	topo *topologypb.HostPhysicalTopology,
	bundles []hostBundle,
	layout counterLayout,
	logger klog.Logger,
) ([]resourceapi.Device, map[string]allocatableUnit) {
	definitions := meshDefinitionsFor(topo)
	if len(definitions) == 0 {
		return nil, nil
	}

	// Without the link graph there is no way to tell a correctly cabled
	// host from one that merely has the right chips in the right slots,
	// and a mesh device that is wrong about its topology is worse than no
	// mesh device at all.
	links := newLinkSet(topo)
	if len(links) == 0 {
		logger.Error(nil, "Not publishing Tenstorrent mesh devices: the fabric manager agent reported no intra-host connections, so the chip layout the mesh definitions describe cannot be confirmed",
			"meshDefinitions", len(definitions),
			"discoveryMethod", topo.GetDiscoveryMethod().String(),
		)
		return nil, nil
	}

	index := newMeshIndex(bundles)
	var devices []resourceapi.Device
	units := make(map[string]allocatableUnit)

	for _, definition := range definitions {
		members, ok := resolveMesh(definition, index, links, logger)
		if !ok {
			continue
		}

		consumption, ok := chipCounterConsumption(layout, members)
		if !ok {
			logger.Error(nil, "Not publishing a Tenstorrent mesh device: its chips span more counter sets than one device may consume from, so the mesh could not be kept exclusive with them",
				"meshName", definition.name,
				"chipDevices", len(members),
				"maxCounterSetsPerDevice", resourceapi.ResourceSliceMaxDeviceCounterConsumptionsPerDevice,
			)
			continue
		}

		device := meshDevice(definition, members, consumption)
		devices = append(devices, device)
		units[device.Name] = allocatableUnit{
			kind:    UnitMesh,
			trayID:  members[0].mmio.GetTrayId(),
			bundles: members,
		}
	}
	return devices, units
}

// resolveMesh turns a definition into the bundles serving it, in mesh
// order, or reports false with the reason logged.
func resolveMesh(definition meshDefinition, index meshIndex, links linkSet, logger klog.Logger) ([]hostBundle, bool) {
	members := make([]hostBundle, 0, len(definition.slots))
	for _, slot := range definition.slots {
		bundle, ok := index.lookup(slot)
		if !ok {
			logger.V(2).Info("Skipping a Tenstorrent mesh definition: the host has no separately allocatable chip in one of its slots",
				"meshName", definition.name,
				"trayID", slot.trayID,
				"asicLocation", slot.asicLocation,
			)
			return nil, false
		}
		members = append(members, bundle)
	}

	// The grouping is only valid for the hardware it was transcribed from.
	for _, bundle := range members {
		if name := boardNameFor(bundle.mmio.GetBoardType()); name != "galaxy-blackhole" {
			logger.Info("Skipping a Tenstorrent mesh definition: it covers a chip of another board type",
				"meshName", definition.name,
				"chipID", bundle.mmio.GetChipId(),
				"boardName", name,
			)
			return nil, false
		}
	}

	if gap, ok := unlinkedNeighbours(definition, members, links); !ok {
		logger.Error(nil, "Skipping a Tenstorrent mesh definition: two chips that the grouping places side by side are not connected on this host, so it is not cabled the way the grouping describes",
			"meshName", definition.name,
			"meshShape", definition.shape(),
			"chipID", members[gap[0]].mmio.GetChipId(),
			"neighbourChipID", members[gap[1]].mmio.GetChipId(),
			"meshPosition", gap[0],
			"neighbourMeshPosition", gap[1],
		)
		return nil, false
	}
	return members, true
}

// unlinkedNeighbours checks that every pair of chips adjacent in the mesh
// is directly connected on this host. It returns the first pair that is
// not, by position in the mesh.
//
// Members are in row-major order, so position i sits at row i/cols, column
// i%cols: its right-hand neighbour is i+1 and the one below it is i+cols.
func unlinkedNeighbours(definition meshDefinition, members []hostBundle, links linkSet) ([2]int, bool) {
	for i := range members {
		row, col := i/definition.cols, i%definition.cols
		neighbours := make([]int, 0, 2)
		if col+1 < definition.cols {
			neighbours = append(neighbours, i+1)
		}
		if row+1 < definition.rows {
			neighbours = append(neighbours, i+definition.cols)
		}
		for _, j := range neighbours {
			if !links.linked(members[i].mmio.GetUniqueId(), members[j].mmio.GetUniqueId()) {
				return [2]int{i, j}, false
			}
		}
	}
	return [2]int{}, true
}

// meshDevice builds the ResourceSlice device for a resolved mesh.
//
// The device is named for its origin chip — the one at mesh position 0 —
// so that the name is derived from the hardware rather than from the order
// definitions happen to be listed in, and stays put across agent restarts.
func meshDevice(definition meshDefinition, members []hostBundle, consumption []resourceapi.DeviceCounterConsumption) resourceapi.Device {
	origin := members[0].mmio

	chipCount := 0
	for _, bundle := range members {
		chipCount += 1 + len(bundle.remotes)
	}

	device := resourceapi.Device{
		Name:             deviceNameForMesh(definition.slug, origin.GetChipId()),
		ConsumesCounters: consumption,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"vendor": {
				StringValue: ptr.To(Vendor),
			},
			"unit": {
				StringValue: ptr.To(UnitMesh),
			},
			// meshName is the grouping's name in the descriptor this
			// definition came from, which is the name a workload knows
			// the topology by.
			"meshName": {
				StringValue: ptr.To(definition.name),
			},
			"meshShape": {
				StringValue: ptr.To(definition.shape()),
			},
			// meshRows and meshCols carry the same shape as integers, so
			// a selector can ask for "at least 4 rows" without taking
			// meshShape apart.
			"meshRows": {
				IntValue: ptr.To(int64(definition.rows)),
			},
			"meshCols": {
				IntValue: ptr.To(int64(definition.cols)),
			},
			// chipIDs lists the host-local chip ids in mesh order: the
			// mesh is an ordered structure, and the order is what a
			// runtime needs to place work on it. Position i sits at row
			// i/meshCols, column i%meshCols.
			"chipIDs": {
				StringValue: ptr.To(chipIDList(members)),
			},
			"trayIDs": {
				StringValue: ptr.To(trayIDList(members)),
			},
			// uniqueID of the origin chip, the stable handle for pinning
			// a workload to one particular mesh.
			"uniqueID": {
				StringValue: ptr.To(fmt.Sprintf("%d", origin.GetUniqueId())),
			},
			"boardType": {
				IntValue: ptr.To(int64(origin.GetBoardType())),
			},
			"boardName": {
				StringValue: ptr.To(boardNameFor(origin.GetBoardType())),
			},
			"chipArch": {
				StringValue: ptr.To(origin.GetChipArch()),
			},
			// chipCount is every ASIC the container gets; chipDeviceCount
			// is how many separately allocatable units the mesh takes out
			// of circulation while it is held.
			"chipCount": {
				IntValue: ptr.To(int64(chipCount)),
			},
			"chipDeviceCount": {
				IntValue: ptr.To(int64(len(members))),
			},
		},
	}
	if total := bundlesMemoryBytes(members); total > 0 {
		device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(total), resource.BinarySI),
			},
		}
	}
	return device
}

// chipIDList renders the host-local chip ids of bundles in order, for
// publication as a device attribute.
func chipIDList(bundles []hostBundle) string {
	ids := make([]string, 0, len(bundles))
	for _, bundle := range bundles {
		ids = append(ids, fmt.Sprintf("%d", bundle.mmio.GetChipId()))
	}
	return joinAttrList(ids)
}

// trayIDList renders the distinct trays a set of bundles sits on, in the
// order they first appear.
func trayIDList(bundles []hostBundle) string {
	var ids []string
	seen := make(map[uint32]struct{}, len(bundles))
	for _, bundle := range bundles {
		trayID := bundle.mmio.GetTrayId()
		if _, ok := seen[trayID]; ok {
			continue
		}
		seen[trayID] = struct{}{}
		ids = append(ids, fmt.Sprintf("%d", trayID))
	}
	return joinAttrList(ids)
}

// deviceNameForMesh returns the canonical ResourceSlice device name for a
// mesh anchored on the given chip. The "mesh" infix keeps it from ever
// colliding with a chip or tray device name.
func deviceNameForMesh(slug string, originChipID uint32) string {
	return fmt.Sprintf("tt-mesh-%s-%d", slug, originChipID)
}

// joinAttrList renders a list of ids as one comma-separated attribute
// value, truncated to stay within the API's attribute value limit.
//
// Device attribute values are capped at DeviceAttributeMaxValueLength, and
// a slice holding one over-long value is rejected whole — taking every
// device on the node with it. An elided list is a cosmetic loss; a rejected
// slice is an outage, so the list gives way.
func joinAttrList(ids []string) string {
	const ellipsis = ",…"

	joined := strings.Join(ids, ",")
	if len(joined) <= resourceapi.DeviceAttributeMaxValueLength {
		return joined
	}
	limit := resourceapi.DeviceAttributeMaxValueLength - len(ellipsis)
	for i := len(ids) - 1; i > 0; i-- {
		if joined = strings.Join(ids[:i], ","); len(joined) <= limit {
			break
		}
	}
	return joined + ellipsis
}
