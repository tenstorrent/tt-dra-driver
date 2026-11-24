# Reporting device health

Status: proposal. Phase 1 needs nothing from the fabric manager; phases 2
and 3 depend on the fabric manager agent change described below.

## Problem

The driver has no notion of a device being unwell. What it publishes is
derived purely from which ASICs and links the fabric manager agent
discovered, so a fault has exactly one way to show up: the affected device
disappears from the ResourceSlice. That is the wrong outcome on every count.

- **Running workloads are not told.** A pod whose chip stopped responding
  keeps running against it; nothing in its status says why it is failing.
- **The scheduler learns too little.** A missing device cannot be allocated,
  which is right, but nothing distinguishes "chip died" from "chip was never
  there", and an operator looking at the slice cannot see either.
- **Eviction is impossible.** A `NoExecute` taint can only act on a device
  that is still published.
- **Degradation looks like miscabling.** Mesh devices are gated on the
  chips the grouping places side by side being linked in
  `local_connections` (see `design/mesh-resource-slices.md`). A link that
  was up and went down is indistinguishable from one that was never cabled,
  so a mesh silently stops being published rather than being reported as
  broken.

`cmd/tt-dra-driver-kubeletplugin/health.go` does not help here: it is a
liveness probe for the plugin's own kubelet sockets, not device health.

## What Kubernetes provides

Kubernetes 1.36 has four relevant mechanisms. They differ in who consumes
them and what consumption does:

| Mechanism | Written by | Effect | Feature gate (1.36) |
| --- | --- | --- | --- |
| `DRAResourceHealth.NodeWatchResources` gRPC stream | kubelet plugin | kubelet sets `allocatedResourcesStatus` in the pod's container statuses; nothing else | `ResourceHealthStatus`, beta, on |
| `Device.Taints` in a ResourceSlice | driver | `NoSchedule` blocks new allocations; `NoExecute` also evicts non-tolerating pods; `None` is informational | `DRADeviceTaints`, beta, on |
| `DeviceTaintRule` | admin / external controller | taints devices by selector without the driver's involvement | `DRADeviceTaintRules`, beta, **off** |
| `ResourceClaim.status.devices[]` | driver | per-allocation conditions and up to 10 KiB of data, for users and tooling | `DRAResourceClaimDeviceStatus`, beta, on |

The two the driver should use are the health stream, which tells *running
pods*, and taints, which steer the *scheduler* and, if allowed, evict.
`DeviceTaintRule` is for operators draining hardware for maintenance and
needs nothing from us beyond documentation.

### Properties that shape the design

These come from reading the 1.36 sources, not from the KEP text:

- **The stream is snapshot-based with a staleness timeout.** Each
  `NodeWatchResourcesResponse` is the complete set of devices; kubelet
  reconciles against it. A device not refreshed within its
  `health_check_timeout_seconds` (default 30s,
  `pkg/kubelet/cm/dra/healthinfo.go`) reverts to `Unknown`. The driver must
  therefore re-send on a timer, not only on change. Kubelet only triggers pod
  status updates when a device's status or message actually changes, so a
  repeated identical snapshot costs nothing downstream.
- **Registration is automatic.** `kubeletplugin.Start` registers the service
  if the plugin type implements `drahealthv1alpha1.DRAResourceHealthServer`
  (`kubeletplugin/draplugin.go`). There is no option to set. Kubelet
  reconnects on its own when the stream drops.
- **Health is matched against allocation results.** Kubelet looks up the
  `(pool, device)` of each allocated device. A pod holding `mesh-…` sees only
  what is reported for `mesh-…`, never for the chips in it. Composites need
  health of their own.
- **Taints do not propagate through counters.** A tainted chip does not make
  the tray or mesh consuming its counter unschedulable; the allocator just
  finds the counter free. Every composite covering a tainted chip must carry
  the taint itself.
- **Taints lower the per-slice device cap.** A slice in which any device has
  taints may hold 64 devices rather than 128. `buildSlices` already uses 64
  whenever counters are published, but the chips-only profile uses 128.
- **Dropped taints are fatal as we handle errors today.** If the cluster has
  `DRADeviceTaints` disabled, the apiserver strips the field and the
  resourceslice controller reports a `resourceslice.DroppedFieldsError`
  through `HandleError`. That error is not `ErrRecoverable`, so
  `driver.HandleError` cancels the main context and the driver crash-loops.

## Health model

Health is computed per published device, in the profile, alongside the
resources it already produces:

- **Chip:** the health the fabric manager reports for the MMIO ASIC and any
  remote ASICs bundled with it, worst first.
- **Tray:** worst of its chips.
- **Mesh:** worst of its chips, and unhealthy if any link between chips the
  grouping places side by side is down.

Each device's health carries a state, a reason, whether the condition is
transient, a since-timestamp and a message. The state and reason drive both
outputs:

