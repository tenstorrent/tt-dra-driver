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
	"testing"

	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

// TestCheckpointRoundTripsPreparedDevices pins the checkpoint as a complete
// record of what a claim was prepared with. The driver regenerates CDI spec
// files from it after a reboot, so a field that silently fails to survive the
// round trip would produce a spec that is valid but missing device nodes or
// mounts, which no caller would notice until the workload did.
func TestCheckpointRoundTripsPreparedDevices(t *testing.T) {
	checkpoint := newCheckpoint()
	checkpoint.V1.PreparedClaims["claim-uid"] = profiles.PreparedDevices{
		{
			Device: drapbv1.Device{
				RequestNames: []string{"req-0"},
				PoolName:     testNodeName,
				DeviceName:   "tt-24",
				CdiDeviceIds: []string{"k8s.tenstorrent.com/tenstorrent=claim-uid-tt-24"},
			},
			ContainerEdits: &cdiapi.ContainerEdits{
				ContainerEdits: &cdispec.ContainerEdits{
					Env:         []string{"TENSTORRENT_DEVICE_TT_24_RESOURCE_CLAIM=claim-uid"},
					DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/tenstorrent/24"}},
					Mounts: []*cdispec.Mount{{
						HostPath:      "/dev/hugepages",
						ContainerPath: "/dev/hugepages",
						Options:       []string{"rw", "bind"},
					}},
				},
			},
			AdminAccess: true,
		},
	}

	marshalled, err := checkpoint.MarshalCheckpoint()
	if err != nil {
		t.Fatalf("MarshalCheckpoint: %v", err)
	}

	restored := newCheckpoint()
	if err := restored.UnmarshalCheckpoint(marshalled); err != nil {
		t.Fatalf("UnmarshalCheckpoint: %v", err)
	}
	if err := restored.VerifyChecksum(); err != nil {
		t.Fatalf("VerifyChecksum: %v", err)
	}

	devices := restored.V1.PreparedClaims["claim-uid"]
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	device := devices[0]

	if device.DeviceName != "tt-24" || !device.AdminAccess {
		t.Errorf("device fields did not survive the round trip: %+v", device)
	}
	if device.ContainerEdits == nil || device.ContainerEdits.ContainerEdits == nil {
		t.Fatalf("container edits did not survive the round trip: %+v", device)
	}
	edits := device.ContainerEdits
	if len(edits.DeviceNodes) != 1 || edits.DeviceNodes[0].Path != "/dev/tenstorrent/24" {
		t.Errorf("device nodes did not survive the round trip: %+v", edits.DeviceNodes)
	}
	if len(edits.Mounts) != 1 || edits.Mounts[0].HostPath != "/dev/hugepages" {
		t.Errorf("mounts did not survive the round trip: %+v", edits.Mounts)
	}
	if len(edits.Env) != 1 {
		t.Errorf("env did not survive the round trip: %+v", edits.Env)
	}
}
