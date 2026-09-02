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
	"k8s.io/klog/v2"
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

// NewDriver wires up the kubelet plugin: it builds the device state, starts
// the kubeletplugin helper, optionally starts the healthcheck server and
// publishes the initial set of resource slices.
func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	logger := klog.FromContext(ctx)
	startupStart := time.Now()

	d := &driver{
		client:    config.coreclient,
		cancelCtx: config.cancelMainCtx,
	}

	stateStart := time.Now()
	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}
	d.state = state
	logger.Info("Built the device state", "duration", time.Since(stateStart))

	helperStart := time.Now()
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
	logger.Info("Started the kubelet plugin helper", "duration", time.Since(helperStart))

	d.healthcheck, err = startHealthcheck(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}

	// Until this returns, the scheduler cannot allocate any device on this
	// node, so its duration is part of every pod's time-to-running after a
	// driver (re)start.
	publishStart := time.Now()
	if err := helper.PublishResources(ctx, state.driverResources); err != nil {
		logger.Error(err, "Failed to publish resource slices", "duration", time.Since(publishStart))
		return nil, err
	}
	logger.Info("Published resource slices",
		"numDevices", countDevices(state.driverResources),
		"duration", time.Since(publishStart),
	)

	logger.Info("Driver is ready", "startupDuration", time.Since(startupStart))
	return d, nil
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

	// The claims in a batch are prepared one after another, and each one
	// serializes on the DeviceState lock, so the batch total is worth
	// separating from the per-claim durations below.
	start := time.Now()
	result := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		result[claim.UID] = d.prepareResourceClaim(ctx, claim)
	}

	logger.Info("PrepareResourceClaims finished", "numClaims", len(claims), "duration", time.Since(start))
	return result, nil
}

func (d *driver) prepareResourceClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	logger := klog.FromContext(ctx)
	logger.Info("Preparing claim", "uid", claim.UID, "namespace", claim.Namespace, "name", claim.Name)

	start := time.Now()
	preparedPBs, err := d.state.Prepare(ctx, claim)
	if err != nil {
		// The kubelet retries a failed NodePrepareResources with backoff, so
		// a claim that keeps landing here reads as a pod stuck starting.
		logger.Error(err, "Error preparing devices for claim", "uid", claim.UID, "duration", time.Since(start))
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

	logger.Info("Returning newly prepared devices for claim", "uid", claim.UID, "devices", prepared, "duration", time.Since(start))
	return kubeletplugin.PrepareResult{Devices: prepared}
}

// UnprepareResourceClaims is invoked by the kubelet to release devices when a
// claim is no longer in use.
func (d *driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	logger := klog.FromContext(ctx)
	logger.Info("UnprepareResourceClaims is called", "numClaims", len(claims))

	start := time.Now()
	result := make(map[types.UID]error)

	for _, claim := range claims {
		result[claim.UID] = d.unprepareResourceClaim(ctx, claim)
	}

	logger.Info("UnprepareResourceClaims finished", "numClaims", len(claims), "duration", time.Since(start))
	return result, nil
}

func (d *driver) unprepareResourceClaim(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	logger := klog.FromContext(ctx)

	start := time.Now()
	if err := d.state.Unprepare(ctx, string(claim.UID)); err != nil {
		logger.Error(err, "Error unpreparing devices for claim", "uid", claim.UID, "duration", time.Since(start))
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
