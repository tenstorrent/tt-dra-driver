// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: The Kubernetes Authors
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

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
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	drahealthv1alpha1 "k8s.io/kubelet/pkg/apis/dra-health/v1alpha1"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

const (
	// healthCheckTimeout is what kubelet is told to wait before treating a
	// device as unknown: three resend intervals, so that a single late or
	// dropped send does not flap pod status.
	healthCheckTimeout = 30 * time.Second

	// maxHealthMessage is the longest message kubelet accepts without
	// truncating it.
	maxHealthMessage = 1024
)

// healthResendInterval is how often every open health stream re-sends the
// full snapshot even when nothing changed. Kubelet reverts a device it has
// not heard about within healthCheckTimeout to Unknown, so silence would
// read as lost health; a repeated identical snapshot costs nothing
// downstream, because pod status is only updated on a change.
//
// It is a variable rather than a constant so that tests can exercise the
// timer in milliseconds.
var healthResendInterval = 10 * time.Second

// healthHub holds the latest device health the profile reported and wakes
// every open health stream when it changes. Streams are woken rather than
// sent the value, so a slow stream sees the newest state instead of a
// backlog.
type healthHub struct {
	pool string

	mu      sync.Mutex
	current profiles.DeviceHealthByName
	subs    map[chan struct{}]struct{}
}

func newHealthHub(pool string, initial profiles.DeviceHealthByName) *healthHub {
	return &healthHub{pool: pool, current: initial, subs: make(map[chan struct{}]struct{})}
}

// set replaces the health and reports whether it differed from before.
func (h *healthHub) set(health profiles.DeviceHealthByName) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if maps.EqualFunc(h.current, health, sameHealth) {
		return false
	}
	h.current = health
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // already pending; the stream will read the newest state
		}
	}
	return true
}

func sameHealth(a, b profiles.DeviceHealth) bool {
	return a.State == b.State && a.Reason == b.Reason && a.Transient == b.Transient &&
		a.Since.Equal(b.Since) && a.Message == b.Message
}

// subscribe returns a channel that receives whenever the health changes, and
// a function that ends the subscription.
func (h *healthHub) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// response renders the current health as the complete snapshot kubelet
// expects, devices in name order so that identical states are identical
// messages.
func (h *healthHub) response(now time.Time) *drahealthv1alpha1.NodeWatchResourcesResponse {
	h.mu.Lock()
	defer h.mu.Unlock()

	resp := &drahealthv1alpha1.NodeWatchResourcesResponse{}
	for _, name := range slices.Sorted(maps.Keys(h.current)) {
		dh := h.current[name]
		resp.Devices = append(resp.Devices, &drahealthv1alpha1.DeviceHealth{
			Device:                    &drahealthv1alpha1.DeviceIdentifier{PoolName: h.pool, DeviceName: name},
			Health:                    kubeletHealth(dh.State),
			LastUpdatedTime:           now.Unix(),
			HealthCheckTimeoutSeconds: int64(healthCheckTimeout / time.Second),
			Message:                   healthMessage(dh),
		})
	}
	return resp
}

// kubeletHealth maps the driver's states to kubelet's three. Degraded is
// UNHEALTHY: kubelet has nowhere to say "works, but not well", and a pod
// holding a degraded device should hear about it.
func kubeletHealth(state profiles.HealthState) drahealthv1alpha1.HealthStatus {
	switch state {
	case profiles.HealthHealthy:
		return drahealthv1alpha1.HealthStatus_HEALTHY
	case profiles.HealthDegraded, profiles.HealthUnhealthy:
		return drahealthv1alpha1.HealthStatus_UNHEALTHY
	default:
		return drahealthv1alpha1.HealthStatus_UNKNOWN
	}
}

func healthMessage(dh profiles.DeviceHealth) string {
	msg := dh.Message
	if len(msg) > maxHealthMessage {
		msg = msg[:maxHealthMessage-3] + "..."
	}
	return msg
}

// NodeWatchResources implements drahealthv1alpha1.DRAResourceHealthServer. It
// sends the current snapshot at once, and again on every change and every
// healthResendInterval, until kubelet goes away. Kubelet reconnects on its
// own, so returning on a send error is all the recovery needed.
func (d *driver) NodeWatchResources(_ *drahealthv1alpha1.NodeWatchResourcesRequest, stream grpc.ServerStreamingServer[drahealthv1alpha1.NodeWatchResourcesResponse]) error {
	ctx := stream.Context()
	logger := klog.FromContext(ctx)

	changed, unsubscribe := d.health.subscribe()
	defer unsubscribe()
	ticker := time.NewTicker(healthResendInterval)
	defer ticker.Stop()

	logger.V(2).Info("Health stream opened")
	defer logger.V(2).Info("Health stream closed")
	for {
		if err := stream.Send(d.health.response(time.Now())); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-ticker.C:
		}
	}
}
