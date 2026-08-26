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
	"errors"
	"fmt"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

// driver is the kubelet plugin handler that satisfies the
// kubeletplugin.DRAPlugin interface.
type driver struct {
	client      coreclientset.Interface
	helper      *kubeletplugin.Helper
	state       *DeviceState
	healthcheck *healthcheck
	cancelCtx   func(error)
}

// initialDevicesTimeout bounds how long NewDriver waits for the profile's
// device watch to report the devices on this node. The wait covers the
// fabric manager agent coming up and completing its first topology
// discovery, which is why it is generous; when it expires the driver fails
// its kubelet probe and gets restarted rather than coming up advertising no
// devices at all.
const initialDevicesTimeout = 5 * time.Minute

// NewDriver wires up the kubelet plugin: it waits for the profile's first
// report of the node's devices, builds the device state, starts the
// kubeletplugin helper, optionally starts the healthcheck server, publishes
// the resource slices and then keeps them in sync with the profile for as
// long as ctx lives.
func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	d := &driver{
		client:    config.coreclient,
		cancelCtx: config.cancelMainCtx,
	}

	watch := config.profile.WatchDevices(ctx)
	driverResources, err := waitForInitialDevices(ctx, watch, initialDevicesTimeout)
	if err != nil {
		return nil, err
	}

	state, err := NewDeviceState(config, driverResources)
	if err != nil {
		return nil, err
	}
	d.state = state

	helper, err := kubeletplugin.Start(ctx, d,
		kubeletplugin.KubeClient(config.coreclient),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(config.flags.driverName),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
		kubeletplugin.RollingUpdate(types.UID(config.flags.podUID)),
	)
	if err != nil {
		return nil, err
	}
	d.helper = helper

	d.healthcheck, err = startHealthcheck(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}

	if err := helper.PublishResources(ctx, state.Resources()); err != nil {
		return nil, err
	}

	go d.republishDevices(ctx, watch)

	return d, nil
}

// waitForInitialDevices blocks until the watch reports the node's devices
// for the first time. A watch rides out transient failures of its source on
// its own, so the only reasons this returns an error are the watch failing
// permanently, the caller going away, or the source taking longer than
// timeout to produce anything.
func waitForInitialDevices(ctx context.Context, watch profiles.DeviceWatch, timeout time.Duration) (resourceslice.DriverResources, error) {
	logger := klog.FromContext(ctx)
	logger.Info("Waiting for the initial set of devices", "timeout", timeout)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	select {
	case driverResources, ok := <-watch.Updates():
		if !ok {
			if err := watch.Err(); err != nil {
				return resourceslice.DriverResources{}, fmt.Errorf("watch devices: %w", err)
			}
			return resourceslice.DriverResources{}, errors.New("device watch stopped before reporting any devices")
		}
		return driverResources, nil
	case <-deadline.C:
		return resourceslice.DriverResources{}, fmt.Errorf("no devices reported within %s", timeout)
	case <-ctx.Done():
		return resourceslice.DriverResources{}, ctx.Err()
	}
}

// republishDevices keeps the published ResourceSlices in step with the
// devices the profile reports, for as long as the watch runs. It exits when
// ctx is cancelled; if the watch instead stops on its own the driver can no
// longer learn about topology changes, which is treated as fatal so that the
// kubelet restarts the plugin with a fresh watch.
func (d *driver) republishDevices(ctx context.Context, watch profiles.DeviceWatch) {
	logger := klog.FromContext(ctx)

	for driverResources := range watch.Updates() {
		if !d.state.SetResources(driverResources) {
			logger.V(4).Info("Devices reported by the profile are unchanged; not republishing")
			continue
		}
		logger.Info("Devices changed; republishing resource slices",
			"numDevices", d.state.AllocatableCount(),
		)
		if err := d.helper.PublishResources(ctx, driverResources); err != nil {
			// PublishResources only fails on misconfiguration, which cannot
			// resolve itself, so let HandleError decide the driver's fate.
			d.HandleError(ctx, err, "Failed to republish resource slices")
			return
		}
	}

	if ctx.Err() != nil {
		return
	}
	err := watch.Err()
	if err == nil {
		err = errors.New("device watch stopped unexpectedly")
	}
	d.HandleError(ctx, err, "Device watch stopped; the driver can no longer track topology changes")
}

// Shutdown stops the kubelet plugin helper and the healthcheck server.
func (d *driver) Shutdown(logger klog.Logger) error {
	if d.healthcheck != nil {
		d.healthcheck.Stop(logger)
	}
	d.helper.Stop()
	return nil
}

// PrepareResourceClaims is invoked by the kubelet for each batch of claims
// destined for this driver.
func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	logger := klog.FromContext(ctx)
	logger.Info("PrepareResourceClaims is called", "numClaims", len(claims))
	result := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		result[claim.UID] = d.prepareResourceClaim(ctx, claim)
	}

	return result, nil
}

func (d *driver) prepareResourceClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	logger := klog.FromContext(ctx)
	logger.Info("Preparing claim", "uid", claim.UID, "namespace", claim.Namespace, "name", claim.Name)

	preparedPBs, err := d.state.Prepare(claim)
	if err != nil {
		logger.Error(err, "Error preparing devices for claim", "uid", claim.UID)
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
		}
	}

	prepared := make([]kubeletplugin.Device, 0, len(preparedPBs))
	for _, preparedPB := range preparedPBs {
		prepared = append(prepared, kubeletplugin.Device{
			Requests:     preparedPB.GetRequestNames(),
			PoolName:     preparedPB.GetPoolName(),
			DeviceName:   preparedPB.GetDeviceName(),
			CDIDeviceIDs: preparedPB.GetCdiDeviceIds(),
		})
	}

	logger.Info("Returning newly prepared devices for claim", "uid", claim.UID, "devices", prepared)
	return kubeletplugin.PrepareResult{Devices: prepared}
}

// UnprepareResourceClaims is invoked by the kubelet to release devices when a
// claim is no longer in use.
func (d *driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	logger := klog.FromContext(ctx)
	logger.Info("UnprepareResourceClaims is called", "numClaims", len(claims))
	result := make(map[types.UID]error)

	for _, claim := range claims {
		result[claim.UID] = d.unprepareResourceClaim(ctx, claim)
	}

	return result, nil
}

func (d *driver) unprepareResourceClaim(_ context.Context, claim kubeletplugin.NamespacedObject) error {
	if err := d.state.Unprepare(string(claim.UID)); err != nil {
		return fmt.Errorf("error unpreparing devices for claim %v: %w", claim.UID, err)
	}
	return nil
}

// HandleError is called by the kubeletplugin helper to surface background
// errors. Non-recoverable errors trigger driver shutdown.
func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) && d.cancelCtx != nil {
		d.cancelCtx(fmt.Errorf("fatal background error: %w", err))
	}
}
