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

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"github.com/tenstorrent/tt-dra-driver/internal/fabricmanager"
	topologypb "github.com/tenstorrent/tt-dra-driver/internal/fabricmanager/proto/topology"
)

const (
	// harnessTimeout bounds a whole scenario. Publishing a ResourceSlice costs
	// about a second of controller sync delay, so this is generous rather than
	// tight; a hang is better reported by the enclosing go test timeout.
	harnessTimeout = 90 * time.Second

	// pollInterval is how often the harness re-checks asynchronous state.
	pollInterval = 20 * time.Millisecond

	// testNodeUID stands in for the UID the ResourceSlice controller reads off
	// the Node object to own the slices it publishes.
	testNodeUID = "node-uid"
)

// harness runs a real kubelet plugin instance in-process and drives it the way
// the kubelet would.
//
// Everything outside the driver is substituted through a seam the driver
// already exposes as a flag: the Tenstorrent hardware behind the fabric
// manager agent address, the kubelet behind the registrar and plugin
// directories, the API server behind the injected clientset, and the CDI root
// behind its own directory. The driver under test is assembled through the
// production validProfiles wiring, so no test-only path exists in the driver
// itself.
type harness struct {
	t   *testing.T
	ctx context.Context

	agent  *fakeAgent
	client *fake.Clientset

	// Directories survive restart so that a second driver instance sees the
	// checkpoint and CDI root the first one left behind.
	cdiRoot   string
	registrar string
	plugins   string

	agentAddress    string
	podUID          string
	healthcheckPort int

	config       *Config
	driver       *driver
	driverCancel context.CancelFunc
	agentClient  *fabricmanager.AgentClient
	conns        []*grpc.ClientConn

	reg    registerapi.RegistrationClient
	dra    drapb.DRAPluginClient
	health grpc_health_v1.HealthClient

	fatalMu sync.Mutex
	fatal   []error
}

// harnessOption customises a harness before its driver is started.
type harnessOption func(*harness)

// withTopology sets what the fake fabric manager agent reports. The default is
// an n300.
func withTopology(topology *topologypb.HostPhysicalTopology) harnessOption {
	return func(h *harness) { h.agent.setTopology(topology) }
}

// withHealthcheck enables the driver's gRPC liveness service on a port the
// harness picks, so a test can probe it the way the kubelet's livenessProbe
// does.
func withHealthcheck() harnessOption {
	return func(h *harness) { h.healthcheckPort = freePort(h.t) }
}

// withPodUID enables the rolling-update socket naming, in which both the
// registration and DRA sockets carry the pod UID.
func withPodUID(uid string) harnessOption {
	return func(h *harness) { h.podUID = uid }
}

// withAgentAddress overrides the fabric manager agent address, for scenarios
// that need it to be unreachable.
func withAgentAddress(address string) harnessOption {
	return func(h *harness) { h.agentAddress = address }
}

// newHarness prepares a harness without starting the driver. Use startHarness
// unless the scenario is about startup failing.
func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), harnessTimeout)
	t.Cleanup(cancel)

	// Unix socket paths are capped near 104 bytes on darwin, and the default
	// macOS TMPDIR consumes most of that before the test name is appended, so
	// the directories that hold sockets get a short base of their own instead
	// of t.TempDir().
	base, err := os.MkdirTemp("/tmp", "ttdra")
	if err != nil {
		t.Fatalf("create the harness base directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	h := &harness{
		t:         t,
		ctx:       ctx,
		agent:     startFakeAgent(t, n300Topology()),
		cdiRoot:   filepath.Join(base, "cdi"),
		registrar: filepath.Join(base, "registrar"),
		plugins:   filepath.Join(base, "plugins"),
		// Disabled unless a scenario asks for it, so that tests do not compete
		// for ports they do not use.
		healthcheckPort: -1,
		client: fake.NewSimpleClientset(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: testNodeName, UID: types.UID(testNodeUID)},
		}),
	}
	h.agentAddress = h.agent.addr

	for _, dir := range []string{h.cdiRoot, h.registrar, h.plugins} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	for _, opt := range opts {
		opt(h)
	}

	t.Cleanup(func() {
		h.stop()
		// In production a non-recoverable background error cancels the main
		// context and takes the process down. Nothing here should provoke one,
		// so report it rather than letting a driver that asked to die look
		// like a passing test.
		for _, err := range h.fatalErrors() {
			t.Errorf("the driver reported a non-recoverable background error: %v", err)
		}
	})
	return h
}

