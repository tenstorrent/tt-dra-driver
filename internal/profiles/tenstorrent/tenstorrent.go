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
// converts each ASIC the agent reports into a ResourceSlice device.
package tenstorrent

import (
	"context"
	"errors"
	"fmt"
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
// Tenstorrent KMD. The "%d" is the host-local chip id reported by the
// fabric-manager agent (also the integer suffix on the corresponding
// ResourceSlice device name).
const devicePathFmt = "/dev/tenstorrent/%d"

// Hugepage mount points. Every UMD-based runtime (tt-metal, tt-train,
// tt-exalens, ...) expects both of these to be visible inside the
// container; they are exported as part of the per-driver "common" CDI spec
// rather than per-device because they are node-wide resources.
const (
	hugepagesPath   = "/dev/hugepages"
	hugepages1GPath = "/dev/hugepages-1G"
)

// Profile is the Tenstorrent device profile.
//
// EnumerateDevices populates an internal map from ResourceSlice device name
// to the AsicInfo that produced it; ApplyConfig consults that map to build
// per-device CDI container edits (e.g. /dev/tenstorrent/<chipID>).
type Profile struct {
	nodeName string
	topology fabricmanager.TopologyClient

	mu           sync.RWMutex
	asicByDevice map[string]*topologypb.AsicInfo
}

// NewProfile constructs a Tenstorrent profile that publishes one
// ResourceSlice device per ASIC reported by the fabric manager agent on the
// given node. The topology client must be non-nil; pass a *fabricmanager.AgentClient
// in production and a fake in tests.
func NewProfile(nodeName string, topology fabricmanager.TopologyClient) *Profile {
	return &Profile{
		nodeName:     nodeName,
		topology:     topology,
		asicByDevice: make(map[string]*topologypb.AsicInfo),
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

	asics := hostTopology.GetAsics()
	devices := make([]resourceapi.Device, 0, len(asics))
	asicByDevice := make(map[string]*topologypb.AsicInfo, len(asics))
	for _, asic := range asics {
		device := asicToDevice(asic)
		devices = append(devices, device)
		asicByDevice[device.Name] = asic
	}

	p.mu.Lock()
	p.asicByDevice = asicByDevice
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
// /dev/tenstorrent/<chipID> to the workload for every MMIO-capable ASIC in
// the allocation. Non-MMIO (remote) ASICs do not have their own character
// device on the host; for those a warning is logged and no DeviceNode is
// emitted (multi-chip allocation is handled in a follow-up step).
func (p *Profile) ApplyConfig(config runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if config != nil {
		return nil, errors.New("tenstorrent profile: opaque configuration is not supported yet")
	}

	logger := klog.Background()

	p.mu.RLock()
	defer p.mu.RUnlock()

	edits := make(profiles.PerDeviceCDIContainerEdits, len(results))
	for _, result := range results {
		asic, ok := p.asicByDevice[result.Device]
		if !ok {
			return nil, fmt.Errorf("tenstorrent profile: device %q is not in the latest enumeration", result.Device)
		}
		if !asic.GetIsMmioCapable() {
			// Remote chips have no /dev/tenstorrent/<N> entry; until we
			// model multi-chip claims explicitly, leave them with no
			// device-node edits and let later steps decide how to expose
			// them.
			logger.V(2).Info("Skipping CDI device-node edit for non-MMIO Tenstorrent chip",
				"device", result.Device,
				"chipID", asic.GetChipId(),
				"trayID", asic.GetTrayId(),
			)
			continue
		}
		edits[result.Device] = &cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{
				DeviceNodes: []*cdispec.DeviceNode{
					{
						Path:        fmt.Sprintf(devicePathFmt, asic.GetChipId()),
						Type:        "c",
						Permissions: "rw",
					},
				},
			},
		}
	}
	return edits, nil
}

// asicToDevice converts a fabric-manager AsicInfo into a ResourceSlice
// device. The device name uses the host-local chip id so it is stable across
// agent restarts on the same host.
func asicToDevice(asic *topologypb.AsicInfo) resourceapi.Device {
	device := resourceapi.Device{
		Name: deviceNameForChip(asic.GetChipId()),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"vendor": {
				StringValue: ptr.To(Vendor),
			},
			"chipID": {
				IntValue: ptr.To(int64(asic.GetChipId())),
			},
			"trayID": {
				IntValue: ptr.To(int64(asic.GetTrayId())),
			},
			"asicLocation": {
				IntValue: ptr.To(int64(asic.GetAsicLocation())),
			},
			"boardType": {
				IntValue: ptr.To(int64(asic.GetBoardType())),
			},
			"chipArch": {
				StringValue: ptr.To(asic.GetChipArch()),
			},
			// uniqueID is uint64 in the proto; render it as a string to
			// avoid losing the high bit when squeezing it into int64.
			"uniqueID": {
				StringValue: ptr.To(fmt.Sprintf("%d", asic.GetUniqueId())),
			},
			"isMmioCapable": {
				BoolValue: ptr.To(asic.GetIsMmioCapable()),
			},
		},
	}
	if pci := asic.GetPciAddress(); pci != "" {
		device.Attributes["pciAddress"] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(pci),
		}
	}
	if mem := asic.GetMemoryBytes(); mem > 0 {
		device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(mem), resource.BinarySI),
			},
		}
	}
	return device
}

// deviceNameForChip returns the canonical ResourceSlice device name used
// for an ASIC with the given host-local chip id.
func deviceNameForChip(chipID uint32) string {
	return fmt.Sprintf("tt-%d", chipID)
}
