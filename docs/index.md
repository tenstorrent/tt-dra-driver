# Device Allocation (DRA)

The DRA Driver exposes Tenstorrent devices to workloads using Kubernetes
[Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/).
A per-node kubelet plugin discovers devices and publishes them as `ResourceSlice`
objects under the `tenstorrent.com` device class. A pod then requests a device
with a `ResourceClaim`. It is one of the components installed by
[tt-operator](https://docs.tenstorrent.com/tt-operator/).

## Requirements

- Kubernetes 1.33 or later with Dynamic Resource Allocation available on the API
  server and kubelet.
- The [Fabric Manager](https://docs.tenstorrent.com/tt-fabric-manager/) reachable,
  since the plugin resolves devices from topology before publishing them.

## Claim a device

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: tt-claim
spec:
  devices:
    requests:
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
---
apiVersion: v1
kind: Pod
metadata:
  name: tt-workload
spec:
  containers:
    - name: app
      image: <your-image>
      resources:
        claims:
          - name: chip
  resourceClaims:
    - name: chip
      resourceClaimName: tt-claim
```

When the claim is allocated, the device is mounted into the container.

## Verify

```bash
kubectl get resourceslices            # one device entry per discovered device
kubectl get resourceclaim tt-claim -o yaml   # .status.allocation once bound
```

```{note}
The driver only publishes devices once topology resolves. On a host with no
staged fabric topology the `ResourceSlice` set can be empty and claims will not
bind. This is an environment limitation rather than a failure. Allocation
behavior is maturing.
```

```{toctree}
:hidden:
:maxdepth: 1

single-card
```