// startHarness prepares a harness and starts its driver.
func startHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()

	h := newHarness(t, opts...)
	h.start()
	return h
}

// start brings up a driver instance and connects the kubelet-side clients to
// it, failing the test if either step fails.
func (h *harness) start() {
	h.t.Helper()

	if err := h.startErr(); err != nil {
		h.t.Fatalf("start the driver: %v", err)
	}
	h.connect()
}

// startErr brings up a driver instance and returns whatever error the driver
// reports, for scenarios that assert on startup failing.
func (h *harness) startErr() error {
	h.t.Helper()

	agentClient, err := fabricmanager.Dial(h.agentAddress)
	if err != nil {
		return fmt.Errorf("dial the fake agent: %w", err)
	}
	h.agentClient = agentClient

	flags := &Flags{
		nodeName:                      testNodeName,
		cdiRoot:                       h.cdiRoot,
		kubeletRegistrarDirectoryPath: h.registrar,
		kubeletPluginsDirectoryPath:   h.plugins,
		driverName:                    testDriverName,
		profile:                       testProfile,
		healthcheckPort:               h.healthcheckPort,
		podUID:                        h.podUID,
		fabricManagerAgentAddress:     h.agentAddress,
	}
	h.config = &Config{
		flags:         flags,
		coreclient:    h.client,
		profile:       validProfiles[testProfile](*flags, agentClient),
		cancelMainCtx: h.recordFatal,
	}

	// RunPlugin creates this directory before it builds the driver; the
	// harness stands in for RunPlugin, so it has to do the same.
	if err := os.MkdirAll(h.config.DriverPluginPath(), 0750); err != nil {
		return fmt.Errorf("create the driver plugin path: %w", err)
	}

	// The driver's watch outlives NewDriver, so each instance gets a context
	// the harness can cancel: a driver that has been stopped must not keep
	// republishing behind the one that replaced it.
	ctx, cancel := context.WithCancel(h.ctx)
	h.driverCancel = cancel

	driver, err := NewDriver(ctx, h.config)
	if err != nil {
		return err
	}
	h.driver = driver
	return nil
}

// stop shuts the driver down and releases the kubelet-side connections. It is
// safe to call on a harness whose driver never started.
func (h *harness) stop() {
	if h.driverCancel != nil {
		h.driverCancel()
		h.driverCancel = nil
	}
	for _, conn := range h.conns {
		_ = conn.Close()
	}
	h.conns = nil
	h.reg, h.dra, h.health = nil, nil, nil

	if h.driver != nil {
		if err := h.driver.Shutdown(klog.Background()); err != nil {
			h.t.Errorf("shut the driver down: %v", err)
		}
		h.driver = nil
	}
	if h.agentClient != nil {
		_ = h.agentClient.Close()
		h.agentClient = nil
	}
}

// restart replaces the running driver with a fresh instance over the same
// directories, API objects and fabric manager agent.
//
// This is how the harness models the driver coming back after a crash, an
// upgrade or a node reboot: the checkpoint under the plugin directory survives,
// and so does whatever the test did to the CDI root in between.
func (h *harness) restart() {
	h.t.Helper()

	h.stop()
	h.discardPublishedSlices()
	h.start()
}

