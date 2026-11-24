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
	"time"

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

// HealthState is how well a published device is doing.
type HealthState int

const (
	// HealthUnknown means the profile cannot currently tell, e.g. because the
	// source it learns about devices from is unreachable. It is the zero
	// value so that a device nobody has reported on reads as unknown rather
	// than healthy.
	HealthUnknown HealthState = iota
	HealthHealthy
	// HealthDegraded means the device works but not as it should.
	HealthDegraded
	HealthUnhealthy
)

// String implements fmt.Stringer.
func (s HealthState) String() string {
	switch s {
	case HealthHealthy:
		return "Healthy"
	case HealthDegraded:
		return "Degraded"
	case HealthUnhealthy:
		return "Unhealthy"
	default:
		return "Unknown"
	}
}

// DeviceHealth is the health of one published device.
type DeviceHealth struct {
	State HealthState
	// Reason is a short, stable, label-value-safe token for what is wrong,
	// e.g. "not-reported". Empty for a healthy device.
	Reason string
	// Transient says the condition is expected to clear without
	// intervention.
	Transient bool
	// Since is when the condition started, or the zero time if not known.
	Since time.Time
	// Message is a human-readable elaboration of Reason.
	Message string
}

// DeviceHealthByName maps a device name to its health. Names are those of
// the devices in the pool published for the node, plus devices the profile
// published earlier and no longer does, which are the ones a running pod may
// still be holding.
type DeviceHealthByName map[string]DeviceHealth

// DeviceUpdate is the profile's complete view of this node's devices at one
// moment: what to publish, and how each device is doing. Keeping them in one
// value means health is never reported against a device set it was not
// computed from.
type DeviceUpdate struct {
	Resources resourceslice.DriverResources
	Health    DeviceHealthByName
}

// DeviceWatch is a live view of the devices a profile can publish for this
// node.
type DeviceWatch interface {
	// Updates delivers the devices as of the moment the watch started, and
	// then a fresh view every time the underlying devices or their health
	// change. Each value is complete and replaces the previous one. A view
	// whose Resources equal the previous one's carries only a health change.
	//
	// The channel is closed when the watch stops: either the context passed
	// to WatchDevices was cancelled, or the watch failed in a way retrying
	// cannot fix. Err says which.
	Updates() <-chan DeviceUpdate

	// Err returns the error that stopped the watch, or nil if it stopped
	// because its context was cancelled. It must only be read after Updates
	// has been observed closed.
	Err() error
}

// Profile describes a kind of device that can be managed by the driver.
type Profile interface {
	ConfigHandler
	// WatchDevices watches the profile's device source and reports the
	// resource slices to publish into the cluster for the node the driver is
	// running on. It returns immediately; the first set of resources arrives
	// on the watch's channel once the source has something to report, which
	// for a remote source (e.g. a per-node device-discovery agent) may take
	// as long as that source needs to come up. Implementations run until ctx
	// is cancelled and are responsible for riding out transient failures of
	// their source.
	WatchDevices(ctx context.Context) DeviceWatch
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
