/*
 * Copyright The Kubernetes Authors
 * Modifications Copyright 2026 Tenstorrent USA, Inc.
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
	"context"
	"fmt"
	"slices"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

// AllocatableDevices is a map of device name to its full descriptor for
// quick lookup during Prepare.
type AllocatableDevices map[string]resourceapi.Device

// PreparedClaims is keyed by claim UID and stores the devices currently
// prepared for that claim.
type PreparedClaims map[string]profiles.PreparedDevices

// OpaqueDeviceConfig is a parsed opaque configuration extracted from a
// ResourceClaim's allocation results.
type OpaqueDeviceConfig struct {
	Requests []string
	Config   runtime.Object
}

// DeviceState owns the in-memory and on-disk state of devices managed by the
// driver.
type DeviceState struct {
	sync.Mutex

	driverName        string
	cdi               *CDIHandler
	driverResources   resourceslice.DriverResources
	allocatable       AllocatableDevices
	checkpointManager checkpointmanager.CheckpointManager
	configDecoder     runtime.Decoder
	configHandler     profiles.ConfigHandler
}

// NewDeviceState constructs the DeviceState by enumerating devices from the
// active profile, initializing the CDI handler and loading any previously
// persisted checkpoint.
func NewDeviceState(ctx context.Context, config *Config) (*DeviceState, error) {
	driverResources, err := config.profile.EnumerateDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %v", err)
	}

	cdi, err := NewCDIHandler(config.flags.cdiRoot, config.flags.driverName, config.flags.profile, config.profile.CommonContainerEdits())
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %v", err)
	}

	if err := cdi.CreateCommonSpecFile(); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for common edits: %v", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	configScheme := runtime.NewScheme()
	configHandler := config.profile
	sb := configHandler.SchemeBuilder()
	if err := sb.AddToScheme(configScheme); err != nil {
		return nil, fmt.Errorf("create config scheme: %w", err)
	}

	decoder := json.NewSerializerWithOptions(
		json.DefaultMetaFactory,
		configScheme,
		configScheme,
		json.SerializerOptions{
			Pretty: true, Strict: true,
		},
	)

	allocatable := make(AllocatableDevices)
	for _, slice := range driverResources.Pools[config.flags.nodeName].Slices {
		for _, device := range slice.Devices {
			allocatable[device.Name] = device
		}
	}

	state := &DeviceState{
		driverName:        config.flags.driverName,
		cdi:               cdi,
		driverResources:   driverResources,
		allocatable:       allocatable,
		checkpointManager: checkpointManager,
		configDecoder:     decoder,
		configHandler:     configHandler,
	}

	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}
	for _, c := range checkpoints {
		if c == DriverPluginCheckpointFile {
			return state, nil
		}
	}

	checkpoint := newCheckpoint()
	if err := state.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return state, nil
}

// Prepare reserves devices for a claim, materializes the per-claim CDI spec
// file and persists the result to the on-disk checkpoint.
func (s *DeviceState) Prepare(claim *resourceapi.ResourceClaim) ([]*drapbv1.Device, error) {
	s.Lock()
	defer s.Unlock()

	claimUID := string(claim.UID)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] != nil {
		return preparedClaims[claimUID].GetDevices(), nil
	}

	preparedDevices, err := s.prepareDevices(claim)
	if err != nil {
		return nil, fmt.Errorf("prepare failed: %v", err)
	}

	if err := s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %v", err)
	}

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

// Unprepare releases devices for a claim, deletes the per-claim CDI spec file
// and persists the new state to the on-disk checkpoint.
func (s *DeviceState) Unprepare(claimUID string) error {
	s.Lock()
	defer s.Unlock()

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		checkpoint = newCheckpoint()
		if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
			return fmt.Errorf("unable to create new checkpoint: %v", err)
		}
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] == nil {
		return nil
	}

	if err := s.unprepareDevices(claimUID, preparedClaims[claimUID]); err != nil {
		return fmt.Errorf("unprepare failed: %v", err)
	}

	if err := s.cdi.DeleteClaimSpecFile(claimUID); err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %v", err)
	}

	delete(preparedClaims, claimUID)
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return nil
}

func (s *DeviceState) prepareDevices(claim *resourceapi.ResourceClaim) (profiles.PreparedDevices, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	hasAdminAccess := s.checkAdminAccess(claim)

	configs, err := GetOpaqueDeviceConfigs(
		s.configDecoder,
		s.driverName,
		claim.Status.Allocation.Devices.Config,
	)
	if err != nil {
		return nil, fmt.Errorf("error getting opaque device configs: %v", err)
	}

	// Insert an empty config at the front with the lowest precedence to
	// guarantee at least one config exists in the lookup below.
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{})

	configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)
	for _, result := range claim.Status.Allocation.Devices.Results {
		// The claim may include allocations meant for other drivers.
		if result.Driver != s.driverName {
			continue
		}
		if _, exists := s.allocatable[result.Device]; !exists {
			return nil, fmt.Errorf("requested device is not allocatable: %v", result.Device)
		}
		for _, c := range slices.Backward(configs) {
			if len(c.Requests) == 0 || slices.Contains(c.Requests, result.Request) {
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
		}
	}

	// Apply each config to its associated set of allocation results and
	// merge the produced container edits into a single per-device map.
	perDeviceCDIContainerEdits := make(profiles.PerDeviceCDIContainerEdits)
	for config, results := range configResultsMap {
		containerEdits, err := s.configHandler.ApplyConfig(config, results)
		if err != nil {
			return nil, fmt.Errorf("error applying config: %w", err)
		}
		for k, v := range containerEdits {
			perDeviceCDIContainerEdits[k] = v
		}
	}

	var preparedDevices profiles.PreparedDevices
	for _, results := range configResultsMap {
		for _, result := range results {
			device := &profiles.PreparedDevice{
				Device: drapbv1.Device{
					RequestNames: []string{result.Request},
					PoolName:     result.Pool,
					DeviceName:   result.Device,
					CdiDeviceIds: s.cdi.GetClaimDevices(string(claim.UID), []string{result.Device}),
				},
				ContainerEdits: perDeviceCDIContainerEdits[result.Device],
				AdminAccess:    hasAdminAccess,
			}
			preparedDevices = append(preparedDevices, device)
		}
	}

	return preparedDevices, nil
}

func (s *DeviceState) unprepareDevices(_ string, _ profiles.PreparedDevices) error {
	// No-op for now: simulated devices have no per-device cleanup.
	return nil
}

// checkAdminAccess determines whether the claim was granted admin access to
// any of the requested devices.
func (s *DeviceState) checkAdminAccess(claim *resourceapi.ResourceClaim) bool {
	if claim != nil && claim.Status.Allocation != nil {
		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.AdminAccess != nil && *result.AdminAccess {
				return true
			}
		}
	}
	return false
}

// GetOpaqueDeviceConfigs returns an ordered list of the configs contained in
// possibleConfigs that target this driver.
//
// Configs can either come from the resource claim itself or from the device
// class associated with the request. Configs coming directly from the
// resource claim take precedence over configs coming from the device class.
// Within the same source, configs found later take precedence over earlier
// ones. The returned list is in order of increasing precedence.
func GetOpaqueDeviceConfigs(
	decoder runtime.Decoder,
	driverName string,
	possibleConfigs []resourceapi.DeviceAllocationConfiguration,
) ([]*OpaqueDeviceConfig, error) {
	var classConfigs []resourceapi.DeviceAllocationConfiguration
	var claimConfigs []resourceapi.DeviceAllocationConfiguration
	for _, config := range possibleConfigs {
		switch config.Source {
		case resourceapi.AllocationConfigSourceClass:
			classConfigs = append(classConfigs, config)
		case resourceapi.AllocationConfigSourceClaim:
			claimConfigs = append(claimConfigs, config)
		default:
			return nil, fmt.Errorf("invalid config source: %v", config.Source)
		}
	}
	candidateConfigs := append(classConfigs, claimConfigs...)

	var resultConfigs []*OpaqueDeviceConfig
	for _, config := range candidateConfigs {
		if config.Opaque == nil {
			return nil, fmt.Errorf("only opaque parameters are supported by this driver")
		}
		if config.Opaque.Driver != driverName {
			continue
		}

		decodedConfig, err := runtime.Decode(decoder, config.Opaque.Parameters.Raw)
		if err != nil {
			return nil, fmt.Errorf("error decoding config parameters: %w", err)
		}

		resultConfigs = append(resultConfigs, &OpaqueDeviceConfig{
			Requests: config.Requests,
			Config:   decodedConfig,
		})
	}

	return resultConfigs, nil
}