| Driver state | Kubelet status | Taint, default | Taint, `NoExecute` opted in |
| --- | --- | --- | --- |
| healthy | `HEALTHY` | none | none |
| degraded | `UNHEALTHY` | `<driverName>/degraded`, `NoSchedule` | same |
| unhealthy, transient | `UNHEALTHY` | `<driverName>/unhealthy`, `NoSchedule` | same |
| unhealthy, persistent | `UNHEALTHY` | `<driverName>/unhealthy`, `NoSchedule` | `<driverName>/unhealthy`, `NoExecute` |
| unknown | `UNKNOWN` | none | none |

Choices worth stating:

- **`NoExecute` is opt-in.** A false positive evicts a long-running job, and
  for a genuinely dead chip the workload has usually failed already. The
  flag only ever upgrades persistent failures; degraded and transient
  conditions never evict.
- **Unknown does not taint.** It is what every device becomes when the
  fabric manager agent restarts. Tainting on it would make a routine agent
  restart block scheduling across the node.
- **Two keys, reason as the value.** Separate `degraded` and `unhealthy` keys
  let a workload tolerate one without the other. The value is the reason in
  label-value form (`arc-unresponsive`, `link-down`, …), so a diagnostics pod
  can tolerate exactly the fault it is meant to investigate. One taint per
  device keeps well under the 16-taint limit.
- **Unhealthy devices stay published.** Removal is reserved for hardware the
  fabric manager says is not there at all.
- **`TimeAdded` comes from the fabric manager's since-timestamp**, so it
  records when the fault started rather than when the driver noticed. The
  resourceslice controller already preserves it across republishes.

## What to ask of the fabric manager

Everything above needs per-ASIC and per-link health with reasons, and
`fm-proto/topology.proto` has none of it. The agent already watches
telemetry — `WatchTopology` notes that it triggers rediscovery "in response
to telemetry-observed link changes" — so the request is largely to expose
what it already observes.

### The gaps

1. **Failed ASICs vanish.** `HostPhysicalTopology.asics` holds only what
   discovery found. The driver needs the ASICs the host is *expected* to
   have — from PCI enumeration or the PSD — including those that failed,
   with `chip_id`, `tray_id` and `asic_location` stable, since device names
   are built from `chip_id`.
2. **Down links vanish.** `local_connections` and `exit_nodes` appear to list
   only links that came up. The driver needs every link the platform expects,
   with its state.
3. **No reasons or severity.** `discovery_error` is one host-wide string.
   Taint values and the `NoExecute` decision need a machine-readable reason
   and a transient/persistent distinction.
4. **No freshness.** Snapshots are sent only on change, so a stalled
   telemetry monitor is indistinguishable from a healthy host.

### Requested additions

A `HostHealth` message in the shared `fabricmanager.topology` package,
carried in `WatchTopologyResponse` rather than inside
`HostPhysicalTopology`:

```proto
message HostHealth {
  repeated AsicHealth asics = 1;
  repeated LinkHealth links = 2;
  // When the agent's health monitor last completed a pass, so a stalled
  // monitor reads as stale rather than healthy.
  int64 last_checked_unix = 3;
  // False when the agent has no telemetry, e.g. under fallback discovery.
  bool telemetry_available = 4;
}

message AsicHealth {
  uint64 unique_id = 1;
  HealthState state = 2;    // HEALTHY, DEGRADED, UNHEALTHY, UNKNOWN
  HealthReason reason = 3;  // stable enum, see below
  bool transient = 4;       // expected to recover without intervention
  int64 since_unix = 5;
  string message = 6;
}

message LinkHealth {
  uint64 src_asic_id = 1;
  uint32 src_channel = 2;
  uint64 dst_asic_id = 3;
  uint32 dst_channel = 4;
  LinkState state = 5;      // UP, DOWN, TRAINING, DEGRADED
  bool expected = 6;        // the platform descriptor says this link exists
  uint32 retrain_count = 7;
  int64 since_unix = 8;
}
```

Why this shape:

- **Same stream, separate message.** One snapshot keeps topology and health
  consistent with each other, and avoids spending a second slot against the
  agent's cap on open watches (`RESOURCE_EXHAUSTED`). Keeping it out of
  `HostPhysicalTopology` leaves the controller's registration and heartbeat
  payloads unaffected unless the fabric manager wants them there too.
- **Expected-but-failed ASICs belong in `asics` as well**, not only in
  `HostHealth`, so that the existing bundling and device naming keep working
  for them unchanged.

### Questions for the fabric manager team

- **Which signals exist today?** The candidates for `HealthReason` are ARC
  firmware heartbeat, temperature against throttle and shutdown thresholds,
  DRAM training failures, PCIe link speed or width below expected, AER
  errors, device reset in progress, firmware bundle mismatch across the
  host, and a remote ASIC unreachable through its MMIO parent. The enum
  should cover what the agent can actually observe, not this whole list.
- **Who debounces?** Flapping links or temperatures near a threshold would
  produce a snapshot per transition, each carrying the cluster descriptor
  YAML. The agent sees raw telemetry and is the better place for
  hysteresis; failing that, health-only updates could omit the YAML.
