# Single-Host Workloads

This page walks through Tenstorrent claim recipes for workloads that run on
**one host**. It covers:

- **n150** — single-chip Wormhole board.
- **n300** — dual-chip Wormhole board (two ASICs on one PCIe card).
- **p150** — single-chip Blackhole board.
- **Wormhole Galaxy (6U UBB)** — 32-chip UBB where every chip is PCIe-MMIO on one host.
- **Blackhole UBB** — same all-MMIO shape as the Wormhole 6U.

The DRA driver publishes one `ResourceSlice` device per MMIO-anchored
bundle. An n300 bundles its non-MMIO remote sibling under the MMIO parent so
a single claim gives the container both chips. A 6U Wormhole Galaxy exposes
every chip via PCIe MMIO, so all 32 surface as individual devices — you claim
them as a set (`count: 32`), not as a single bundle.

```{note}
Multi-host layouts — one host's share of a multi-host Galaxy, or a whole
Galaxy spanning several hosts — are **out of scope** here. Those require
fabric-aware placement (one pod per host with matching `nodeAffinity`) rather
than a single-host ResourceClaim, and are covered separately alongside the
fabric-mesh spec.
```

## How each board appears in a ResourceSlice

Each device carries the same attributes; the values differ per board. Match on
`boardName` for exact board discrimination — `chipArch` + `chipCount` alone
cannot always distinguish a single-board card from a same-arch Galaxy.

| Board                        | `boardName` | `chipArch`  | Bundles per host  | `chipCount` per bundle | Notes                                                                          |
| ---------------------------- | ----------- | ----------- | ----------------- | ---------------------- | ------------------------------------------------------------------------------ |
| **n150**                     | `n150`      | `wormhole`  | 1                 | `1`                    | Single Wormhole ASIC, one MMIO endpoint.                                       |
| **n300**                     | `n300`      | `wormhole`  | 1                 | `2`                    | MMIO ASIC + one bundled remote sibling on the same tray.                       |
| **p150**                     | `p150`      | `blackhole` | 1                 | `1`                    | Single Blackhole ASIC.                                                         |
| **Wormhole Galaxy (6U UBB)** | `galaxy-wormhole`  | `wormhole`  | 32                | `1`                    | All 32 chips are PCIe-MMIO — each surfaces as its own bundle. Claim with `count: 32`. |
| **Blackhole UBB**            | `galaxy-blackhole` | `blackhole` | N (per your board)| `1`                    | Same all-MMIO shape as WH Galaxy; verify per-board with `kubectl get resourceslice`. |

```{note}
Bundling groups *non-MMIO* chips under their MMIO parent on the same tray;
for boards where every chip is MMIO (Wormhole 6U Galaxy), no adoption happens
and each chip is its own `chipCount == 1` bundle. Use `count: N` in the
request to claim the whole board as a set. Verify with
`kubectl get resourceslice -o yaml` — the actual number of bundles a host
exposes is authoritative.
```

Inspect what's actually on your node:

```bash
kubectl get resourceslice -o yaml | \
  yq '.items[].spec.devices[] | {name, attrs: .basic.attributes}'
```

## Attribute reference

Every device in the `tenstorrent.com` DeviceClass carries the attributes below.
Reference them in CEL selectors as `device.attributes["tenstorrent.com"].<name>`.

