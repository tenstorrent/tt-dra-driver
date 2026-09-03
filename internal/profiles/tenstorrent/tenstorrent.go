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

// Package tenstorrent implements the default DRA profile for Tenstorrent
// accelerators.
//
// Devices are discovered via the Tenstorrent Fabric Manager (TTFM) agent
// running on the same node: EnumerateDevices issues a GetTopology RPC and
// converts each MMIO-capable ASIC the agent reports into a ResourceSlice
// device. Non-MMIO ("remote") ASICs are not separately allocatable: they
// have no host-visible /dev/tenstorrent/<N> entry, so they are bundled
// into the MMIO parent on the same physical tray and travel with it
// whenever it is allocated.
//
// On top of those per-chip devices the profile also publishes one device
// per physical tray, so a workload can claim a whole tray as a single
// unit instead of enumerating its chips. Chip and tray devices describe
// the same silicon, so they are mutually exclusive: allocating a tray
// makes every chip on it unallocatable, and a single allocated chip makes
// its tray unallocatable. That exclusion is expressed with DRA
// partitionable-device counters (KEP-4815) and is therefore enforced by
// the scheduler, not by this driver; see buildTrayCounterSets.
package tenstorrent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/tenstorrent/tt-dra-driver/internal/fabricmanager"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

// ProfileName is the canonical name of this profile. It is used by the
// kubelet plugin to select between profiles and to derive the default DRA
// driver name.
const ProfileName = "tenstorrent"

// DefaultDriverName is the DRA driver name advertised on ResourceSlices and
// DeviceClasses managed by this profile.
const DefaultDriverName = "tenstorrent.com"

// Vendor is the value reported in the `vendor` device attribute.
const Vendor = "tenstorrent"

// Values of the `unit` device attribute, which says what a device stands
// for: a single MMIO-anchored chip bundle, or a whole physical tray.
// DeviceClasses use it to keep the two apart, since both are published by
// the same driver into the same pool.
const (
	UnitChip = "chip"
	UnitTray = "tray"
)

// devicePathFmt is the path of the per-ASIC character device created by the
// Tenstorrent KMD. The "%d" is the device node id reported by the
// fabric-manager agent (the N in /dev/tenstorrent/N), which is the
// authoritative mapping between an ASIC and its host-visible character
// device.
const devicePathFmt = "/dev/tenstorrent/%d"

// Hugepage mount points. Every UMD-based runtime (tt-metal, tt-train,
// tt-exalens, ...) expects both of these to be visible inside the
// container; they are exported as part of the per-driver "common" CDI spec
// rather than per-device because they are node-wide resources.
const (
	hugepagesPath   = "/dev/hugepages"
	hugepages1GPath = "/dev/hugepages-1G"
)

// boardTypeName maps the numeric BoardType enum reported by the FM agent
// (originating from UMD, see cluster_descriptor_types.hpp) to a stable
// human-readable string suitable for CEL selectors on ResourceSlice
// attributes. Selectors that pin a board type should prefer this over the
// raw `boardType` int: the int values change with UMD's enum and are not
// documented externally.
//
// Any value not in this map surfaces as "unknown", including future
// BoardType additions until this map catches up.
var boardTypeName = map[uint32]string{
	0:  "e75",
	1:  "e150",
	2:  "e300",
	3:  "n150",
	4:  "n300",
	5:  "p100",
	6:  "p150",
	7:  "p300",
	8:  "galaxy",           // legacy: UMD BoardType::GALAXY — TG 4U, deprecated in favor of 6U UBB
	9:  "galaxy-wormhole",  // matches KMD sysfs tt_card_type; UMD BoardType::UBB / UBB_WORMHOLE
	10: "galaxy-blackhole", // matches KMD sysfs tt_card_type; UMD BoardType::UBB_BLACKHOLE
	11: "quasar",
	12: "unknown",
}

// boardNameFor returns the stable string for a numeric BoardType, or
// "unknown" when the value has no mapping.
func boardNameFor(bt uint32) string {
	if name, ok := boardTypeName[bt]; ok {
		return name
	}
	return "unknown"
}

