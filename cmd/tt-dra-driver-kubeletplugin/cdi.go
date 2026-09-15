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
	"fmt"
	"os"
	"regexp"
	"strings"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

const cdiCommonDeviceName = "common"

var nonWord = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// CDIHandler manages the on-disk CDI specs published by the driver.
type CDIHandler struct {
	cache       *cdiapi.Cache
	driverName  string
	class       string
	commonEdits *cdiapi.ContainerEdits
}

// NewCDIHandler returns a CDIHandler that writes its specs into the given
// root directory. The optional commonEdits, when non-nil, are merged into
// the per-driver "common" CDI device that the kubelet plugin prepends to
// every claim; a typical use is to declare node-wide bind mounts (e.g.
// hugepages) that every workload using a managed device requires.
func NewCDIHandler(root, driverName, class string, commonEdits *cdiapi.ContainerEdits) (*CDIHandler, error) {
	cache, err := cdiapi.NewCache(
		cdiapi.WithSpecDirs(root),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create a new CDI cache: %w", err)
	}
	return &CDIHandler{
		cache:       cache,
		driverName:  driverName,
		class:       class,
		commonEdits: commonEdits,
	}, nil
}

// CreateCommonSpecFile writes the per-driver "common" CDI spec that injects
// node-wide environment variables and any profile-supplied container edits
// (e.g. hugepage mounts) into every container that consumes a device.
func (cdi *CDIHandler) CreateCommonSpecFile() error {
	commonEdits := cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: []string{
				fmt.Sprintf("KUBERNETES_NODE_NAME=%s", os.Getenv("NODE_NAME")),
				fmt.Sprintf("DRA_RESOURCE_DRIVER_NAME=%s", cdi.driverName),
			},
		},
	}
	commonEdits.Append(cdi.commonEdits)

	spec := &cdispec.Spec{
		Kind: cdi.kind(),
		Devices: []cdispec.Device{
			{
				Name:           cdiCommonDeviceName,
				ContainerEdits: *commonEdits.ContainerEdits,
			},
		},
	}

	minVersion, err := cdiapi.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("failed to get minimum required CDI spec version: %v", err)
	}
	spec.Version = minVersion

	specName, err := cdiapi.GenerateNameForTransientSpec(spec, cdiCommonDeviceName)
	if err != nil {
		return fmt.Errorf("failed to generate Spec name: %w", err)
	}

	return cdi.cache.WriteSpec(spec, specName)
}

// CreateClaimSpecFile writes the per-claim CDI spec that the kubelet will
// reference when injecting devices into containers.
func (cdi *CDIHandler) CreateClaimSpecFile(claimUID string, devices profiles.PreparedDevices) error {
	specName := cdiapi.GenerateTransientSpecName(cdi.vendor(), cdi.class, claimUID)

	spec := &cdispec.Spec{
		Kind:    cdi.kind(),
		Devices: []cdispec.Device{},
	}

	for _, device := range devices {
		deviceEnvKey := strings.ToUpper(nonWord.ReplaceAllString(device.DeviceName, "_"))
		claimEdits := cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{
				Env: []string{
					fmt.Sprintf("%s_DEVICE_%s_RESOURCE_CLAIM=%s", strings.ToUpper(cdi.class), deviceEnvKey, claimUID),
					fmt.Sprintf("DRA_ADMIN_ACCESS=%t", device.AdminAccess),
				},
			},
		}

		// Future: when this device has admin access, inject host hardware
		// information here (e.g. firmware versions, telemetry sockets).

		claimEdits.Append(device.ContainerEdits)

		spec.Devices = append(spec.Devices, cdispec.Device{
			Name:           fmt.Sprintf("%s-%s", claimUID, device.DeviceName),
			ContainerEdits: *claimEdits.ContainerEdits,
		})
	}

	minVersion, err := cdiapi.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("failed to get minimum required CDI spec version: %v", err)
	}
	spec.Version = minVersion

	return cdi.cache.WriteSpec(spec, specName)
}

// DeleteClaimSpecFile removes the per-claim CDI spec from disk.
func (cdi *CDIHandler) DeleteClaimSpecFile(claimUID string) error {
	specName := cdiapi.GenerateTransientSpecName(cdi.vendor(), cdi.class, claimUID)
	return cdi.cache.RemoveSpec(specName)
}

// GetClaimDevices returns the qualified CDI device IDs that the kubelet
// should pass into the OCI runtime for the given claim and device list.
func (cdi *CDIHandler) GetClaimDevices(claimUID string, devices []string) []string {
	cdiDevices := []string{
		cdiparser.QualifiedName(cdi.vendor(), cdi.class, cdiCommonDeviceName),
	}
	for _, device := range devices {
		cdiDevice := cdiparser.QualifiedName(cdi.vendor(), cdi.class, fmt.Sprintf("%s-%s", claimUID, device))
		cdiDevices = append(cdiDevices, cdiDevice)
	}
	return cdiDevices
}

func (cdi *CDIHandler) kind() string {
	return cdi.vendor() + "/" + cdi.class
}

func (cdi *CDIHandler) vendor() string {
	return "k8s." + cdi.driverName
}
