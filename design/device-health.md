# Reporting device health

Status: phase 1 is implemented. Phases 2 and 3 depend on the fabric manager
agent change described below.

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

Everything above needs per-ASIC health, and `fm-proto/topology.proto` has
none of it. This is the request as agreed with the fabric manager side so
far; the first version is deliberately small and built to grow.

### The gaps

1. **Failed ASICs vanish.** `HostPhysicalTopology.asics` holds only what
   discovery found, so an ASIC that failed to probe is indistinguishable
   from one that was never there. The driver needs the ASICs the host is
   *expected* to have, from the factory system descriptor (FSD), including
   those that failed.
2. **No identity that survives failure.** `unique_id` needs the ethernet
   subsystem to have come up, and `chip_id` is assigned during enumeration,
   so a device that fails to probe has neither, and `chip_id` can be
   reordered among the devices that did. The only identity available for
   every expected ASIC is where it sits: `(tray_id, asic_location)`.
3. **No health.** Nothing says whether an ASIC is working, and
   `discovery_error` is one host-wide string.

Link health is a gap too, and is left out of the first version; see
"Deferred" below for what that costs.

### Requested additions

A `HostHealth` message, carried as a new field of `WatchTopologyResponse`:

```proto
message WatchTopologyResponse {
  // ... existing fields 1-4 ...

  // Absent when the agent does not report health. Consumers must treat
  // absence as "unknown", not as "healthy".
  fabricmanager.topology.HostHealth health = 5;
}

message HostHealth {
  // One entry per ASIC the FSD says this host has, whether or not it
  // probed. Complete: an ASIC missing from this list is not expected.
  repeated AsicHealth asics = 1;
}

enum HealthState {
  HEALTH_STATE_UNKNOWN = 0;
  HEALTH_STATE_HEALTHY = 1;
  HEALTH_STATE_DEGRADED = 2;
  HEALTH_STATE_UNHEALTHY = 3;
}

// Stable and machine-readable; the driver uses it as a taint value, so new
// values are added, never renamed. The first version needs one.
enum HealthReason {
  HEALTH_REASON_UNSPECIFIED = 0;
  HEALTH_REASON_PROBE_FAILED = 1;  // expected by the FSD, did not probe
}

message AsicHealth {
  // Identity. The only fields guaranteed for a failed ASIC.
  uint32 tray_id = 1;
  uint32 asic_location = 2;

  // The aggregate of whatever signals the agent has, one value so that
  // better signals can be added without changing this message.
  HealthState state = 3;
  HealthReason reason = 4;       // unset when HEALTHY
  bool transient = 5;            // expected to recover without intervention
  int64 since_unix = 6;          // when the state began; 0 if unknown
  string message = 7;            // human-readable detail

  // Facts available from enumeration without the device's cooperation.
  // What the agent cannot tell is left zero or empty.
  bool is_mmio_capable = 8;
  string pci_address = 9;
  uint32 device_node_id = 10;
  string device_node_path = 11;
}
```

Why this shape:

- **Keyed by `(tray_id, asic_location)`.** See gap 2. `unique_id` and
  `chip_id` stay where they are, in `AsicInfo`, as facts about ASICs that
  did probe.
- **Failed ASICs appear only in `HostHealth`, not in `asics`.** An older
  driver talking to a newer agent must not see an ASIC with no `unique_id`
  or `chip_id` in the list it publishes devices from. The new driver builds
  what it needs for a failed ASIC from the identity and enumeration facts
  above.
- **One aggregated `state`.** The agent combines its signals into one value;
  the first signal is whether an expected ASIC probed. A single state keeps
  the message stable as signals are added, and keeps the combining rule in
  the component that understands the signals.
- **Same stream.** One snapshot keeps topology and health consistent with
  each other, and avoids a second slot against the agent's cap on open
  watches (`RESOURCE_EXHAUSTED`).
- **Emit on change of aggregated state only.** Probing does not flap, so
  no hysteresis is needed yet. When richer signals arrive, the agent is the
  place to debounce them, since it sees the raw readings.
- **Backward compatible.** Both directions: the field is new and optional,
  and nothing in `HostPhysicalTopology` changes meaning.

### Deferred

- **Link health.** Without it, a mesh whose link went down still disappears
  because mesh gating uses `local_connections`, which lists only links that
  came up. Mesh health in the first version is therefore the health of its
  chips only. Expected links from the FSD, with a state, would fix this; the
  endpoints would also be keyed by `(tray_id, asic_location, channel)`,
  since `unique_id` is absent exactly when a link is down.
- **Richer reasons and `DEGRADED`.** Thermals, ARC heartbeat, DRAM
  training, PCIe link state and so on, as the agent gains signals.
- **Freshness.** Whether the agent's health view is current. Probe results
  do not go stale the way telemetry does, so this waits for a signal that
  can.


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

Depends on the proto addition above, and on stable device names.

- **Stable device names (prerequisite).** Names are built from `chip_id`
  today (`tt-<chip_id>`; meshes use the origin chip's), and `chip_id` can be
  reordered among devices and does not exist for an ASIC that failed to
  probe. Health reported against a name that can move between chips is
  wrong, and a failed ASIC could not be named at all. Names move to
  `(tray_id, asic_location)`: chips become `tt-<tray>-<location>`, and
  meshes take the tray and location of their origin chip where they take
  its `chip_id` now. Trays keep `tt-tray-<id>`. The breaking change is
  accepted, since nothing is known to depend on the naming, so there is no
  migration path. It reaches checkpoints, CDI device ids and the `chipIDs`
  mesh attribute, so it lands on its own, before the health work.
- **Vendor the new proto** and carry `HostHealth` through
  `fabricmanager.Snapshot`.
- **Keep failed ASICs published.** For each unhealthy `AsicHealth` with no
  matching entry in `asics`, the driver builds a device from the identity
  and enumeration facts, with attributes it cannot know omitted. Prepare
  does not refuse an unhealthy device: a pod that tolerates the taint has
  asked for it. Nothing can be bundled onto a failed MMIO chip, so remote
  ASICs that depended on it are reported unhealthy with it.
- **Health comes from the agent when present.** A snapshot with `health`
  makes each device's health that of its chips, worst first. A snapshot
  without it leaves the Phase 1 behaviour in place, so an older agent still
  works.
- **Meshes** are judged by their chips only until link health exists. The
  existing adjacency gating on `local_connections` stays.
- **Phase 1 tombstones stay.** They are still the only signal for a device
  the agent no longer lists, and for an agent without `health`. They are
  dropped only for devices the agent reports through `health`, which it
  keeps listing.


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
  the MMIO chip still be useful on its own? The other direction is settled:
  the agent reports each remote ASIC's own probe result, and remote ASICs
  behind a failed MMIO chip are unhealthy with it.