| Attribute         | Type   | When set     | Value                                                                                    |
| ----------------- | ------ | ------------ | ---------------------------------------------------------------------------------------- |
| `vendor`          | string | always       | Constant `"tenstorrent.com"`.                                                            |
| `chipArch`        | string | always       | ASIC architecture. Currently `"wormhole"` or `"blackhole"`.                              |
| `chipCount`       | int    | always       | Total ASICs a workload gets from this device: the MMIO parent plus bundled remote siblings on the same tray. Match on it to require an n300 (`== 2`) or a single-chip card (`== 1`). |
| `chipID`          | int    | always       | Host-local chip ID of the MMIO parent. Stable across agent restarts on the same host.    |
| `uniqueID`        | string | always       | Globally unique 64-bit ASIC ID of the MMIO parent, rendered as a decimal string.         |
| `trayID`          | int    | always       | Physical tray this bundle belongs to.                                                     |
| `asicLocation`    | int    | always       | Position of the MMIO ASIC within its tray.                                                |
| `boardName`       | string | always       | Human-readable board type: `"n150"`, `"n300"`, `"p150"`, `"p100"`, `"p300"`, `"e75"`, `"e150"`, `"e300"`, `"galaxy"`, `"galaxy-wormhole"`, `"galaxy-blackhole"`, `"quasar"`, or `"unknown"`. Prefer this over `boardType` in selectors. The UBB values match KMD's sysfs `tt_card_type`. |
| `boardType`       | int    | always       | Numeric board-type enum reported by Fabric Manager. Values are not stable across UMD releases; prefer `boardName`.                        |
| `pciAddress`      | string | when known   | PCI address of the MMIO endpoint (e.g. `0000:01:00.0`).                                  |
| `remoteChipIDs`   | string | when bundled | Comma-separated host-local chip IDs of bundled remote siblings.                          |
| `remoteUniqueIDs` | string | when bundled | Comma-separated `uniqueID`s of bundled remote siblings.                                  |

Devices also advertise capacity:

| Capacity | Unit     | Value                                                                                     |
| -------- | -------- | ----------------------------------------------------------------------------------------- |
| `memory` | BinarySI | Aggregate DRAM across every ASIC in the bundle (bundled total, not per-ASIC).             |

```{note}
Attributes are set per MMIO-anchored *bundle*, not per ASIC. For an n300 the
listed `chipID`, `uniqueID`, `trayID`, `asicLocation`, and `pciAddress` refer
to the MMIO parent; the remote sibling's IDs appear in `remoteChipIDs` /
`remoteUniqueIDs`, and its DRAM is folded into the `memory` capacity.
```

## Claim recipes

Each recipe below is a self-contained `ResourceClaim`. Attach it to a Pod by
referencing the claim name in `spec.resourceClaims` and requesting it under a
container's `resources.claims`.

### Any n150

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: any-n150
spec:
  devices:
    requests:
      - name: board
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "n150"
```

### Any n300

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: any-n300
spec:
  devices:
    requests:
      - name: board
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "n300"
```

The pod that holds this claim gets access to both ASICs on the board.

### Any p150

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: any-p150
spec:
  devices:
    requests:
      - name: board
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "p150"
```

### Whole Wormhole Galaxy (6U UBB, single host)

Every chip on the 6U UBB is PCIe-MMIO, so DRA publishes 32 individual
`chipCount == 1` bundles per host. Claim them as a set with `count: 32`:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: whole-wh-galaxy
spec:
  devices:
    requests:
      - name: boards
        exactly:
          deviceClassName: tenstorrent.com
          count: 32
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "galaxy-wormhole"
```

The container ends up with 32 `/dev/tenstorrent/<N>` device nodes, one per
ASIC. Adjust `count` if your host has more than one Galaxy attached.

### Whole Blackhole UBB (single host)

Same pattern as the Wormhole Galaxy recipe, with `boardName == "galaxy-blackhole"`.
Verify the actual per-host bundle count on your board with
`kubectl get resourceslice -o yaml` and set `count` accordingly.

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: whole-bh-ubb
spec:
  devices:
    requests:
      - name: boards
        exactly:
          deviceClassName: tenstorrent.com
          count: 32
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "galaxy-blackhole"
```

### A specific chip by uniqueID

Useful when a workload is pinned to a known ASIC (repro, hardware bring-up):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: pin-to-chip
spec:
  devices:
    requests:
      - name: board
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].uniqueID == "18080982798508798832"
```

## End-to-end example

