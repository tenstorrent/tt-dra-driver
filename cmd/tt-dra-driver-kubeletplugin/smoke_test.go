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

// End-to-end smoke tests for the kubelet plugin. Each one starts a real driver
// against a fake fabric manager agent and drives it over the same two gRPC
// sockets the kubelet uses. See harness_test.go for how the surroundings are
// substituted, and fakeagent_test.go for the hardware fixtures.

package main

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	resourceapi "k8s.io/api/resource/v1"

	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

// TestSmokeRegistersAndPreparesAnN300 is the headline path: an n300 is
// discovered, published, claimed, prepared and released, all through the
// interfaces the kubelet uses.
func TestSmokeRegistersAndPreparesAnN300(t *testing.T) {
	h := startHarness(t, withTopology(n300Topology()))

	// The kubelet finds the plugin by its registration socket and asks who it
	// is before it will route any DRA call to it.
	info := h.getInfo()
	if got := info.GetName(); got != testDriverName {
		t.Errorf("registered name is %q, want %q", got, testDriverName)
	}
	if got, want := info.GetEndpoint(), h.draSocketPath(); got != want {
		t.Errorf("advertised endpoint is %q, want %q", got, want)
	}
	if !slices.Contains(info.GetSupportedVersions(), "v1.DRAPlugin") {
		t.Errorf("v1.DRAPlugin is not among the supported versions %v", info.GetSupportedVersions())
	}

	// The n300's remote sibling has no device node of its own, so it is
	// bundled into its MMIO parent and the host advertises one device.
	devices := h.waitForPublishedDevices("tt-0")
	if got := attrInt(t, devices["tt-0"], "chipCount"); got != 2 {
		t.Errorf("chipCount is %d, want 2 for an n300", got)
	}

	claim := h.allocate("claim-uid", "tt-0")
	prepared := h.mustPrepare(claim)
	if len(prepared) != 1 {
		t.Fatalf("prepared %d devices, want 1", len(prepared))
	}
	if got := prepared[0].GetDeviceName(); got != "tt-0" {
		t.Errorf("prepared device is %q, want tt-0", got)
	}
	if got, want := prepared[0].GetPoolName(), testNodeName; got != want {
		t.Errorf("prepared pool is %q, want %q", got, want)
	}

	// The kubelet passes these names to the runtime verbatim, so they are the
	// driver's contract with containerd and worth pinning literally. The
	// common device carries the node-wide hugepage mounts; the per-claim one
	// carries the character device.
	wantCDI := []string{
		"k8s.tenstorrent.com/tenstorrent=common",
		"k8s.tenstorrent.com/tenstorrent=claim-uid-tt-0",
	}
	if got := prepared[0].GetCdiDeviceIds(); !slices.Equal(got, wantCDI) {
		t.Errorf("CDI device IDs are %v, want %v", got, wantCDI)
	}

	// Those names have to resolve to something on disk, or the runtime fails
	// the container with no useful reason.
	assertExists(t, h.claimSpecPath(cdiCommonDeviceName))
	spec := readFile(t, h.claimSpecPath("claim-uid"))
	// The character device must come from the ASIC's device node id, not its
	// chip id; the fixtures keep the two apart so that this distinguishes them.
	for _, want := range []string{
		"claim-uid-tt-0",
		"/dev/tenstorrent/" + strconv.Itoa(deviceNodeIDOffset),
		"TENSTORRENT_DEVICE_TT_0_RESOURCE_CLAIM=claim-uid",
		"DRA_ADMIN_ACCESS=false",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("the claim spec does not contain %q:\n%s", want, spec)
		}
	}

	common := readFile(t, h.claimSpecPath(cdiCommonDeviceName))
	for _, want := range []string{"/dev/hugepages", "/dev/hugepages-1G"} {
		if !strings.Contains(common, want) {
			t.Errorf("the common spec does not mount %q:\n%s", want, common)
		}
	}

	h.unprepare(claim)
	assertNotExists(t, h.claimSpecPath("claim-uid"))
	// The common spec belongs to the driver, not to any claim, so releasing
	// the last claim must not take it with it.
	assertExists(t, h.claimSpecPath(cdiCommonDeviceName))
}