- **What does fallback discovery know?** If `DISCOVERY_METHOD_FALLBACK`
  means no telemetry, `telemetry_available = false` lets the driver report
  `UNKNOWN` rather than infer health it does not have.

## Phase 1 — health stream from what we already know

No proto change. Lands the plumbing so that phase 2 only changes where the
health comes from.

- **Profile.** `DeviceWatch` grows a way to deliver health with each update,
  so resources and health stay consistent with each other. A
  `map[deviceName]DeviceHealth` alongside `DriverResources` is the simplest
  shape.
- **Signals available today:**
  - Agent unreachable, or snapshots not usable: every device `UNKNOWN`,
    message naming the cause. Today these snapshots are skipped and the last
    resources kept; that stays, health is what changes.
  - A device published earlier in this process's lifetime and now gone:
    `UNHEALTHY`, "no longer reported by the fabric manager". It is not
    republished in the slice — that needs phase 2 — but it stays in the
    health stream, which is exactly the case that matters: a running pod
    holding the device it lost.
- **Driver.** `driver` implements `NodeWatchResources`. Each open stream
  subscribes to a fan-out from the device state; it receives the current
  snapshot immediately, then one on every change and one every 10 seconds,
  with `health_check_timeout_seconds` set to 30.
- **Rename** `health.go` to `liveness.go` and `healthcheck` to `liveness`,
  so the two are not confused.

Tests: the health model per device kind; the fan-out with multiple and
reconnecting streams; and an extension of the existing live-topology-change
smoke test in which a chip disappears and the stream reports it.

## Phase 2 — consume fabric manager health

Depends on the proto addition above.

- **Vendor the new proto** and carry `HostHealth` through
  `fabricmanager.Snapshot`.
- **Keep failed ASICs published.** Because the agent now reports them, they
  go through the normal bundling and naming path; only their health
  differs. Prepare does not refuse an unhealthy device: a pod that
  tolerates the taint has asked for it.
- **Validate meshes against expected links, judge them by actual links.**
  Mesh gating checks adjacency against links with `expected` set, so a mesh
  is published whenever the host is cabled the way the grouping says; a
  down link then makes it unhealthy instead of making it disappear. The
  check is still independent of our hardcoded pairing — the expected links
  come from the PSD, the pairing from the PGD — so it keeps its value as
  a guard against a wrong grouping table.
- **Staleness.** `last_checked_unix` older than a threshold, or
  `telemetry_available == false`, turns every device `UNKNOWN`.
- **Drop the phase 1 tombstones.** Gone-but-remembered devices were a
  stand-in for the agent's expected inventory; once it exists, a device the
  agent does not report is genuinely absent.

## Phase 3 — taints

- **Taints per the table above**, applied in `applyTopology` from the health
  model, with every chip taint copied onto each tray and mesh covering that
  chip. A composite takes the most severe taint among its members and its
  links.
- **Opt-in eviction.** An `--evict-on-device-failure` flag, Helm value
  `kubeletPlugin.deviceHealth.evictOnFailure`, default false.
- **Fixed slice cap.** Use `ResourceSliceMaxDevicesWithAdvancedFeatures`
  unconditionally once health is enabled, so that the first taint on a node
  does not reshuffle its slices.
- **Survive clusters without `DRADeviceTaints`.** `HandleError` recognises a
  `DroppedFieldsError` whose `DisabledFeatures` include `DRADeviceTaints`,
  logs once, disables taints for the life of the process and republishes
  without them, and treats the error as recoverable. The health stream keeps
  working either way.
- **Documentation.** The taint keys and reasons, a toleration example for
  diagnostics pods, and `DeviceTaintRule` for operator-driven drains.

Tests: taint propagation onto composites; the `NoSchedule`/`NoExecute`
decision across state, transience and the flag; an allocator pass showing a
tainted chip blocks the tray and mesh over it; and the dropped-fields path.

## Not planned

- **Claim device status.** Writing a `Ready` condition and diagnostic data
  into `ResourceClaim.status.devices` during Prepare would need
  `resourceclaims/status` RBAC the chart does not grant today. It reports
  setup outcome rather than ongoing health, which the stream already covers
  for running pods. Worth revisiting if users ask for it.
- **Binding conditions.** Holding a pod until, say, a mesh's links have been
  retrained through `RetrainLinks` fits `BindingConditions`, but needs a
  controller-side component that sets the condition. Out of scope for a
  node-local plugin.
- **Acting on health.** The driver reports; it does not reset chips or
  retrain links. Remediation stays with the fabric manager and operators.

## Open questions

- **How long should a phase 1 tombstone live?** Until the device returns,
  or for a bounded time? Kubelet only cares while a pod holds the device, so
  unbounded growth needs a host where devices churn, which seems unlikely.
- **Should degraded taint at all?** If the reasons the agent can produce for
  `DEGRADED` (e.g. a PCIe link at reduced width) do not affect correctness,
  `None` may be the better default effect.
- **Remote ASICs.** A chip bundle's health takes the worst of its MMIO and
  remote ASICs. Is a remote ASIC failing always fatal to the bundle, or can
  the MMIO chip still be useful on its own?