// hostBundle represents the bundling decision for a single MMIO-capable
// ASIC: the MMIO chip itself plus any non-MMIO ("remote") chips on the
// same physical tray that are reachable only through it. Remote chips do
// not appear as standalone ResourceSlice devices; their host-visible
// character device is the MMIO parent's, so allocating the parent
// transparently grants access to its remotes.
type hostBundle struct {
	mmio    *topologypb.AsicInfo
	remotes []*topologypb.AsicInfo
}

// trayGroup is every bundle that sits on one physical tray, in ascending
// MMIO chip-id order. It backs both the tray ResourceSlice device and the
// counter that keeps that device mutually exclusive with its chips.
type trayGroup struct {
	trayID  uint32
	bundles []hostBundle
}

// allocatableUnit is what one published ResourceSlice device resolves to
// on this host: a single bundle for a chip device, or every bundle on the
// tray for a tray device. ApplyConfig turns the bundle list into the set
// of character devices a container gets.
type allocatableUnit struct {
	kind    string
	trayID  uint32
	bundles []hostBundle
}

// Options tunes what a Profile advertises.
type Options struct {
	// TrayDevices publishes one additional device per physical tray
	// alongside the per-chip devices, letting a claim take a whole tray
	// as one unit.
	//
	// Tray devices rely on ResourceSlice shared counters to stay
	// mutually exclusive with the chip devices they cover, so they must
	// only be enabled on clusters where the DRAPartitionableDevices
	// feature gate is on. With the gate off the apiserver silently drops
	// the counters and the scheduler would happily hand out a tray and
	// its chips at the same time.
	TrayDevices bool
}

// Profile is the Tenstorrent device profile.
//
// EnumerateDevices populates an internal map from ResourceSlice device
// name to the allocatableUnit that produced it; ApplyConfig consults that
// map to build per-device CDI container edits (e.g. /dev/tenstorrent/<N>
// for every MMIO chip the unit covers).
type Profile struct {
	nodeName    string
	topology    fabricmanager.TopologyClient
	trayDevices bool

	mu           sync.RWMutex
	unitByDevice map[string]allocatableUnit
}

// NewProfile constructs a Tenstorrent profile that publishes one
// ResourceSlice device per MMIO-capable ASIC reported by the fabric
// manager agent on the given node, plus one device per physical tray when
// opts.TrayDevices is set. The topology client must be non-nil; pass a
// *fabricmanager.AgentClient in production and a fake in tests.
func NewProfile(nodeName string, topology fabricmanager.TopologyClient, opts Options) *Profile {
	return &Profile{
		nodeName:     nodeName,
		topology:     topology,
		trayDevices:  opts.TrayDevices,
		unitByDevice: make(map[string]allocatableUnit),
	}
}