// TestSmokePublishesDocumentedAttributes pins the published device contract
// per board shape. These attributes are what users write CEL selectors
// against, so a silent change to any of them breaks claims out in the field
// rather than anything inside this repo.
func TestSmokePublishesDocumentedAttributes(t *testing.T) {
	galaxyDevices := make([]string, 0, 32)
	for chipID := 0; chipID < 32; chipID++ {
		galaxyDevices = append(galaxyDevices, "tt-"+strconv.Itoa(chipID))
	}

	tests := []struct {
		name          string
		topology      *topologypb.HostPhysicalTopology
		wantDevices   []string
		wantBoardName string
		wantBoardType int64
		wantArch      string
		wantChipCount int64
		wantMemory    int64
	}{
		{
			name:          "n150",
			topology:      n150Topology(),
			wantDevices:   []string{"tt-0"},
			wantBoardName: "n150",
			wantBoardType: boardTypeN150,
			wantArch:      archWormhole,
			wantChipCount: 1,
			wantMemory:    wormholeChipMemory,
		},
		{
			name:          "n300",
			topology:      n300Topology(),
			wantDevices:   []string{"tt-0"},
			wantBoardName: "n300",
			wantBoardType: boardTypeN300,
			wantArch:      archWormhole,
			wantChipCount: 2,
			// The bundled total, not the MMIO parent's own DRAM.
			wantMemory: 2 * wormholeChipMemory,
		},
		{
			name:          "p150",
			topology:      p150Topology(),
			wantDevices:   []string{"tt-0"},
			wantBoardName: "p150",
			wantBoardType: boardTypeP150,
			wantArch:      archBlackhole,
			wantChipCount: 1,
			wantMemory:    blackholeChipMemory,
		},
		{
			name: "wormhole galaxy 6U",
			// Every chip on a 6U UBB is PCIe-MMIO, so nothing is bundled and
			// all 32 surface individually.
			topology:      galaxyWormholeTopology(),
			wantDevices:   galaxyDevices,
			wantBoardName: "galaxy-wormhole",
			wantBoardType: boardTypeGalaxyWormhole,
			wantArch:      archWormhole,
			wantChipCount: 1,
			wantMemory:    wormholeChipMemory,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := startHarness(t, withTopology(test.topology))

			devices := h.waitForPublishedDevices(test.wantDevices...)
			device := devices[test.wantDevices[0]]

			// Documented as the constant "tenstorrent.com" in
			// docs/single-host.md, but the profile publishes "tenstorrent".
			// Pinned to what the driver actually emits; the documentation and
			// the code need reconciling either way.
			if got := attrString(t, device, "vendor"); got != "tenstorrent" {
				t.Errorf("vendor is %q, want tenstorrent", got)
			}
			// Passed through from the agent verbatim. docs/single-host.md
			// describes this as "wormhole" and shows a selector matching that,
			// which would not match the "wormhole_b0" the proto documents.
			if got := attrString(t, device, "chipArch"); got != test.wantArch {
				t.Errorf("chipArch is %q, want %q", got, test.wantArch)
			}
			if got := attrString(t, device, "boardName"); got != test.wantBoardName {
				t.Errorf("boardName is %q, want %q", got, test.wantBoardName)
			}
			if got := attrInt(t, device, "boardType"); got != test.wantBoardType {
				t.Errorf("boardType is %d, want %d", got, test.wantBoardType)
			}
			if got := attrInt(t, device, "chipCount"); got != test.wantChipCount {
				t.Errorf("chipCount is %d, want %d", got, test.wantChipCount)
			}
			if got := capacityBytes(t, device, "memory"); got != test.wantMemory {
				t.Errorf("memory capacity is %d, want %d", got, test.wantMemory)
			}
			if got := attrInt(t, device, "chipID"); got != 0 {
				t.Errorf("chipID is %d, want 0", got)
			}
			if got := attrInt(t, device, "trayID"); got != 1 {
				t.Errorf("trayID is %d, want 1", got)
			}
			if got := attrString(t, device, "pciAddress"); got != "0000:01:00.0" {
				t.Errorf("pciAddress is %q, want 0000:01:00.0", got)
			}
			// uniqueID is a uint64 rendered as a string precisely so that an
			// ID above math.MaxInt64 survives. The fixtures use such an ID, so
			// a regression to an int attribute shows up here.
			if got, want := attrString(t, device, "uniqueID"), "18374686479671623680"; got != want {
				t.Errorf("uniqueID is %q, want %q", got, want)
			}
		})
	}
}

