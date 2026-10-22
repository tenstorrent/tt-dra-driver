# Publishing PGD meshes as DRA devices

Status: proposal. Nothing here is implemented yet.

## Problem

The driver advertises Tenstorrent hardware at two granularities: one device
per MMIO-anchored chip bundle, and one device per physical tray. Both are
derived from what the fabric manager agent reports about the host, and the
two are kept mutually exclusive with ResourceSlice shared counters.

Neither granularity is what a multi-chip workload actually asks for. A
runtime wants a *mesh* — a set of chips wired into a specific rectangular
topology, in a specific order, because the ordering is the mesh coordinate
system its MGD is built on. On a Blackhole Galaxy rev C a tray is a 2x4 of
ASICs, and the interesting meshes cut across trays: `4x2_Mesh_horizontal`
takes a 2x2 quadrant from one tray and the quadrant physically beneath it on
another.

Which combinations are valid is not something the driver should invent. It is
already written down, per platform, in the Physical Grouping Descriptors the
fabric manager selects from the PSD. The end state is that those descriptors
drive what the driver publishes. The first step is to hardcode one of them
and prove the device model can carry it.

## What we have to build on

`fm-proto/topology.proto` already carries everything the first phase needs,
and the driver reads almost none of it:

| Field | Today | Needed for |
| --- | --- | --- |
| `AsicInfo.tray_id`, `asic_location` | published as attributes | matching ASICs against PGD instances |
| `HostTopologyQuirks.is_bh_glx_rev_c` | unused | gating rev-C mesh definitions |
| `HostPhysicalTopology.local_connections` | unused | confirming a mesh is physically wired as declared |
| `HostPhysicalTopology.exit_nodes` | unused | multi-host meshes (out of scope, see below) |

The device model in `internal/profiles/tenstorrent/tenstorrent.go` already
has the right shape: an `allocatableUnit` is an ordered list of host bundles,
`ApplyConfig` emits one character device per MMIO chip in the unit, and
composite devices are kept exclusive with their constituents through shared
counters. A mesh is another composite over the same machinery.

### API limits that bound the design

| Constant | Value | Consequence |
| --- | --- | --- |
| `ResourceSliceMaxCountersPerCounterSet` | 32 | a 32-chip galaxy host fits in exactly one counter set |
| `ResourceSliceMaxDeviceCounterConsumptionsPerDevice` | 2 | **a composite device may span at most two counter sets** |
| `ResourceSliceMaxCountersPerDeviceCounterConsumption` | 32 | a composite may cover 32 chips per set it touches |
| `ResourceSliceMaxDevicesWithAdvancedFeatures` | 64 | per *slice*, not per pool; slices are already chunked |
| `ResourceSliceMaxAttributesAndCapacitiesPerDevice` | 32 | the chip list must be one CSV string, not an attribute per chip |

The two-consumptions limit is the one that constrains counter layout, and it
is why counter packing is a design decision rather than an implementation
detail.

## What `4x2_Mesh_horizontal` resolves to

The PGD describes the host as a 2x2 of trays — `tray_2` and `tray_4` on the
top row, `tray_1` and `tray_3` below — and defines `4x2_Mesh_horizontal` as a
HALFTRAY_2 quadrant stacked on a HALFTRAY_1 quadrant. Reading those together:
a horizontal 4x2 pairs a quadrant of tray 2 with the column-aligned quadrant
of tray 1 beneath it, and likewise tray 4 with tray 3. That makes the pairing
index-order within each group, and yields four meshes per host:

| Mesh | Top half (HALFTRAY_2) | Bottom half (HALFTRAY_1) |
| --- | --- | --- |
| 0 | tray 2, locations 1, 2, 5, 6 | tray 1, locations 5, 6, 1, 2 |
| 1 | tray 2, locations 3, 4, 7, 8 | tray 1, locations 7, 8, 3, 4 |
| 2 | tray 4, locations 4, 3, 8, 7 | tray 3, locations 8, 7, 4, 3 |
| 3 | tray 4, locations 2, 1, 6, 5 | tray 3, locations 6, 5, 2, 1 |

The order of the eight entries is the row-major mesh coordinate order, not an
arbitrary set — it is part of what the device has to publish.

### Assumptions to verify before coding

Each of these would invalidate the table above, and none can be settled from
the repository alone:

1. **Tray reporting.** Does the agent report `tray_id` 1-4 with eight ASICs
   each on a Blackhole Galaxy? `docs/single-host.md` currently states that a
   6U Galaxy is *one* tray of 32 chips, which contradicts the PGD's
   TRAY_1..TRAY_4. One of the two is wrong.
2. **`asic_location` base.** The PGD enum reads `ASIC_LOCATION_1`..`_8`; the
   proto field is a plain `uint32`. Whether the wire values are 1-based or
   0-based decides whether the table is used as written or shifted.
3. **MMIO coverage.** Are all 32 chips `is_mmio_capable` on rev C? If some
   are remote, the atomic unit is a bundle rather than a chip, and a
   quadrant-granular mesh may not be representable at all — the bundling in
   `bundleHostASICs` gives every remote chip on a tray to one MMIO parent.
4. **Quadrant pairing.** The inference that HALFTRAY_2 instance *i* pairs
   with HALFTRAY_1 instance *i*. Checkable against `local_connections` on a
   real host, which is also how phase 2 validates itself at runtime.

## Phase 1 — per-chip counters