// EnumerateDevices implements profiles.Profile.
func (p *Profile) EnumerateDevices(ctx context.Context) (resourceslice.DriverResources, error) {
	if p.topology == nil {
		return resourceslice.DriverResources{}, fmt.Errorf("tenstorrent profile: fabric manager agent client is not configured")
	}

	hostTopology, err := p.topology.GetTopology(ctx)
	if err != nil {
		return resourceslice.DriverResources{}, fmt.Errorf("tenstorrent profile: get topology from fabric manager agent: %w", err)
	}

	logger := klog.FromContext(ctx)
	bundles := bundleHostASICs(hostTopology.GetAsics(), logger)
	trays := groupBundlesByTray(bundles)

	// Counters only exist to keep tray devices exclusive with their
	// chips, so a chips-only profile publishes none at all and keeps the
	// ResourceSlice free of partitionable-device fields.
	var counterSets []resourceapi.CounterSet
	var counterSetByTray map[uint32]string
	if p.trayDevices {
		counterSets, counterSetByTray = buildTrayCounterSets(trays)
	}

	unitByDevice := make(map[string]allocatableUnit, len(bundles)+len(trays))

	// Chip devices come first, in ascending chip-id order, so that the
	// ResourceSlice ordering (which the allocator uses as its first-fit
	// preference) is stable across calls and unchanged by the addition
	// of tray devices.
	devices := make([]resourceapi.Device, 0, len(bundles)+len(trays))
	for _, bundle := range bundles {
		trayID := bundle.mmio.GetTrayId()
		device := chipDevice(bundle, counterSetByTray[trayID])
		devices = append(devices, device)
		unitByDevice[device.Name] = allocatableUnit{
			kind:    UnitChip,
			trayID:  trayID,
			bundles: []hostBundle{bundle},
		}
	}
	if p.trayDevices {
		for _, tray := range trays {
			device := trayDevice(tray, counterSetByTray[tray.trayID], logger)
			devices = append(devices, device)
			unitByDevice[device.Name] = allocatableUnit{
				kind:    UnitTray,
				trayID:  tray.trayID,
				bundles: tray.bundles,
			}
		}
	}

	p.mu.Lock()
	p.unitByDevice = unitByDevice
	p.mu.Unlock()

	logger.V(2).Info("Enumerated Tenstorrent allocatable units",
		"chipDevices", len(bundles),
		"trayDevices", len(devices)-len(bundles),
		"trays", len(trays),
		"counterSets", len(counterSets),
	)

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: buildSlices(devices, counterSets),
			},
		},
	}, nil
}

// CommonContainerEdits implements profiles.Profile.
//
// Tenstorrent runtimes need access to both the 2 MiB and 1 GiB hugepage
// filesystems on the host; these are required by every ASIC and are
// therefore declared once at the "common" level instead of being duplicated
// per device.
func (p *Profile) CommonContainerEdits() *cdiapi.ContainerEdits {
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Mounts: []*cdispec.Mount{
				{
					HostPath:      hugepagesPath,
					ContainerPath: hugepagesPath,
					Options:       []string{"rbind", "rw"},
				},
				{
					HostPath:      hugepages1GPath,
					ContainerPath: hugepages1GPath,
					Options:       []string{"rbind", "rw"},
				},
			},
		},
	}
}

// SchemeBuilder implements profiles.ConfigHandler.
//
// The Tenstorrent profile does not yet define an opaque configuration
// schema; the returned builder is empty so that the kubelet plugin's config
// scheme can still be initialised consistently across profiles.
func (p *Profile) SchemeBuilder() runtime.SchemeBuilder {
	return runtime.NewSchemeBuilder()
}

// Validate implements profiles.ConfigHandler.
func (p *Profile) Validate(config runtime.Object) error {
	if config == nil {
		return nil
	}
	return errors.New("tenstorrent profile: opaque configuration is not supported yet")
}

// ApplyConfig implements profiles.ConfigHandler.
//
// It produces per-device CDI container edits that expose one
// /dev/tenstorrent/<N> character device per MMIO chip the allocated device
// covers: a single node for a chip device, and every MMIO chip on the tray
// for a tray device. Because EnumerateDevices only ever anchors units on
// MMIO-capable ASICs (with any non-MMIO siblings bundled in), each bundle
// corresponds to exactly one host-visible character device.
func (p *Profile) ApplyConfig(config runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if config != nil {
		return nil, errors.New("tenstorrent profile: opaque configuration is not supported yet")
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	edits := make(profiles.PerDeviceCDIContainerEdits, len(results))
	for _, result := range results {
		unit, ok := p.unitByDevice[result.Device]
		if !ok {
			return nil, fmt.Errorf("tenstorrent profile: device %q is not in the latest enumeration", result.Device)
		}
		deviceNodes := make([]*cdispec.DeviceNode, 0, len(unit.bundles))
		for _, bundle := range unit.bundles {
			// Defensive: EnumerateDevices is responsible for anchoring
			// every unit on an MMIO chip. If a non-MMIO one ever leaks
			// through, fail loudly rather than silently producing a CDI
			// spec with a missing device node.
			if !bundle.mmio.GetIsMmioCapable() {
				return nil, fmt.Errorf("tenstorrent profile: %s device %q (tray %d) resolves to non-MMIO ASIC chip %d, which should never appear in the ResourceSlice",
					unit.kind, result.Device, unit.trayID, bundle.mmio.GetChipId())
			}
			deviceNodes = append(deviceNodes, &cdispec.DeviceNode{
				Path:        fmt.Sprintf(devicePathFmt, bundle.mmio.GetDeviceNodeId()),
				Type:        "c",
				Permissions: "rw",
			})
		}
		edits[result.Device] = &cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{
				DeviceNodes: deviceNodes,
			},
		}
	}
	return edits, nil
}