// discardPublishedSlices removes the ResourceSlices the previous driver
// instance published, so that what the next one publishes can be asserted on.
//
// This compensates for the fake clientset rather than modelling anything the
// driver does. A real API server assigns a name from generateName, which is
// how the restarted ResourceSlice controller recognises the slice it published
// before and updates it in place; the fake stores an empty name instead, so
// the controller cannot find it and the stale slice lingers beside the new
// one. The end state on a real cluster is a pool that describes current
// hardware, which is what clearing them here reproduces. Running this harness
// against a real API server would make the call unnecessary.
func (h *harness) discardPublishedSlices() {
	h.t.Helper()

	if err := h.client.ResourceV1().ResourceSlices().
		DeleteCollection(h.ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
		h.t.Fatalf("discard the published resource slices: %v", err)
	}
}

// connect dials the two sockets the kubelet uses, plus the liveness service
// when it is enabled.
func (h *harness) connect() {
	h.t.Helper()

	regSocket := h.regSocketPath()
	h.waitForSocket(regSocket)
	h.reg = registerapi.NewRegistrationClient(h.dial("unix://" + regSocket))

	draSocket := h.draSocketPath()
	h.waitForSocket(draSocket)
	h.dra = drapb.NewDRAPluginClient(h.dial("unix://" + draSocket))

	if h.healthcheckPort > 0 {
		h.health = grpc_health_v1.NewHealthClient(
			h.dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(h.healthcheckPort))))
	}
}

// regSocketPath is where the driver advertises itself for kubelet plugin
// registration. It is spelled out rather than derived so that a change to the
// naming shows up here as a failing test.
func (h *harness) regSocketPath() string {
	name := testDriverName + "-reg.sock"
	if h.podUID != "" {
		name = testDriverName + "-" + h.podUID + "-reg.sock"
	}
	return filepath.Join(h.registrar, name)
}

// draSocketPath is where the driver serves the DRA gRPC API to the kubelet.
func (h *harness) draSocketPath() string {
	name := "dra.sock"
	if h.podUID != "" {
		name = "dra-" + h.podUID + ".sock"
	}
	return filepath.Join(h.config.DriverPluginPath(), name)
}

func (h *harness) dial(target string) *grpc.ClientConn {
	h.t.Helper()

	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		h.t.Fatalf("dial %s: %v", target, err)
	}
	h.conns = append(h.conns, conn)
	return conn
}

// waitForSocket blocks until the driver has created the given socket. The
// kubeletplugin helper sets its listeners up in the background, so a client
// that dials immediately after NewDriver can lose the race.
func (h *harness) waitForSocket(path string) {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the driver never created the socket %s", path)
		}
		time.Sleep(pollInterval)
	}
}

// recordFatal stands in for the main context cancellation that a
// non-recoverable background error triggers in production, so that a scenario
// can assert the driver asked to go down.
func (h *harness) recordFatal(err error) {
	h.fatalMu.Lock()
	defer h.fatalMu.Unlock()

	h.fatal = append(h.fatal, err)
}

// fatalErrors returns the non-recoverable background errors the driver
// reported so far.
func (h *harness) fatalErrors() []error {
	h.fatalMu.Lock()
	defer h.fatalMu.Unlock()

	return append([]error(nil), h.fatal...)
}

// allocate creates an allocated ResourceClaim in the API, as the scheduler
// would once it picked devices on this node. The kubelet passes only a claim
// reference over the DRA socket and the plugin fetches the claim itself, so the
// object has to exist before prepare is called.
func (h *harness) allocate(uid string, deviceNames ...string) *resourceapi.ResourceClaim {
	h.t.Helper()

	claim := newTestClaim(uid, deviceNames...)
	if _, err := h.client.ResourceV1().ResourceClaims(claim.Namespace).
		Create(h.ctx, claim, metav1.CreateOptions{}); err != nil {
		h.t.Fatalf("create the resource claim %s: %v", uid, err)
	}
	return claim
}

