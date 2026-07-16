# Single-Card Workloads (n150, n300, p150)

This page walks through allocating a single Tenstorrent card to a Pod using
Dynamic Resource Allocation. It covers the three common single-board cards:

- **n150** — single-chip Wormhole board.
- **n300** — dual-chip Wormhole board (two ASICs on one tray).
- **p150** — single-chip Blackhole board.

The DRA driver publishes one `ResourceSlice` device per MMIO-anchored bundle,
so each *board* is one device regardless of how many ASICs sit on it. An n300
bundles its remote sibling into the same claim; the container sees both chips
when it holds that one device.

## How each card appears in a ResourceSlice

Each device carries the same attributes; the values differ per board:

| Card      | `chipArch`  | `chipCount` | Notes                                    |
| --------- | ----------- | ----------- | ---------------------------------------- |
| **n150**  | `wormhole`  | `1`         | Single Wormhole ASIC, one MMIO endpoint. |
| **n300**  | `wormhole`  | `2`         | MMIO ASIC + one bundled remote sibling.  |
| **p150**  | `blackhole` | `1`         | Single Blackhole ASIC.                   |

Other attributes on every device: `vendor`, `chipID`, `trayID`, `asicLocation`,
`boardType`, `uniqueID`, `pciAddress`. See the [driver source](../internal/profiles/tenstorrent/tenstorrent.go)
for the full schema.

Inspect what's actually on your node:

```bash
kubectl get resourceslice -o yaml | \
  yq '.items[].spec.devices[] | {name, attrs: .basic.attributes}'
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
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].chipArch == "wormhole" &&
                  device.attributes["tenstorrent.com"].chipCount == 1
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
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].chipArch == "wormhole" &&
                  device.attributes["tenstorrent.com"].chipCount == 2
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
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].chipArch == "blackhole" &&
                  device.attributes["tenstorrent.com"].chipCount == 1
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
      - name: chip
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
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].chipArch == "wormhole" &&
                  device.attributes["tenstorrent.com"].chipCount == 1
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
          - name: chip
  resourceClaims:
    - name: chip
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
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].chipArch == "wormhole" &&
                  device.attributes["tenstorrent.com"].chipCount == 1
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: { name: n150-b }
spec:
  devices:
    requests:
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: |
                  device.attributes["tenstorrent.com"].chipArch == "wormhole" &&
                  device.attributes["tenstorrent.com"].chipCount == 1
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
        - name: chip
          exactly:
            deviceClassName: tenstorrent.com
            selectors:
              - cel:
                  expression: |
                    device.attributes["tenstorrent.com"].chipArch == "wormhole" &&
                    device.attributes["tenstorrent.com"].chipCount == 1
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
              - name: chip
      resourceClaims:
        - name: chip
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

**Wrong device bound (e.g. got an n300 when I wanted an n150).**
- Check `chipCount` on the CEL expression. n150 = 1, n300 = 2.
- If you have a mix of Wormhole and Blackhole in the same cluster, pin
  `chipArch` too.

**Pod scheduled but `/dev/tenstorrent` empty.**
- The CDI edits are still evolving. Confirm the driver version and check the
  kubelet-plugin logs for errors when it applied the allocation.
