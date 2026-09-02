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

// Package profiles defines the abstraction used by the kubelet plugin to
// publish and prepare devices of different shapes (e.g. Wormhole vs.
// Blackhole). New profiles can be added by implementing the Profile interface
// and registering them in the kubelet plugin's main package.
package profiles

import (
	"context"
	"errors"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
)

// PerDeviceCDIContainerEdits maps a device name to the CDI container edits
// that should be applied whenever the device is allocated to a container.
type PerDeviceCDIContainerEdits map[string]*cdiapi.ContainerEdits

// PreparedDevice carries the kubelet-facing device descriptor along with any
// driver-specific data needed during cleanup.
type PreparedDevice struct {
	drapbv1.Device
	ContainerEdits *cdiapi.ContainerEdits
	AdminAccess    bool
}

// PreparedDevices is the per-claim list of devices the driver has prepared.
type PreparedDevices []*PreparedDevice

// GetDevices returns the kubelet-facing slice of Device messages for this
// claim.
func (pds PreparedDevices) GetDevices() []*drapbv1.Device {
	devices := make([]*drapbv1.Device, 0, len(pds))
	for _, pd := range pds {
		devices = append(devices, &pd.Device)
	}
	return devices
}

// Profile describes a kind of device that can be managed by the driver.
type Profile interface {
	ConfigHandler
	// EnumerateDevices returns the resource slice published into the cluster
	// for the node the driver is running on. Implementations may perform
	// remote calls (e.g. to a per-node device-discovery agent) and should
	// honour ctx for cancellation and timeouts.
	EnumerateDevices(ctx context.Context) (resourceslice.DriverResources, error)
	// CommonContainerEdits returns CDI container edits that must be applied
	// to every container that consumes a device managed by this profile,
	// independent of which specific devices were allocated. Returning nil
	// means no node-wide edits are required.
	//
	// These edits are written into the per-driver "common" CDI device that
	// the kubelet plugin prepends to every claim, so a typical use is to
	// declare host bind mounts (e.g. hugepages) or env vars that the
	// runtime needs regardless of the chip selection.
	CommonContainerEdits() *cdiapi.ContainerEdits
}

// ConfigHandler handles opaque configuration set for requests in
// ResourceClaims and DeviceClasses.
type ConfigHandler interface {
	// SchemeBuilder produces a runtime.Scheme for the profile's configuration
	// types.
	SchemeBuilder() runtime.SchemeBuilder
	// Validate returns nil for a valid configuration, or an error explaining
	// why the configuration is invalid.
	Validate(config runtime.Object) error
	// ApplyConfig applies a configuration to a set of device allocation
	// results. When config is nil the profile's default configuration should
	// be applied.
	ApplyConfig(config runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (PerDeviceCDIContainerEdits, error)
}

// NoopConfigHandler implements a ConfigHandler that does not allow any
// configuration. It is useful as a default for profiles that have not yet
// defined their opaque config schema.
type NoopConfigHandler struct{}

// SchemeBuilder implements ConfigHandler.
func (NoopConfigHandler) SchemeBuilder() runtime.SchemeBuilder {
	return runtime.NewSchemeBuilder()
}

// Validate implements ConfigHandler.
func (NoopConfigHandler) Validate(config runtime.Object) error {
	if config == nil {
		return nil
	}
	return errors.New("configuration not allowed for this profile")
}

// ApplyConfig implements ConfigHandler.
func (NoopConfigHandler) ApplyConfig(config runtime.Object, _ []*resourceapi.DeviceRequestAllocationResult) (PerDeviceCDIContainerEdits, error) {
	if config != nil {
		return nil, errors.New("configuration not allowed for this profile")
	}
	return nil, nil
}