// prepare calls NodePrepareResources over the DRA socket for the given claims
// and returns the per-claim responses keyed by claim UID.
//
// A transport failure fails the test; a per-claim error is left in the response
// for the caller to assert on, because that is how the driver reports a claim
// it cannot satisfy.
func (h *harness) prepare(claims ...*resourceapi.ResourceClaim) map[string]*drapb.NodePrepareResourceResponse {
	h.t.Helper()

	resp, err := h.dra.NodePrepareResources(h.ctx, &drapb.NodePrepareResourcesRequest{
		Claims: claimRefs(claims),
	})
	if err != nil {
		h.t.Fatalf("NodePrepareResources: %v", err)
	}
	return resp.GetClaims()
}

// mustPrepare prepares a single claim and fails the test unless the driver
// satisfied it, returning the devices it handed back to the kubelet.
func (h *harness) mustPrepare(claim *resourceapi.ResourceClaim) []*drapb.Device {
	h.t.Helper()

	result, ok := h.prepare(claim)[string(claim.UID)]
	if !ok {
		h.t.Fatalf("no result for claim %s", claim.UID)
	}
	if result.GetError() != "" {
		h.t.Fatalf("prepare claim %s: %s", claim.UID, result.GetError())
	}
	return result.GetDevices()
}

// unprepare calls NodeUnprepareResources over the DRA socket and fails the
// test unless every claim was released.
func (h *harness) unprepare(claims ...*resourceapi.ResourceClaim) {
	h.t.Helper()

	resp, err := h.dra.NodeUnprepareResources(h.ctx, &drapb.NodeUnprepareResourcesRequest{
		Claims: claimRefs(claims),
	})
	if err != nil {
		h.t.Fatalf("NodeUnprepareResources: %v", err)
	}
	for uid, result := range resp.GetClaims() {
		if result.GetError() != "" {
			h.t.Fatalf("unprepare claim %s: %s", uid, result.GetError())
		}
	}
}

// claimRefs reduces claims to the references the kubelet actually sends.
func claimRefs(claims []*resourceapi.ResourceClaim) []*drapb.Claim {
	refs := make([]*drapb.Claim, 0, len(claims))
	for _, claim := range claims {
		refs = append(refs, &drapb.Claim{
			Uid:       string(claim.UID),
			Name:      claim.Name,
			Namespace: claim.Namespace,
		})
	}
	return refs
}

// getInfo performs the kubelet's plugin registration handshake.
func (h *harness) getInfo() *registerapi.PluginInfo {
	h.t.Helper()

	info, err := h.reg.GetInfo(h.ctx, &registerapi.InfoRequest{})
	if err != nil {
		h.t.Fatalf("GetInfo: %v", err)
	}
	return info
}

// checkHealth runs the liveness probe the chart configures, which reports
// SERVING only once both the registration and DRA sockets answer.
func (h *harness) checkHealth() grpc_health_v1.HealthCheckResponse_ServingStatus {
	h.t.Helper()

	if h.health == nil {
		h.t.Fatal("the harness was not started withHealthcheck")
	}
	resp, err := h.health.Check(h.ctx, &grpc_health_v1.HealthCheckRequest{Service: "liveness"})
	if err != nil {
		h.t.Fatalf("health Check: %v", err)
	}
	return resp.GetStatus()
}

// publishedDevices waits until the driver has published a ResourceSlice for
// this node and returns the devices it advertises, keyed by device name.
//
// PublishResources hands off to a background controller instead of writing
// through, so this has to poll. The fake clientset also does not implement
// generateName, so every slice it stores has an empty name and the controller
// cannot recognise the slice it created; that can leave more than one slice for
// the pool. The devices are therefore collected across all of the node's
// slices rather than asserted against slice identity. Running the same harness
// against a real API server removes the caveat.
func (h *harness) publishedDevices() map[string]resourceapi.Device {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		devices := h.publishedDevicesNow()
		if len(devices) > 0 {
			return devices
		}
		if time.Now().After(deadline) {
			h.t.Fatal("the driver never published a ResourceSlice for the node")
		}
		time.Sleep(pollInterval)
	}
}

