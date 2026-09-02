# tt-dra-driver

## Overview

tt-dra-driver is a Kubernetes [Dynamic Resource Allocation (DRA)][dra] driver
for Tenstorrent devices. It advertises Tenstorrent accelerators (Wormhole,
Blackhole, and Galaxy systems) to the cluster as `ResourceSlice` devices under
the `tenstorrent.com` DeviceClass, and injects allocated devices into Pods
through the Container Device Interface (CDI). It follows the structure and
practices established by the upstream
[`kubernetes-sigs/dra-example-driver`][example-driver].

The driver does not probe hardware itself. Its kubelet plugin runs as a
DaemonSet and calls the `GetTopology` remote procedure call (RPC) of the
[Tenstorrent Fabric Manager (TTFM)][ttfm] agent on the same node to discover
the application-specific integrated circuits (ASICs) present, then publishes
one device per ASIC that has a memory-mapped I/O (MMIO) path to the host.
Remote ASICs reachable only through another chip on the same tray are grouped
under their MMIO-capable parent. Each allocated device receives a CDI edit
that exposes its `/dev/tenstorrent/<N>` node, and a driver-wide CDI spec
mounts the 2 MiB and 1 GiB hugepage filesystems the Tenstorrent user-mode
driver requires.

> [!NOTE]
> The Fabric Manager agent must be running on every node before the DRA
> driver can publish devices. See the [TTFM documentation][ttfm] for
> installation.

## Architecture

The DRA kubelet plugin DaemonSet connects to the Fabric Manager agent's local
discovery endpoint to enumerate devices and register `ResourceSlice` objects.

![Diagram of the tt-dra-driver kubelet plugin discovering devices through the Fabric Manager agent and publishing ResourceSlices](img/dra-diagram.png)

## Getting started

Create a `ResourceClaim` targeting a specific device by its unique ID, for
example:

```yaml
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: onechip
spec:
  devices:
    requests:
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
          selectors:
            - cel:
                expression: 'device.attributes["tenstorrent.com"].uniqueID == "18080982798508798832"'
```

Create a Pod that references the claim. This example uses a tt-metal
container image:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: onechip-pod
spec:
  containers:
    - name: ctr
      image: ghcr.io/tenstorrent/tt-metal/upstream-tests-bh:v0.72.0-dev20260519-42-gee154fec28d
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: onechip
  resourceClaims:
    - name: onechip
      resourceClaimName: onechip
