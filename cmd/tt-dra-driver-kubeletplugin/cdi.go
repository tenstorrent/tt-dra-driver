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
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
)

const cdiCommonDeviceName = "common"

var nonWord = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// cdiSpecExtensions are the file extensions a CDI spec can be written with.
// The driver only ever writes the default (YAML), but a spec left behind by
// an older version of the driver could carry either.
var cdiSpecExtensions = []string{".yaml", ".json"}

// CDIHandler manages the on-disk CDI specs published by the driver.
type CDIHandler struct {
	cache       *cdiapi.Cache
	root        string
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
		root:        root,
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

	return cdi.cache.WriteSpec(spec, cdi.claimSpecName(cdiCommonDeviceName))
}

// CreateClaimSpecFile writes the per-claim CDI spec that the kubelet will
// reference when injecting devices into containers. The spec is derived
// entirely from its arguments, so calling it again with the same claim and
// devices rewrites the same content; the driver relies on that to restore a
// spec file that was lost with the CDI root (see DeviceState.Prepare).
func (cdi *CDIHandler) CreateClaimSpecFile(claimUID string, devices profiles.PreparedDevices) error {
	specName := cdi.claimSpecName(claimUID)

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

// DeleteClaimSpecFile removes the per-claim CDI spec from disk. A spec that
// is already gone is not an error.
func (cdi *CDIHandler) DeleteClaimSpecFile(claimUID string) error {
	return cdi.cache.RemoveSpec(cdi.claimSpecName(claimUID))
}

// PruneClaimSpecFiles removes every per-claim CDI spec in the CDI root that
// does not belong to one of the given claim UIDs, and returns the names of the
// files it removed. The driver's own common spec and any spec belonging to
// another vendor or class are left alone.
//
// Prepare writes the CDI spec before it checkpoints the claim, so a driver
// that dies between the two leaves a spec file no claim accounts for. Those
// files are inert but they accumulate, and a stale one can outlive the device
// it names.
func (cdi *CDIHandler) PruneClaimSpecFiles(claimUIDs []string) ([]string, error) {
	prefix := cdiapi.GenerateSpecName(cdi.vendor(), cdi.class) + "_"

	keep := map[string]struct{}{
		cdi.claimSpecName(cdiCommonDeviceName): {},
	}
	for _, claimUID := range claimUIDs {
		keep[cdi.claimSpecName(claimUID)] = struct{}{}
	}

	entries, err := os.ReadDir(cdi.root)
	if err != nil {
		return nil, fmt.Errorf("unable to read the CDI root %q: %w", cdi.root, err)
	}

	var removed []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := filepath.Ext(name)
		if !slices.Contains(cdiSpecExtensions, ext) {
			continue
		}
		specName := strings.TrimSuffix(name, ext)
		if !strings.HasPrefix(specName, prefix) {
			continue
		}
		if ext == ".yaml" {
			if _, ok := keep[specName]; ok {
				continue
			}
		}
		if err := cdi.cache.RemoveSpec(name); err != nil {
			return removed, fmt.Errorf("unable to remove the orphaned CDI spec %q: %w", name, err)
		}
		removed = append(removed, name)
	}

	return removed, nil
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

// claimSpecName returns the extensionless name of the CDI spec file that
// holds the devices of the given claim.
func (cdi *CDIHandler) claimSpecName(claimUID string) string {
	return cdiapi.GenerateTransientSpecName(cdi.vendor(), cdi.class, claimUID)
}

func (cdi *CDIHandler) kind() string {
	return cdi.vendor() + "/" + cdi.class
}

func (cdi *CDIHandler) vendor() string {
	return "k8s." + cdi.driverName
}