Prerequisite for everything else. The current scheme gives each tray one
counter sized to its chip count, with a chip consuming one unit and the tray
device consuming all of them. That cannot express a mesh: consuming four
units of tray 2's counter reserves *four of its eight* chips, not *which*
four, so the scheduler would happily hand a mesh and one of the chips inside
it to two different workloads.

Replace it with one counter per chip device, value 1. Every composite —
tray, mesh, anything later — consumes one unit of each chip it covers.
Exclusion in all directions (chip/tray, chip/mesh, mesh/mesh, tray/mesh)
then falls out of the same arithmetic, and the tray device stops being a
special case.

Counter packing, given the two-consumptions limit:

- Fill counter sets in chip-id order, so a tray's chips stay within one set.
- One set covers a 32-chip galaxy host exactly; a composite there always
  costs one consumption entry.
- A composite that would span more than two sets is not publishable. Skip it
  and log it rather than publishing a device whose exclusion the apiserver
  will not carry.
- Hosts past roughly 64 chip devices fall back to chips and trays only.

This phase changes no observable behaviour beyond counter naming; the
existing allocator tests should pass with the names updated.

## Phase 2 — hardcoded `4x2_Mesh_horizontal`

Publish the four meshes from the table above, gated on
`quirks.is_bh_glx_rev_c`, a `galaxy-blackhole` board type, and all four trays
being present. Anything else publishes no mesh devices at all.

**Device model.** A third `unit` attribute value, `mesh`, alongside `chip`
and `tray`. A `mesh.<driverName>` DeviceClass in the chart with
`extendedResourceName: tenstorrent.com/mesh`, selecting on `unit == 'mesh'`
the way the tray class already does. A `--enable-mesh-devices` flag
(`kubeletPlugin.meshDevices` in the chart) on by default, since the rev-C
gate already keeps it off everywhere it does not apply.

**Attributes.** `meshName: "4x2_Mesh_horizontal"`, `meshShape: "4x2"`,
`chipCount: 8`, `trayIDs: "1,2"`, and ordered `chipIDs` / `uniqueIDs` CSVs in
mesh order. Capacity carries the summed memory. The per-device attribute cap
is why the chip list is a string rather than structured. Name the device from
the lowest covered chip id so it is stable across topology churn.

`ApplyConfig` needs no new machinery: an `allocatableUnit` already holds an
ordered bundle list and emits one `/dev/tenstorrent/N` per MMIO chip.

**Runtime validation.** Before publishing a mesh, confirm its eight chips are
wired as the declared 4x2 in `local_connections`. If they are not, log
loudly and skip that mesh. This keeps a mis-cabled or rev-B host from
receiving a device that claims a topology it does not have, and it is the
first piece of the phase 3 resolver.

**Tests.** A 32-ASIC rev-C fake topology driven through the existing
allocator harness: four meshes published with the right chip sets *and*
order; exclusion proven in both directions against chips and against trays;
two disjoint meshes co-allocatable; nothing published when the quirk flag is
absent.

**Docs.** A section and claim recipe in `docs/single-host.md`, and a fix for
the "one tray of 32 chips" statement once assumption 1 is settled.

## Phase 3 — PGDs as configuration

### Where the descriptor comes from

Three options, in the order they should happen:

1. **Compiled in** — what phase 2 does. Fine for one platform, does not
   scale, and duplicates knowledge the fabric manager already has.
2. **Chart-shipped ConfigMap** — PGDs per platform mounted into the
   DaemonSet, selected by quirks and board type. Needs no fabric manager
   change and the chart already owns driver configuration, so this is the
   next step.
3. **Served by the fabric manager agent** — the end state. The agent already
   picks the right PGD from the PSD via `is_bh_galaxy_rev_c()`; anything else
   is a second source of truth that will drift from the first. Needs a
   topology proto addition, so it is worth starting the conversation with the
   fabric manager team early even though the work lands last.

### The work

- Get the `groupings` schema as a `.proto` from the fabric manager team,
  vendor it under `fm-proto/`, and parse the text format with `prototext`.
- Resolve `custom_type` grouping definitions against discovered ASICs by
  `(tray_id, asic_location)`; drop any grouping whose members are not all
  present on this host.
- Expand `grouping_ref` instances into concrete combinations.

The hard part is exactly what phase 2 hardcodes: the PGD never states which
HALFTRAY_2 instance combines with which HALFTRAY_1 instance. Derive it from
the `local_connections` graph — emit only those combinations that physically
form the declared shape — and add an explicit-enumeration escape hatch in the
descriptor for cases the graph cannot disambiguate. Phase 2's four meshes
become the resolver's test oracle: the generic path must reproduce them
exactly, which is the main reason to hardcode them first.

### Scope limits

- **Nothing at or above the HOSTS grouping.** `4x8_Mesh`, `8x8_Mesh`,
  `4x16_Mesh` and friends span hosts. A node-local DRA driver cannot allocate
  across nodes; that needs cluster-scoped allocation and is a separate
  project, not a later phase of this one.
- **Publish a subset, not everything.** A full PGD expansion on a galaxy
  produces a large number of overlapping composites, each adding counter
  arithmetic for the scheduler. The chart should carry an allowlist of
  grouping names to publish so a cluster opts into what it needs.

## Open questions

- Phase 2 publishes four mesh devices per host on top of 32 chips and 4
  trays. What does the scheduler's counter arithmetic cost once a full PGD
  expansion multiplies that? Worth measuring before phase 3 picks defaults.
- Does any consumer need the mesh coordinate of each chip explicitly, or is
  the published order enough to reconstruct it from `meshShape`?
- Should a mesh device expose `exit_nodes` information so a workload can tell
  whether its mesh has off-host links available?
