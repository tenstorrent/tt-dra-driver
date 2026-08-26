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
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/tenstorrent/tt-dra-driver/internal/fabricmanager"
	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

// fakeProfile returns a scripted sequence of EnumerateDevices results,
// repeating the last one once the script is exhausted.
type fakeProfile struct {
	profiles.NoopConfigHandler

	errs  []error
	calls int
}

func (p *fakeProfile) EnumerateDevices(context.Context) (resourceslice.DriverResources, error) {
	p.calls++
	err := p.errs[min(p.calls, len(p.errs))-1]
	if err != nil {
		return resourceslice.DriverResources{}, err
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{"node": {}},
	}, nil
}

func (p *fakeProfile) CommonContainerEdits() *cdiapi.ContainerEdits { return nil }

// withFastBackoff shrinks the retry budget so the tests exercise the retry
// logic without sleeping for minutes.
func withFastBackoff(t *testing.T, steps int) {
	t.Helper()
	original := enumerateBackoff
	enumerateBackoff = wait.Backoff{
		Duration: time.Millisecond,
		Factor:   1.0,
		Steps:    steps,
	}
	t.Cleanup(func() { enumerateBackoff = original })
}

func TestEnumerateDevicesWithRetry(t *testing.T) {
	// The wrapping mirrors what the tenstorrent profile does to errors from
	// the agent client, so the sentinels have to survive %w to be seen here.
	notReady := fmt.Errorf("tenstorrent profile: get topology: %w", fabricmanager.ErrTopologyNotReady)
	unavailable := fmt.Errorf("tenstorrent profile: get topology: %w", fabricmanager.ErrAgentUnavailable)
	permanent := errors.New("tenstorrent profile: malformed topology")

	for _, tc := range []struct {
		name      string
		errs      []error
		wantErr   error
		wantCalls int
	}{
		{
			name:      "succeeds on first attempt",
			errs:      []error{nil},
			wantCalls: 1,
		},
		{
			name:      "waits out topology discovery",
			errs:      []error{notReady, notReady, nil},
			wantCalls: 3,
		},
		{
			name:      "waits out an agent that is not answering",
			errs:      []error{unavailable, unavailable, nil},
			wantCalls: 3,
		},
		{
			name:      "waits out a mix of transient failures",
			errs:      []error{unavailable, notReady, nil},
			wantCalls: 3,
		},
		{
			name:      "gives up on a permanent error without retrying",
			errs:      []error{permanent},
			wantErr:   permanent,
			wantCalls: 1,
		},
		{
			name:      "surfaces the transient error once the budget is exhausted",
			errs:      []error{unavailable},
			wantErr:   fabricmanager.ErrAgentUnavailable,
			wantCalls: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastBackoff(t, tc.wantCalls)
			profile := &fakeProfile{errs: tc.errs}

			resources, err := enumerateDevicesWithRetry(context.Background(), profile)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("enumerateDevicesWithRetry: unexpected error: %v", err)
				}
				if _, ok := resources.Pools["node"]; !ok {
					t.Errorf("enumerateDevicesWithRetry returned %v, want the profile's pools", resources.Pools)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("enumerateDevicesWithRetry returned %v, want %v", err, tc.wantErr)
			}

			if profile.calls != tc.wantCalls {
				t.Errorf("EnumerateDevices was called %d times, want %d", profile.calls, tc.wantCalls)
			}
		})
	}
}