`ResourceClaim` for any n150, plus a Pod that mounts it. Container runs the
upstream tt-metal test image:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: any-n150
spec:
  devices:
    requests:
      - name: board
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "n150"
---
apiVersion: v1
kind: Pod
metadata:
  name: n150-workload
spec:
  containers:
    - name: app
      image: ghcr.io/tenstorrent/tt-metal/upstream-tests-wh:latest
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: board
  resourceClaims:
    - name: board
      resourceClaimName: any-n150
```

Apply, then confirm allocation and device presence:

```bash
kubectl apply -f n150.yaml

kubectl get resourceclaim any-n150 -o yaml           # .status.allocation once bound
kubectl exec n150-workload -- ls -l /dev/tenstorrent # device node inside pod
kubectl exec n150-workload -- tt-smi                 # if the image ships tt-smi
```

## Multiple single cards in one pod

For a workload that wants two independent single cards on the same host
(e.g. two n150s), issue two claims and reference both from the container:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: { name: n150-a }
spec:
  devices:
    requests:
      - name: board
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "n150"
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: { name: n150-b }
spec:
  devices:
    requests:
      - name: board
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].boardName == "n150"
---
apiVersion: v1
kind: Pod
metadata:
  name: two-n150
spec:
  containers:
    - name: app
      image: <your-image>
      resources:
        claims:
          - { name: card-a }
          - { name: card-b }
  resourceClaims:
    - { name: card-a, resourceClaimName: n150-a }
    - { name: card-b, resourceClaimName: n150-b }
```

For workloads that consume several cards *as a unit*, prefer a single
`ResourceClaim` that requests multiple devices — one request per card — in the
same `devices.requests` list; the scheduler will bind them together.

## Templating with `ResourceClaimTemplate`

For Deployments and StatefulSets, use a `ResourceClaimTemplate` so each pod
replica gets its own claim:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: n150-template
spec:
  spec:
    devices:
      requests:
        - name: board
          exactly:
            deviceClassName: tenstorrent.com
            selectors:
              - cel:
                  expression: |
                    device.attributes["tenstorrent.com"].boardName == "n150"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: n150-service
spec:
  replicas: 3
  selector:
    matchLabels: { app: n150-service }
  template:
    metadata:
      labels: { app: n150-service }
    spec:
      containers:
        - name: app
          image: <your-image>
          resources:
            claims:
              - name: board
      resourceClaims:
        - name: board
          resourceClaimTemplateName: n150-template
```

Each of the three pods gets its own n150 on whatever host has one free.

## Troubleshooting

**Claim stays `Pending` forever.**
- Confirm the driver is running: `kubectl -n tenstorrent-system get pods -l app.kubernetes.io/component=kubelet-plugin`.
- Confirm devices are published: `kubectl get resourceslice`. If empty, the
  driver couldn't resolve topology from FM (see the note in [Requirements](index.md#requirements)).
- Confirm your CEL selector matches something. Grab the attributes from a real
  device and hand-evaluate: `kubectl get resourceslice <name> -o yaml`.

**Wrong device bound (e.g. got an n300 when I wanted an n150, or a Galaxy MMIO chip when I wanted an n150).**
- Prefer `boardName == "n150"` (or `"n300"`, `"p150"`, ...) over selectors
  that mix `chipArch` and `chipCount`. In clusters that mix single-board
  cards with a Wormhole Galaxy, a standalone Galaxy MMIO bundle can present
  as `chipArch == "wormhole"` with `chipCount == 1` — the same as an n150.
  Only `boardName` (or the raw numeric `boardType`) discriminates.

**"Whole Galaxy" claim only bound one chip.**
- Every UBB chip is its own bundle — a single request without `count` binds
  exactly one. Add `count: 32` (or the actual per-host bundle count you see
  in `kubectl get resourceslice -o yaml`) so DRA allocates the full set.

**Pod scheduled but `/dev/tenstorrent` empty.**
- The CDI edits are still evolving. Confirm the driver version and check the
  kubelet-plugin logs for errors when it applied the allocation.
