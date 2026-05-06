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
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

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

// Profile is the Tenstorrent device profile.
type Profile struct {
	profiles.NoopConfigHandler

	nodeName string
	topology fabricmanager.TopologyClient
}

// NewProfile constructs a Tenstorrent profile that publishes one
// ResourceSlice device per ASIC reported by the fabric manager agent on the
// given node. The topology client must be non-nil; pass a *fabricmanager.AgentClient
// in production and a fake in tests.
func NewProfile(nodeName string, topology fabricmanager.TopologyClient) Profile {
	return Profile{
		nodeName: nodeName,
		topology: topology,
	}
}

// EnumerateDevices implements profiles.Profile.
func (p Profile) EnumerateDevices(ctx context.Context) (resourceslice.DriverResources, error) {
	if p.topology == nil {
		return resourceslice.DriverResources{}, fmt.Errorf("tenstorrent profile: fabric manager agent client is not configured")
	}

	hostTopology, err := p.topology.GetTopology(ctx)
	if err != nil {
		return resourceslice.DriverResources{}, fmt.Errorf("tenstorrent profile: get topology from fabric manager agent: %w", err)
	}

	asics := hostTopology.GetAsics()
	devices := make([]resourceapi.Device, 0, len(asics))
	for _, asic := range asics {
		devices = append(devices, asicToDevice(asic))
	}

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

// asicToDevice converts a fabric-manager AsicInfo into a ResourceSlice
// device. The device name uses the host-local chip id so it is stable across
// agent restarts on the same host.
func asicToDevice(asic *topologypb.AsicInfo) resourceapi.Device {
	device := resourceapi.Device{
		Name: fmt.Sprintf("tt-%d", asic.GetChipId()),
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
