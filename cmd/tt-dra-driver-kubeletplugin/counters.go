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
	"fmt"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// sharedCountersSupported reports whether this cluster keeps
// ResourceSlice shared counters, i.e. whether the DRAPartitionableDevices
// feature gate is enabled on the apiserver.
//
// The driver needs to know because shared counters are what keep a tray
// device and its chip devices mutually exclusive. When the gate is off the
// apiserver does not reject a slice that uses them — it silently strips
// `spec.sharedCounters` and `spec.devices[].consumesCounters` — so the
// driver would publish trays and chips that look independently
// allocatable and the scheduler could hand the same silicon to two
// workloads at once. Detecting it up front lets the driver fall back to
// publishing chips only.
//
// The probe is a dry-run create of a counters-only ResourceSlice: nothing
// is persisted, and the answer is simply whether the counters survived the
// round trip.
func sharedCountersSupported(ctx context.Context, client coreclientset.Interface, driverName, nodeName string) (bool, error) {
	logger := klog.FromContext(ctx)
	start := time.Now()

	probe := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			// GenerateName keeps the dry run from ever colliding with an
			// existing object, including a concurrent probe from another
			// node's driver pod.
			GenerateName: "tt-dra-driver-counter-probe-",
		},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   driverName,
			NodeName: ptr.To(nodeName),
			Pool: resourceapi.ResourcePool{
				Name:               nodeName,
				Generation:         1,
				ResourceSliceCount: 1,
			},
			SharedCounters: []resourceapi.CounterSet{
				{
					Name: "probe",
					Counters: map[string]resourceapi.Counter{
						"probe": {Value: *resource.NewQuantity(1, resource.DecimalSI)},
					},
				},
			},
		},
	}

	created, err := client.ResourceV1().ResourceSlices().Create(ctx, probe, metav1.CreateOptions{
		DryRun: []string{metav1.DryRunAll},
	})
	if err != nil {
		return false, fmt.Errorf("dry-run create of a ResourceSlice with shared counters (after %s): %w",
			time.Since(start).Round(time.Millisecond), err)
	}

	supported := len(created.Spec.SharedCounters) > 0
	logger.V(2).Info("Probed apiserver support for ResourceSlice shared counters",
		"supported", supported,
		"duration", time.Since(start),
	)
	return supported, nil
}

// resolveTrayDevices decides whether this driver instance publishes
// whole-tray devices. Trays are opt-out via --enable-tray-devices, but
// even when asked for they are only safe on a cluster that keeps shared
// counters, so the answer is confirmed against the apiserver first.
//
// A cluster that cannot keep counters — or a probe that fails to give a
// clear answer — downgrades to chips only rather than failing startup:
// chip allocation keeps working, which is both the safe and the less
// disruptive outcome. The reason is logged as an error so the operator can
// act on it.
func resolveTrayDevices(ctx context.Context, client coreclientset.Interface, flags *Flags) bool {
	logger := klog.FromContext(ctx)
	if !flags.enableTrayDevices {
		logger.Info("Whole-tray devices are disabled; publishing per-chip devices only")
		return false
	}

	supported, err := sharedCountersSupported(ctx, client, flags.driverName, flags.nodeName)
	switch {
	case err != nil:
		logger.Error(err, "Could not determine whether this cluster supports ResourceSlice shared counters; publishing per-chip devices only. "+
			"Tray devices need the DRAPartitionableDevices feature gate to stay mutually exclusive with the chips they contain")
		return false
	case !supported:
		logger.Error(nil, "This cluster drops ResourceSlice shared counters, so a tray and its chips could be allocated at the same time; publishing per-chip devices only. "+
			"Enable the DRAPartitionableDevices feature gate (default on since Kubernetes 1.36) on the apiserver and the scheduler, or set --enable-tray-devices=false to silence this")
		return false
	}

	logger.Info("Publishing whole-tray devices alongside per-chip devices")
	return true
}