// TestSmokeBundlesRemoteSiblingsIntoTheirMMIOParent covers the attributes that
// only exist once a bundle has remote chips, which is what tells a workload
// which extra ASICs it got.
func TestSmokeBundlesRemoteSiblingsIntoTheirMMIOParent(t *testing.T) {
	h := startHarness(t, withTopology(n300Topology()))

	device := h.publishedDevices()["tt-0"]
	if got, want := attrString(t, device, "remoteChipIDs"), "1"; got != want {
		t.Errorf("remoteChipIDs is %q, want %q", got, want)
	}
	if got, want := attrString(t, device, "remoteUniqueIDs"), "18374686479671623681"; got != want {
		t.Errorf("remoteUniqueIDs is %q, want %q", got, want)
	}
}

// TestSmokeKeepsSeparateTraysSeparate checks that a host with two boards
// publishes one device per MMIO anchor, ordered by chip id.
func TestSmokeKeepsSeparateTraysSeparate(t *testing.T) {
	h := startHarness(t, withTopology(twoBoardTopology()))

	devices := h.waitForPublishedDevices("tt-0", "tt-2")
	// tt-0 anchors the n300 and adopts chip 1; tt-2 is the standalone n150.
	if got := attrInt(t, devices["tt-0"], "chipCount"); got != 2 {
		t.Errorf("tt-0 chipCount is %d, want 2", got)
	}
	if got := attrInt(t, devices["tt-2"], "chipCount"); got != 1 {
		t.Errorf("tt-2 chipCount is %d, want 1", got)
	}
}

// TestSmokeDropsTrayWithoutAnMMIOPeer guards the one case where the driver
// must advertise less than the agent reports: chips on a tray with no MMIO
// peer have no host-visible character device, so a container could not open
// them even if it were allocated one.
func TestSmokeDropsTrayWithoutAnMMIOPeer(t *testing.T) {
	h := startHarness(t, withTopology(remoteOnlyTrayTopology()))

	devices := h.waitForPublishedDevices("tt-0")
	for _, unusable := range []string{"tt-7", "tt-8"} {
		if _, ok := devices[unusable]; ok {
			t.Errorf("%s was published despite having no MMIO peer", unusable)
		}
	}
}

// TestSmokeWaitsForTopologyDiscovery covers the startup race the driver is
// built to tolerate: the fabric manager agent is reachable but has not
// finished discovering its own hardware yet. The driver must retry rather than
// publish an empty ResourceSlice, which the scheduler would read as a node
// with no Tenstorrent devices.
func TestSmokeWaitsForTopologyDiscovery(t *testing.T) {
	shortenEnumerateBackoff(t)

	h := newHarness(t, withTopology(n150Topology()))
	h.agent.setNotReady(3)
	h.start()

	if got := h.agent.callCount(); got < 4 {
		t.Errorf("the agent served %d GetTopology calls, want at least 4", got)
	}
	h.waitForPublishedDevices("tt-0")
}

// TestSmokeFailsWhenTheAgentNeverFinishesDiscovery checks the other end of the
// retry budget: once it is exhausted the driver fails instead of coming up
// with nothing to offer.
func TestSmokeFailsWhenTheAgentNeverFinishesDiscovery(t *testing.T) {
	shortenEnumerateBackoff(t)

	h := newHarness(t, withTopology(n150Topology()))
	h.agent.setNotReady(1 << 30)

	err := h.startErr()
	if err == nil {
		t.Fatal("the driver started while the agent was still discovering")
	}
	if !strings.Contains(err.Error(), "topology not yet discovered") {
		t.Errorf("the error does not explain the agent was not ready: %v", err)
	}
}