// bundleHostASICs partitions a host's ASICs into one hostBundle per
// MMIO-capable chip. Non-MMIO chips are attached to the MMIO chip with
// the lowest asic_location on their tray; any additional MMIO chip on the
// same tray becomes a standalone bundle (no remotes). ASICs on trays
// without any MMIO chip are dropped with a warning, since no host-visible
// device exists through which they could be opened.
//
// Bundles are returned in ascending chip-id order so that the resulting
// ResourceSlice has stable, deterministic ordering across calls.
func bundleHostASICs(asics []*topologypb.AsicInfo, logger klog.Logger) []hostBundle {
	byTray := make(map[uint32][]*topologypb.AsicInfo)
	for _, asic := range asics {
		byTray[asic.GetTrayId()] = append(byTray[asic.GetTrayId()], asic)
	}

	var bundles []hostBundle
	for trayID, group := range byTray {
		mmio, remote := splitByMmio(group)
		if len(mmio) == 0 {
			logger.Info("Skipping Tenstorrent ASICs on tray with no MMIO peer; they cannot be opened from a container",
				"trayID", trayID,
				"chipIDs", chipIDsOf(remote),
			)
			continue
		}
		sort.Slice(mmio, func(i, j int) bool {
			return mmio[i].GetAsicLocation() < mmio[j].GetAsicLocation()
		})
		// The lowest-asic_location MMIO chip on the tray adopts every
		// remote chip on that tray; any additional MMIO chips become
		// standalone bundles.
		bundles = append(bundles, hostBundle{mmio: mmio[0], remotes: remote})
		for _, extra := range mmio[1:] {
			bundles = append(bundles, hostBundle{mmio: extra})
		}
	}

	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].mmio.GetChipId() < bundles[j].mmio.GetChipId()
	})
	return bundles
}

// groupBundlesByTray collects bundles into per-tray groups, sorted by tray
// id, with each group's bundles left in the ascending chip-id order
// bundleHostASICs produced. Trays are the granularity at which whole-board
// devices are published and at which shared counters are defined.
func groupBundlesByTray(bundles []hostBundle) []trayGroup {
	byTray := make(map[uint32][]hostBundle)
	for _, bundle := range bundles {
		trayID := bundle.mmio.GetTrayId()
		byTray[trayID] = append(byTray[trayID], bundle)
	}

	trays := make([]trayGroup, 0, len(byTray))
	for trayID, group := range byTray {
		trays = append(trays, trayGroup{trayID: trayID, bundles: group})
	}
	sort.Slice(trays, func(i, j int) bool {
		return trays[i].trayID < trays[j].trayID
	})
	return trays
}

// splitByMmio partitions a list of ASICs into MMIO-capable and non-MMIO
// (remote) subsets, preserving relative order within each.
func splitByMmio(asics []*topologypb.AsicInfo) (mmio, remote []*topologypb.AsicInfo) {
	for _, asic := range asics {
		if asic.GetIsMmioCapable() {
			mmio = append(mmio, asic)
		} else {
			remote = append(remote, asic)
		}
	}
	return mmio, remote
}

// chipIDsOf extracts host-local chip ids from an ASIC list, used purely
// for human-readable log output.
func chipIDsOf(asics []*topologypb.AsicInfo) []uint32 {
	ids := make([]uint32, 0, len(asics))
	for _, a := range asics {
		ids = append(ids, a.GetChipId())
	}
	return ids
}