// waitForPublishedDevices waits until the node's published device names are
// exactly want, and returns the devices.
//
// Publishing is asynchronous, so a scenario that knows which devices it
// expects should converge on them rather than read once and hope the
// background controller has caught up.
func (h *harness) waitForPublishedDevices(want ...string) map[string]resourceapi.Device {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var got []string
	for {
		devices := h.publishedDevicesNow()
		got = deviceNames(devices)
		if slices.Equal(got, want) {
			return devices
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the driver published devices %v, want %v", got, want)
		}
		time.Sleep(pollInterval)
	}
}

// publishedDevicesNow reads the currently published devices without waiting.
func (h *harness) publishedDevicesNow() map[string]resourceapi.Device {
	h.t.Helper()

	slices, err := h.client.ResourceV1().ResourceSlices().List(h.ctx, metav1.ListOptions{})
	if err != nil {
		h.t.Fatalf("list resource slices: %v", err)
	}

	devices := make(map[string]resourceapi.Device)
	for _, slice := range slices.Items {
		if slice.Spec.Driver != testDriverName || slice.Spec.Pool.Name != testNodeName {
			continue
		}
		for _, device := range slice.Spec.Devices {
			devices[device.Name] = device
		}
	}
	return devices
}

// attrString reads a string attribute off a published device.
func attrString(t *testing.T, device resourceapi.Device, name string) string {
	t.Helper()

	attr, ok := device.Attributes[resourceapi.QualifiedName(name)]
	if !ok {
		t.Fatalf("device %q has no attribute %q", device.Name, name)
	}
	if attr.StringValue == nil {
		t.Fatalf("attribute %q of device %q is not a string: %+v", name, device.Name, attr)
	}
	return *attr.StringValue
}

// attrInt reads an integer attribute off a published device.
func attrInt(t *testing.T, device resourceapi.Device, name string) int64 {
	t.Helper()

	attr, ok := device.Attributes[resourceapi.QualifiedName(name)]
	if !ok {
		t.Fatalf("device %q has no attribute %q", device.Name, name)
	}
	if attr.IntValue == nil {
		t.Fatalf("attribute %q of device %q is not an integer: %+v", name, device.Name, attr)
	}
	return *attr.IntValue
}

// capacityBytes reads a capacity off a published device.
func capacityBytes(t *testing.T, device resourceapi.Device, name string) int64 {
	t.Helper()

	capacity, ok := device.Capacity[resourceapi.QualifiedName(name)]
	if !ok {
		t.Fatalf("device %q has no capacity %q", device.Name, name)
	}
	return capacity.Value.Value()
}

// freePort asks the kernel for an unused TCP port and releases it again.
//
// The healthcheck service binds whatever port it is given but does not report
// back the one it chose when asked for a random port, so a test that wants to
// probe it has to pick the number itself.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", listener.Addr())
	}
	return addr.Port
}

// shortenInitialDevicesTimeout shrinks how long the driver waits for its first
// set of devices, for the duration of a test.
//
// The timeout is a package variable rather than a flag, which is what lets a
// test tighten it without the driver growing a knob that only tests would set.
// Production keeps its five-minute budget.
func shortenInitialDevicesTimeout(t *testing.T) {
	t.Helper()

	original := initialDevicesTimeout
	t.Cleanup(func() { initialDevicesTimeout = original })

	initialDevicesTimeout = 100 * time.Millisecond
}

// emptyCDIRoot removes every file from the CDI root, modelling the tmpfs that
// most distributions mount there coming back empty after a node reboot.
func (h *harness) emptyCDIRoot() {
	h.t.Helper()

	entries, err := os.ReadDir(h.cdiRoot)
	if err != nil {
		h.t.Fatalf("read the CDI root: %v", err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(h.cdiRoot, entry.Name())); err != nil {
			h.t.Fatalf("empty the CDI root: %v", err)
		}
	}
}

// claimSpecPath is the file the container runtime resolves a claim's CDI
// devices against.
func (h *harness) claimSpecPath(transientID string) string {
	return specPath(h.config, transientID)
}