```


Devices published by the driver carry the attributes `boardName`,
`chipCount`, `uniqueID`, `remoteChipIDs`, and `pciAddress`, and a `memory`
capacity, all in the `tenstorrent.com` attribute domain. Any of them can be
used in a Common Expression Language (CEL) selector. See
[docs/single-host.md](docs/single-host.md) for a complete walkthrough on a
single-node cluster, and the published documentation at
<https://docs.tenstorrent.com/tt-dra-driver/>.

## Repository layout

```
.
├── cmd/
│   └── tt-dra-driver-kubeletplugin/   # Kubelet plugin entrypoint and main loop
│       ├── main.go, driver.go, ...
├── deployment/
│   ├── flux.yaml                      # Flux deployment manifest
├── docker/
│   ├── Dockerfile                     # Production Dockerfile
│   ├── Dockerfile.devel               # Dev/builder image for `make docker-*`
│   └── Makefile                       # Helper Makefile for docker builds
├── docs/                              # Sphinx documentation site sources
├── fm-proto/                          # Fabric Manager protobuf definitions (source for gen-proto)
├── helm/
│   └── tt-dra-driver/
│       ├── Chart.yaml, values.yaml    # Helm chart for cluster-side install
│       └── templates/                 # Helm templates (RBAC, DaemonSet, DeviceClass)
├── internal/
│   ├── fabricmanager/                 # Tenstorrent Fabric Manager gRPC client
│   │   ├── client.go
│   │   └── proto/
│   │       ├── agent/agent.pb.go, ...
│   │       └── topology/topology.pb.go
│   └── profiles/                      # Pluggable device profiles
│       ├── profiles.go
│       └── tenstorrent/tenstorrent.go # Default profile (TTFM-driven discovery)
├── pkg/
│   └── flags/                         # Shared CLI flag groups (kubeclient, logging)
│       ├── kubeclient.go
│       └── logging.go
├── Makefile / common.mk               # Build entrypoints
├── go.mod, go.sum                     # Module definition and deps
└── README.md
```

## Prerequisites

* Go 1.26+
* GNU Make 3.81+
* Docker 20.10+ (with buildx) or Podman 4.9+
* Helm v3.7.0+, only required to install the chart
* A Kubernetes cluster with the DRA feature gates enabled (v1.33+)
* The Tenstorrent Fabric Manager agent running on each node with Tenstorrent
  devices

## Building

Build the binary on the host:

```bash
make binaries
# produces ./tt-dra-driver-kubeletplugin (or under $PREFIX when set)
```

Build the container image (matches what the Helm chart deploys):

```bash
make -f docker/Makefile ubuntu22.04
```

Run the standard checks and tests:

```bash
make check  # fmt, vet, lint, etc.
make test
```

Regenerate the Go code for the Fabric Manager protobuf definitions in
`fm-proto/` after changing them (requires `protoc`, `protoc-gen-go`, and
`protoc-gen-go-grpc`):

```bash
make gen-proto
```

All of the above can also be executed inside the dev image with `make
docker-<target>` (see the `Makefile` for the full target list).

## Installing the Helm chart

Released images and charts are published to the GitHub Container Registry.
Install the published chart with:

```bash
helm upgrade --install \
  --create-namespace \
  --namespace tt-dra-driver \
  tt-dra-driver \
  oci://ghcr.io/tenstorrent/helm/tt-dra-driver
```

To install a locally built image instead, push it to a registry reachable by
the cluster and install the chart from this repository:

```bash
helm upgrade --install \
  --create-namespace \
  --namespace tt-dra-driver \
  --set image.repository=<your-registry>/tt-dra-driver \
  --set image.tag=<tag> \
  tt-dra-driver \
  helm/tt-dra-driver
```

The chart deploys a privileged DaemonSet (the kubelet plugin), a
ServiceAccount, ClusterRole, and ClusterRoleBinding for role-based access
control (RBAC), and a `DeviceClass` named `tenstorrent.com`. By default the
DaemonSet is scheduled only onto nodes labeled `tenstorrent.com/has-tt=true`
or carrying the Node Feature Discovery label for the Tenstorrent PCI vendor
ID. See `helm/tt-dra-driver/values.yaml` for the full set of options,
including the Fabric Manager agent address.

## Roadmap

The following items are not yet implemented and are planned for subsequent
changes:

* `ResourceClaim` opaque configuration (sharing modes, performance tiers, ...)
* Validating admission webhook
* End-to-end test suite

## Contributing

Contributions are welcome. Report bugs and request features through
[GitHub Issues](https://github.com/tenstorrent/tt-dra-driver/issues), and
submit bug fixes and new functionality as pull requests. Pull requests are
reviewed weekly. See [CONTRIBUTING.md](CONTRIBUTING.md) for the development
workflow and requirements, and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for
community expectations. To report a security vulnerability, follow
[SECURITY.md](SECURITY.md).

## License

- [LICENSE](LICENSE): Apache License 2.0, the overall license for this project,
  except where specified.
- [LICENSE-DOCS](LICENSE-DOCS): Creative Commons Attribution 4.0 International,
  the license for all documentation and images only.
- [LICENSE_understanding.txt](LICENSE_understanding.txt): Tenstorrent's
  clarification of how the Apache License 2.0 applies to this project.
- [NOTICE](NOTICE): copyright notice and third-party attributions, including
  the portions of this project derived from `kubernetes-sigs/dra-example-driver`.

[dra]: https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/
[example-driver]: https://github.com/kubernetes-sigs/dra-example-driver
[ttfm]: https://docs.tenstorrent.com/tt-fabric-manager/