// buildTrayCounterSets defines the shared counters that make a tray device
// and the chip devices on that tray mutually exclusive.
//
// Every tray gets exactly one counter, `tray-<trayID>`, whose value is the
// number of chip devices on the tray. A chip device consumes one unit of
// its tray's counter; the tray device consumes all of them. The scheduler
// therefore rejects a tray whenever any of its chips is already allocated
// (the counter is short by at least one), and rejects every chip once the
// tray is allocated (the counter is fully drained) — which is exactly the
// exclusion this profile needs, without the driver having to track
// allocations itself.
//
// The counters are spread over as many counter sets as needed to respect
// the per-set counter limit; the returned map says which set holds a given
// tray's counter.
func buildTrayCounterSets(trays []trayGroup) ([]resourceapi.CounterSet, map[uint32]string) {
	counterSetByTray := make(map[uint32]string, len(trays))
	if len(trays) == 0 {
		return nil, counterSetByTray
	}

	var counterSets []resourceapi.CounterSet
	for chunk := range slices.Chunk(trays, resourceapi.ResourceSliceMaxCountersPerCounterSet) {
		name := counterSetName(len(counterSets))
		counters := make(map[string]resourceapi.Counter, len(chunk))
		for _, tray := range chunk {
			counters[counterNameForTray(tray.trayID)] = newCounter(int64(len(tray.bundles)))
			counterSetByTray[tray.trayID] = name
		}
		counterSets = append(counterSets, resourceapi.CounterSet{
			Name:     name,
			Counters: counters,
		})
	}
	return counterSets, counterSetByTray
}

// buildSlices packs devices and counter sets into ResourceSlices that stay
// within the API's per-slice limits. Devices and shared counters cannot be
// mixed in one ResourceSlice, so the counters get slices of their own,
// appended after the device slices: the allocator reads counters
// pool-wide, while slice order only influences which devices it tries
// first.
//
// At least one slice is always returned, even for a host with no usable
// ASICs: an empty pool tells the scheduler the driver is running and has
// nothing to offer, which is different from no pool at all.
func buildSlices(devices []resourceapi.Device, counterSets []resourceapi.CounterSet) []resourceslice.Slice {
	maxDevices := resourceapi.ResourceSliceMaxDevices
	if len(counterSets) > 0 {
		// Devices that consume counters are subject to the lower
		// advanced-features cap.
		maxDevices = resourceapi.ResourceSliceMaxDevicesWithAdvancedFeatures
	}

	var out []resourceslice.Slice
	for chunk := range slices.Chunk(devices, maxDevices) {
		out = append(out, resourceslice.Slice{Devices: chunk})
	}
	if len(out) == 0 {
		out = append(out, resourceslice.Slice{Devices: devices})
	}
	for chunk := range slices.Chunk(counterSets, resourceapi.ResourceSliceMaxCounterSets) {
		out = append(out, resourceslice.Slice{SharedCounters: chunk})
	}
	return out
}