// TestSmokeFailsWhenTheAgentIsUnreachable covers a down or misaddressed agent.
// Because grpc.NewClient connects lazily, this is the first call that notices,
// and the failure has to be loud: the kubelet probe failing and the driver
// crashlooping is the intended outcome.
func TestSmokeFailsWhenTheAgentIsUnreachable(t *testing.T) {
	// A port nothing listens on: taken and released, so it is free.
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))

	h := newHarness(t, withAgentAddress(address))
	err := h.startErr()
	if err == nil {
		t.Fatal("the driver started without a reachable fabric manager agent")
	}
	if !strings.Contains(err.Error(), "GetTopology") {
		t.Errorf("the error does not name the failing call: %v", err)
	}
	if devices := h.publishedDevicesNow(); len(devices) > 0 {
		t.Errorf("the driver published %v despite failing to enumerate", deviceNames(devices))
	}
}

// TestSmokeSurfacesAnUnexpectedAgentStatus covers a transport-level RPC
// failure, which is neither a not-ready answer nor a success and so must not
// be retried into the backoff budget.
func TestSmokeSurfacesAnUnexpectedAgentStatus(t *testing.T) {
	h := newHarness(t, withTopology(n150Topology()))
	h.agent.setRPCError(status.Error(codes.Internal, "discovery exploded"))

	err := h.startErr()
	if err == nil {
		t.Fatal("the driver started despite the agent failing every call")
	}
	if !strings.Contains(err.Error(), "discovery exploded") {
		t.Errorf("the error does not carry the agent's reason: %v", err)
	}
	// One attempt only: an Internal error is not the retryable not-ready case.
	if got := h.agent.callCount(); got != 1 {
		t.Errorf("the agent served %d calls, want exactly 1 with no retries", got)
	}
}

// TestSmokeLivenessProbeReportsServing exercises the probe the Helm chart wires
// up as the container's livenessProbe. It answers SERVING only once both the
// registration and the DRA socket respond, which makes it a single assertion
// that the driver is actually reachable the way the kubelet needs it to be.
func TestSmokeLivenessProbeReportsServing(t *testing.T) {
	h := startHarness(t, withTopology(n150Topology()), withHealthcheck())

	if got := h.checkHealth(); got != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("the liveness probe reports %s, want SERVING", got)
	}
}

// TestSmokeRestoresClaimSpecsLostWithTheCDIRoot is the node reboot from
// issue #33, driven through the whole driver rather than through DeviceState
// alone: the CDI root is a tmpfs and comes back empty while the checkpoint
// under the kubelet plugin directory survives.
//
// The specs have to be back before the kubelet asks for anything, so the first
// assertion follows the restart with no prepare call in between.
func TestSmokeRestoresClaimSpecsLostWithTheCDIRoot(t *testing.T) {
	h := startHarness(t, withTopology(n300Topology()))

	claim := h.allocate("claim-uid", "tt-0")
	before := h.mustPrepare(claim)
	specBefore := readFile(t, h.claimSpecPath("claim-uid"))

	h.emptyCDIRoot()
	h.restart()

	assertExists(t, h.claimSpecPath(cdiCommonDeviceName))
	if after := readFile(t, h.claimSpecPath("claim-uid")); after != specBefore {
		t.Errorf("the claim spec was not restored identically:\nbefore:\n%s\nafter:\n%s", specBefore, after)
	}

	// The kubelet re-prepares every claim it restores, and the device names it
	// gets back must still resolve, otherwise the pod fails to start.
	after := h.mustPrepare(claim)
	if len(after) != len(before) {
		t.Fatalf("got %d devices after the restart, want %d", len(after), len(before))
	}
	for i := range before {
		if got, want := after[i].GetCdiDeviceIds(), before[i].GetCdiDeviceIds(); !slices.Equal(got, want) {
			t.Errorf("CDI device IDs changed across the restart: got %v, want %v", got, want)
		}
	}
}

