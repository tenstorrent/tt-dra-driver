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
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	drahealthv1alpha1 "k8s.io/kubelet/pkg/apis/dra-health/v1alpha1"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

// fakeHealthStream stands in for kubelet's end of a NodeWatchResources call.
type fakeHealthStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *drahealthv1alpha1.NodeWatchResourcesResponse
}

func newFakeHealthStream(ctx context.Context) *fakeHealthStream {
	return &fakeHealthStream{ctx: ctx, sent: make(chan *drahealthv1alpha1.NodeWatchResourcesResponse, 16)}
}

func (s *fakeHealthStream) Context() context.Context { return s.ctx }
func (s *fakeHealthStream) Send(r *drahealthv1alpha1.NodeWatchResourcesResponse) error {
	s.sent <- r
	return nil
}

func (s *fakeHealthStream) recv(t *testing.T) *drahealthv1alpha1.NodeWatchResourcesResponse {
	t.Helper()
	select {
	case r := <-s.sent:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a health snapshot")
		return nil
	}
}

// serve runs NodeWatchResources on its own goroutine, returning a function
// that ends the stream and waits for the handler to return.
func serve(t *testing.T, d *driver) (*fakeHealthStream, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeHealthStream(ctx)
	done := make(chan error, 1)
	go func() { done <- d.NodeWatchResources(&drahealthv1alpha1.NodeWatchResourcesRequest{}, stream) }()
	return stream, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("NodeWatchResources returned %v after the stream ended, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("NodeWatchResources did not return after the stream ended")
		}
	}
}

func healthy(names ...string) profiles.DeviceHealthByName {
	h := profiles.DeviceHealthByName{}
	for _, name := range names {
		h[name] = profiles.DeviceHealth{State: profiles.HealthHealthy}
	}
	return h
}

func TestHealthStreamSendsSnapshotThenEveryChange(t *testing.T) {
	d := &driver{health: newHealthHub(testNodeName, healthy("chip-0", "chip-1"))}
	stream, stop := serve(t, d)
	defer stop()

	first := stream.recv(t)
	if len(first.GetDevices()) != 2 {
		t.Fatalf("first snapshot has %d devices, want 2", len(first.GetDevices()))
	}
	for _, dev := range first.GetDevices() {
		if dev.GetHealth() != drahealthv1alpha1.HealthStatus_HEALTHY ||
			dev.GetHealthCheckTimeoutSeconds() != 30 ||
			dev.GetDevice().GetPoolName() != testNodeName {
			t.Errorf("unexpected device health %v", dev)
		}
	}

	update := healthy("chip-0")
	update["chip-1"] = profiles.DeviceHealth{State: profiles.HealthUnhealthy, Message: "gone"}
	if !d.health.set(update) {
		t.Fatal("set reported no change for a different health")
	}
	second := stream.recv(t)
	for _, dev := range second.GetDevices() {
		if dev.GetDevice().GetDeviceName() == "chip-1" &&
			(dev.GetHealth() != drahealthv1alpha1.HealthStatus_UNHEALTHY || dev.GetMessage() != "gone") {
			t.Errorf("chip-1 reported as %v, want UNHEALTHY with its message", dev)
		}
	}

	if d.health.set(update) {
		t.Error("set reported a change for identical health")
	}
}

func TestHealthStreamResendsWithoutChange(t *testing.T) {
	old := healthResendInterval
	healthResendInterval = 10 * time.Millisecond
	defer func() { healthResendInterval = old }()

	d := &driver{health: newHealthHub(testNodeName, healthy("chip-0"))}
	stream, stop := serve(t, d)
	defer stop()

	// Kubelet reverts what it has not heard about to Unknown, so an idle
	// stream must keep talking.
	stream.recv(t)
	stream.recv(t)
	stream.recv(t)
}

// TestHealthStreamsAreIndependent checks that several streams, and a stream
// that reconnects after another has gone, all see the current state.
func TestHealthStreamsAreIndependent(t *testing.T) {
	d := &driver{health: newHealthHub(testNodeName, healthy("chip-0"))}
	a, stopA := serve(t, d)
	b, stopB := serve(t, d)
	defer stopB()
	a.recv(t)
	b.recv(t)

	stopA()
	d.health.set(profiles.DeviceHealthByName{"chip-0": {State: profiles.HealthUnknown}})
	if got := b.recv(t).GetDevices()[0].GetHealth(); got != drahealthv1alpha1.HealthStatus_UNKNOWN {
		t.Errorf("the surviving stream reports %v, want UNKNOWN", got)
	}

	c, stopC := serve(t, d)
	defer stopC()
	if got := c.recv(t).GetDevices()[0].GetHealth(); got != drahealthv1alpha1.HealthStatus_UNKNOWN {
		t.Errorf("a reconnected stream reports %v, want the current UNKNOWN", got)
	}

	d.health.mu.Lock()
	subs := len(d.health.subs)
	d.health.mu.Unlock()
	if subs != 2 {
		t.Errorf("%d subscribers remain, want 2: a stream that ended must unsubscribe", subs)
	}
}

func TestKubeletHealthMapping(t *testing.T) {
	for state, want := range map[profiles.HealthState]drahealthv1alpha1.HealthStatus{
		profiles.HealthHealthy:   drahealthv1alpha1.HealthStatus_HEALTHY,
		profiles.HealthDegraded:  drahealthv1alpha1.HealthStatus_UNHEALTHY,
		profiles.HealthUnhealthy: drahealthv1alpha1.HealthStatus_UNHEALTHY,
		profiles.HealthUnknown:   drahealthv1alpha1.HealthStatus_UNKNOWN,
	} {
		if got := kubeletHealth(state); got != want {
			t.Errorf("%v maps to %v, want %v", state, got, want)
		}
	}
}

func TestHealthMessageIsTruncated(t *testing.T) {
	got := healthMessage(profiles.DeviceHealth{Message: strings.Repeat("x", 2000)})
	if len(got) != maxHealthMessage || !strings.HasSuffix(got, "...") {
		t.Errorf("message has %d bytes, want %d ending in an ellipsis", len(got), maxHealthMessage)
	}
}