// chipDevice converts a hostBundle into a ResourceSlice device. The device
// name uses the MMIO parent's host-local chip id so it is stable across
// agent restarts on the same host. counterSet names the counter set
// holding this chip's tray counter, or is empty when tray devices are
// disabled and no counters are published.
func chipDevice(bundle hostBundle, counterSet string) resourceapi.Device {
	mmio := bundle.mmio
	device := resourceapi.Device{
		Name: deviceNameForChip(mmio.GetChipId()),
		// A chip device holds one unit of its tray's counter for as long
		// as it is allocated, which is what blocks the tray device.
		ConsumesCounters: trayCounterConsumption(counterSet, mmio.GetTrayId(), 1),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"vendor": {
				StringValue: ptr.To(Vendor),
			},
			// unit distinguishes the per-chip devices from the whole-tray
			// devices published alongside them. DeviceClasses select on
			// it so that a request for one never binds the other.
			"unit": {
				StringValue: ptr.To(UnitChip),
			},
			"chipID": {
				IntValue: ptr.To(int64(mmio.GetChipId())),
			},
			"trayID": {
				IntValue: ptr.To(int64(mmio.GetTrayId())),
			},
			"asicLocation": {
				IntValue: ptr.To(int64(mmio.GetAsicLocation())),
			},
			"boardType": {
				IntValue: ptr.To(int64(mmio.GetBoardType())),
			},
			// boardName is the stable, human-readable form of boardType.
			// Prefer it in CEL selectors ("n150", "wh-galaxy", ...) over
			// matching against boardType's raw enum value, which comes
			// from UMD and is not stable across releases.
			"boardName": {
				StringValue: ptr.To(boardNameFor(mmio.GetBoardType())),
			},
			"chipArch": {
				StringValue: ptr.To(mmio.GetChipArch()),
			},
			// uniqueID is uint64 in the proto; render it as a string to
			// avoid losing the high bit when squeezing it into int64.
			"uniqueID": {
				StringValue: ptr.To(fmt.Sprintf("%d", mmio.GetUniqueId())),
			},
			// chipCount is the total number of chips a workload gets when
			// it is allocated this device: the MMIO parent plus any
			// bundled non-MMIO siblings on the same tray. Schedulers can
			// match on it to e.g. require "an N300" (chipCount == 2).
			"chipCount": {
				IntValue: ptr.To(int64(1 + len(bundle.remotes))),
			},
		},
	}
	if pci := mmio.GetPciAddress(); pci != "" {
		device.Attributes["pciAddress"] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(pci),
		}
	}
	if len(bundle.remotes) > 0 {
		chipIDs := make([]string, 0, len(bundle.remotes))
		uniqueIDs := make([]string, 0, len(bundle.remotes))
		for _, r := range bundle.remotes {
			chipIDs = append(chipIDs, fmt.Sprintf("%d", r.GetChipId()))
			uniqueIDs = append(uniqueIDs, fmt.Sprintf("%d", r.GetUniqueId()))
		}
		device.Attributes["remoteChipIDs"] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(strings.Join(chipIDs, ",")),
		}
		device.Attributes["remoteUniqueIDs"] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(strings.Join(uniqueIDs, ",")),
		}
	}
	if total := bundlesMemoryBytes([]hostBundle{bundle}); total > 0 {
		device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(total), resource.BinarySI),
			},
		}
	}
	return device
}

// trayDevice converts a trayGroup into the ResourceSlice device that
// represents the whole physical tray. Allocating it grants the container
// every MMIO chip on the tray at once (plus their bundled remotes) and,
// through the tray counter, takes the tray's chip devices out of the
// allocatable set.
//
// Board-identity attributes (boardType, boardName, chipArch) describe the
// tray as a whole and are taken from its lowest-chip-id bundle. A tray is
// one physical board, so those values are expected to agree across its
// chips; a tray that disagrees is logged and still described by that
// representative chip.
func trayDevice(tray trayGroup, counterSet string, logger klog.Logger) resourceapi.Device {
	representative := tray.bundles[0].mmio
	warnIfHeterogeneous(tray, logger)

	chipCount := 0
	for _, bundle := range tray.bundles {
		chipCount += 1 + len(bundle.remotes)
	}

	device := resourceapi.Device{
		Name: deviceNameForTray(tray.trayID),
		// The tray device consumes its tray counter in full: while it is
		// allocated no chip device on the tray can be, and it cannot be
		// allocated while any of them is.
		ConsumesCounters: trayCounterConsumption(counterSet, tray.trayID, int64(len(tray.bundles))),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"vendor": {
				StringValue: ptr.To(Vendor),
			},
			"unit": {
				StringValue: ptr.To(UnitTray),
			},
			"trayID": {
				IntValue: ptr.To(int64(tray.trayID)),
			},
			"boardType": {
				IntValue: ptr.To(int64(representative.GetBoardType())),
			},
			"boardName": {
				StringValue: ptr.To(boardNameFor(representative.GetBoardType())),
			},
			"chipArch": {
				StringValue: ptr.To(representative.GetChipArch()),
			},
			// uniqueID of the tray's lowest-chip-id MMIO ASIC. It is the
			// stable handle for pinning a workload to one physical tray,
			// the same way chip devices are pinned by uniqueID.
			"uniqueID": {
				StringValue: ptr.To(fmt.Sprintf("%d", representative.GetUniqueId())),
			},
			// chipCount is the total number of ASICs on the tray: every
			// MMIO chip plus their bundled non-MMIO siblings.
			"chipCount": {
				IntValue: ptr.To(int64(chipCount)),
			},
			// chipDeviceCount is how many chip devices this tray covers,
			// i.e. how many separately allocatable units the tray device
			// takes out of circulation while it is held.
			"chipDeviceCount": {
				IntValue: ptr.To(int64(len(tray.bundles))),
			},
		},
	}
	if total := bundlesMemoryBytes(tray.bundles); total > 0 {
		device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(total), resource.BinarySI),
			},
		}
	}
	return device
}