// TestSmokeRejectsAClaimWhoseDeviceDisappeared is the other half of a reboot:
// the checkpoint outlives the enumeration, so a chip that did not come back
// leaves an entry naming a device the driver can no longer see. That claim must
// fail by name, and it must not take the claims around it down with it.
func TestSmokeRejectsAClaimWhoseDeviceDisappeared(t *testing.T) {
	h := startHarness(t, withTopology(twoBoardTopology()))

	surviving := h.allocate("claim-keeps", "tt-0")
	doomed := h.allocate("claim-loses", "tt-2")
	h.mustPrepare(surviving)
	h.mustPrepare(doomed)

	// The n150 on tray 2 does not come back.
	h.agent.setTopology(n300Topology())
	h.emptyCDIRoot()
	h.restart()

	h.waitForPublishedDevices("tt-0")

	// The claim whose device survived keeps working, spec and all.
	assertExists(t, h.claimSpecPath("claim-keeps"))
	h.mustPrepare(surviving)

	// The claim whose device is gone fails, naming the device so that the
	// reason lands on the pod instead of inside a container with a missing
	// device node.
	result, ok := h.prepare(doomed)[string(doomed.UID)]
	if !ok {
		t.Fatalf("no result for claim %s", doomed.UID)
	}
	if result.GetError() == "" {
		t.Fatal("prepare succeeded for a device that is no longer allocatable")
	}
	if !strings.Contains(result.GetError(), "tt-2") {
		t.Errorf("the error does not name the missing device: %s", result.GetError())
	}
	assertNotExists(t, h.claimSpecPath("claim-loses"))
}

// TestSmokeRollingUpdateUsesPodScopedSockets covers the socket naming the
// chart's maxSurge rolling update depends on. Two instances overlap during an
// upgrade, so the sockets carry the pod UID to keep them apart — and health.go
// derives the same two paths independently, which is exactly the kind of
// duplicated spelling that drifts.
func TestSmokeRollingUpdateUsesPodScopedSockets(t *testing.T) {
	const podUID = "pod-uid-1"

	h := startHarness(t, withTopology(n150Topology()), withPodUID(podUID), withHealthcheck())

	for _, path := range []string{h.regSocketPath(), h.draSocketPath()} {
		if !strings.Contains(path, podUID) {
			t.Errorf("the socket path %q does not carry the pod UID", path)
		}
		assertExists(t, path)
	}

	// The healthcheck resolves both socket paths on its own, so a SERVING
	// answer proves its spelling still matches the helper's.
	if got := h.checkHealth(); got != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("the liveness probe reports %s, want SERVING", got)
	}

	if got, want := h.getInfo().GetEndpoint(), h.draSocketPath(); got != want {
		t.Errorf("advertised endpoint is %q, want %q", got, want)
	}

	claim := h.allocate("claim-uid", "tt-0")
	h.mustPrepare(claim)
}

// TestSmokeUnprepareIsIdempotent checks what the kubelet does on retry: an
// unprepare it already completed, and one for a claim this driver instance
// never prepared, must both be no-ops rather than errors.
func TestSmokeUnprepareIsIdempotent(t *testing.T) {
	h := startHarness(t, withTopology(n150Topology()))

	claim := h.allocate("claim-uid", "tt-0")
	h.mustPrepare(claim)

	h.unprepare(claim)
	h.unprepare(claim)

	unknown := h.allocate("never-prepared", "tt-0")
	h.unprepare(unknown)
}

// deviceNames lists the published device names in a stable order.
func deviceNames(devices map[string]resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		// Sort tt-<n> numerically so that tt-2 precedes tt-10 and failure
		// messages stay readable on a 32-chip host.
		ai, aerr := strconv.Atoi(strings.TrimPrefix(a, "tt-"))
		bi, berr := strconv.Atoi(strings.TrimPrefix(b, "tt-"))
		if aerr != nil || berr != nil {
			return strings.Compare(a, b)
		}
		return ai - bi
	})
	return names
}
