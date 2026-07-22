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
package tenstorrent

import (
	"context"
	"errors"
	"fmt"
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
	8:  "galaxy", // legacy: UMD BoardType::GALAXY — TG 4U, deprecated in favor of 6U UBB
	9:  "galaxy-wormhole", // matches KMD sysfs tt_card_type; UMD BoardType::UBB / UBB_WORMHOLE
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

// Profile is the Tenstorrent device profile.
//
// EnumerateDevices populates an internal map from ResourceSlice device
// name to the hostBundle that produced it; ApplyConfig consults that map
// to build per-device CDI container edits (e.g. /dev/tenstorrent/<chipID>
// for the bundle's MMIO parent).
type Profile struct {
	nodeName string
	topology fabricmanager.TopologyClient

	mu             sync.RWMutex
	bundleByDevice map[string]hostBundle
}

// NewProfile constructs a Tenstorrent profile that publishes one
// ResourceSlice device per MMIO-capable ASIC reported by the fabric
// manager agent on the given node. The topology client must be non-nil;
// pass a *fabricmanager.AgentClient in production and a fake in tests.
func NewProfile(nodeName string, topology fabricmanager.TopologyClient) *Profile {
	return &Profile{
		nodeName:       nodeName,
		topology:       topology,
		bundleByDevice: make(map[string]hostBundle),
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

	devices := make([]resourceapi.Device, 0, len(bundles))
	bundleByDevice := make(map[string]hostBundle, len(bundles))
	for _, bundle := range bundles {
		device := bundleToDevice(bundle)
		devices = append(devices, device)
		bundleByDevice[device.Name] = bundle
	}

	p.mu.Lock()
	p.bundleByDevice = bundleByDevice
	p.mu.Unlock()

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: []resourceslice.Slice{
					{
						Devices: devices,
					},
				},
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
// It produces per-device CDI container edits that expose
// /dev/tenstorrent/<chipID> to the workload for every allocated device.
// Because EnumerateDevices only ever publishes MMIO-capable ASICs (with
// any non-MMIO siblings bundled in), every entry in results corresponds
// to exactly one host-visible character device.
func (p *Profile) ApplyConfig(config runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if config != nil {
		return nil, errors.New("tenstorrent profile: opaque configuration is not supported yet")
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	edits := make(profiles.PerDeviceCDIContainerEdits, len(results))
	for _, result := range results {
		bundle, ok := p.bundleByDevice[result.Device]
		if !ok {
			return nil, fmt.Errorf("tenstorrent profile: device %q is not in the latest enumeration", result.Device)
		}
		// Defensive: EnumerateDevices is responsible for filtering
		// non-MMIO chips out of the ResourceSlice. If one ever leaks
		// through, fail loudly rather than silently producing a CDI spec
		// with no device node.
		if !bundle.mmio.GetIsMmioCapable() {
			return nil, fmt.Errorf("tenstorrent profile: device %q resolves to a non-MMIO ASIC, which should never appear in the ResourceSlice", result.Device)
		}
		edits[result.Device] = &cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{
				DeviceNodes: []*cdispec.DeviceNode{
					{
						Path:        fmt.Sprintf(devicePathFmt, bundle.mmio.GetDeviceNodeId()),
						Type:        "c",
						Permissions: "rw",
					},
				},
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

// bundleToDevice converts a hostBundle into a ResourceSlice device. The
// device name uses the MMIO parent's host-local chip id so it is stable
// across agent restarts on the same host.
func bundleToDevice(bundle hostBundle) resourceapi.Device {
	mmio := bundle.mmio
	device := resourceapi.Device{
		Name: deviceNameForChip(mmio.GetChipId()),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"vendor": {
				StringValue: ptr.To(Vendor),
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
	if total := totalMemoryBytes(bundle); total > 0 {
		device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(total), resource.BinarySI),
			},
		}
	}
	return device
}

// totalMemoryBytes returns the aggregate DRAM advertised by every ASIC in
// the bundle. Reporting the bundled total (rather than just the MMIO
// chip's memory) lets schedulers express memory requests in terms of the
// physically usable memory the workload will see when allocated this
// device.
func totalMemoryBytes(bundle hostBundle) uint64 {
	total := bundle.mmio.GetMemoryBytes()
	for _, r := range bundle.remotes {
		total += r.GetMemoryBytes()
	}
	return total
}

// deviceNameForChip returns the canonical ResourceSlice device name used
// for an ASIC with the given host-local chip id.
func deviceNameForChip(chipID uint32) string {
	return fmt.Sprintf("tt-%d", chipID)
}