// warnIfHeterogeneous logs when the chips on one tray disagree about board
// type or architecture. That should not happen for a physical tray, and it
// means the tray device's board attributes describe only its
// representative chip.
func warnIfHeterogeneous(tray trayGroup, logger klog.Logger) {
	representative := tray.bundles[0].mmio
	for _, bundle := range tray.bundles[1:] {
		if bundle.mmio.GetBoardType() != representative.GetBoardType() ||
			bundle.mmio.GetChipArch() != representative.GetChipArch() {
			logger.Info("Tenstorrent tray reports chips of differing board type or architecture; the tray device describes its lowest-chip-id ASIC",
				"trayID", tray.trayID,
				"boardType", representative.GetBoardType(),
				"chipArch", representative.GetChipArch(),
				"otherChipID", bundle.mmio.GetChipId(),
				"otherBoardType", bundle.mmio.GetBoardType(),
				"otherChipArch", bundle.mmio.GetChipArch(),
			)
			return
		}
	}
}

// trayCounterConsumption declares that a device consumes count units of
// its tray's shared counter. It returns nil when no counter set was
// published (tray devices disabled), leaving the device free of
// partitionable-device fields.
func trayCounterConsumption(counterSet string, trayID uint32, count int64) []resourceapi.DeviceCounterConsumption {
	if counterSet == "" {
		return nil
	}
	return []resourceapi.DeviceCounterConsumption{
		{
			CounterSet: counterSet,
			Counters: map[string]resourceapi.Counter{
				counterNameForTray(trayID): newCounter(count),
			},
		},
	}
}

// newCounter builds a shared counter quantity. Counters are plain
// integers: the number of chip devices on a tray, or how many of them a
// device holds.
func newCounter(value int64) resourceapi.Counter {
	return resourceapi.Counter{Value: *resource.NewQuantity(value, resource.DecimalSI)}
}

// bundlesMemoryBytes returns the aggregate DRAM advertised by every ASIC
// in the given bundles. Reporting the bundled total (rather than just the
// MMIO chips' memory) lets schedulers express memory requests in terms of
// the physically usable memory the workload will see when allocated the
// device.
func bundlesMemoryBytes(bundles []hostBundle) uint64 {
	var total uint64
	for _, bundle := range bundles {
		total += bundle.mmio.GetMemoryBytes()
		for _, r := range bundle.remotes {
			total += r.GetMemoryBytes()
		}
	}
	return total
}

// deviceNameForChip returns the canonical ResourceSlice device name used
// for an ASIC with the given host-local chip id.
func deviceNameForChip(chipID uint32) string {
	return fmt.Sprintf("tt-%d", chipID)
}

// deviceNameForTray returns the canonical ResourceSlice device name used
// for a whole physical tray. The "tray" infix keeps it from ever colliding
// with a chip device name.
func deviceNameForTray(trayID uint32) string {
	return fmt.Sprintf("tt-tray-%d", trayID)
}

// counterSetName returns the name of the i-th counter set published for
// this node's trays.
func counterSetName(i int) string {
	return fmt.Sprintf("tt-trays-%d", i)
}

// counterNameForTray returns the name of the counter that tracks how much
// of a tray is still free.
func counterNameForTray(trayID uint32) string {
	return fmt.Sprintf("tray-%d", trayID)
}
