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
// This bootstrap implementation publishes a configurable number of simulated
// devices. Real device discovery (via /dev/tenstorrent/* and the Tenstorrent
// runtime) and per-device CDI container edits are intentionally left out and
// will be added in follow-up work.
package tenstorrent

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

// ProfileName is the canonical name of this profile. It is used by the
// kubelet plugin to select between profiles and to derive the default DRA
// driver name.
const ProfileName = "tenstorrent"

// DefaultDriverName is the DRA driver name advertised on ResourceSlices and
// DeviceClasses managed by this profile.
const DefaultDriverName = "tenstorrent.com"

// DefaultDeviceFamily is the value reported in the `family` device attribute
// for the simulated devices. Real implementations should set this from the
// device's product information.
const DefaultDeviceFamily = "wormhole"

// Profile is the Tenstorrent device profile.
type Profile struct {
	profiles.NoopConfigHandler

	nodeName   string
	numDevices int
}

// NewProfile constructs a Tenstorrent profile that simulates numDevices
// devices on the given node.
func NewProfile(nodeName string, numDevices int) Profile {
	return Profile{
		nodeName:   nodeName,
		numDevices: numDevices,
	}
}

// EnumerateDevices implements profiles.Profile.
func (p Profile) EnumerateDevices() (resourceslice.DriverResources, error) {
	devices := make([]resourceapi.Device, 0, p.numDevices)
	for i := 0; i < p.numDevices; i++ {
		devices = append(devices, resourceapi.Device{
			Name: fmt.Sprintf("tt-%d", i),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"index": {
					IntValue: ptr.To(int64(i)),
				},
				"family": {
					StringValue: ptr.To(DefaultDeviceFamily),
				},
				"vendor": {
					StringValue: ptr.To("tenstorrent"),
				},
			},
		})
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
