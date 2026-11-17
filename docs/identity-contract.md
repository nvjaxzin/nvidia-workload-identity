# Identity contract

A DSX JWT-SVID subject (`sub`) is a [SPIFFE](https://spiffe.io/docs/latest/spiffe-about/overview/) URI. Its trust domain identifies the
tenant, region, service, and partition. Its resource path uses complete key/value
pairs with unique keys:

```text
spiffe://<tenant>.<region>.<service>.<partition>/<key>/<value>[/<key>/<value>...]
spiffe://<operator-tenant>.<region>.<service>.<partition>/bm/<node-name>/pod/<pod-uuid>
  [/role/<role>]
```

DPU service identities use:

```text
spiffe://<operator-tenant>.<region>.<service>.<partition>/dpu/<serial>/dpuservice/<service-id>/namespace/<namespace>
```

Exchange preserves path keys and values exactly. For example:

```text
local:     spiffe://<local-domain>/tenant/<tenant>/service/dsx/cluster/cluster-1/namespace/team-a/workload/api
exchanged: spiffe://<tenant>.<region>.dsx.<partition>/cluster/cluster-1/namespace/team-a/workload/api
```

Path keys and values retain their case and order. Each segment starts with an
ASCII letter or digit, followed by ASCII letters, digits, `.`, `_`, or `-`.

Cluster-local [SPIRE](https://spiffe.io/docs/latest/spire-about/) identities encode the same fields under the shard trust
domain. Exchange takes the region and partition from explicit configuration,
the tenant and service from the local subject, and preserves the resource path.

```text
spiffe://<local-trust-domain>/tenant/<tenant>/service/<service>/<key>/<value>[/<key>/<value>...]
```

| Resource | Tenant source | Subject resource path |
|---|---|---|
| DPU | Operator tenant in [DPF](https://github.com/NVIDIA/doca-platform) identity templates | `/dpu/<normalized-serial>` resource prefix |
| DPU-paired operator bare-metal host | `global.operatorTenant` | `/bm/<dpu-machine-id>` |
| Operator bare-metal Kubernetes worker | `global.operatorTenant` | `/bm/<node-name>` |
| Kubernetes Pod | `global.operatorTenant` | `/bm/<node-name>/pod/<pod-uid>` |
| Bare-metal host with a Launch Layer source | `launchlayer.nvidia.com/org-id` annotation | `/bm/<bm-id>` |
| VM with a Launch Layer source | `launchlayer.nvidia.com/org-id` annotation | `/vm/<vm-id>` |

Every host-cluster Kubernetes worker is an operator-owned bare-metal host. DPU
workers in Kamaji clusters use the DPU identity path. The Pod
registration template derives the host ID from the Kubernetes node name, without
a configured worker list or LaunchLayer source. The DPU source supplies the BM
resource ID through its
`carbide.nvidia.com/dpu-machine-id` label.

Every DSX Kubernetes Pod supplies its service label. SPIRE uses its value for
the service path segment. The fixed parent template identifies the worker's
operator-tenant `dsx_imds` agent.

```yaml
metadata:
  labels:
    spiffe.nvidia.com/service: dsx
    spiffe.nvidia.com/role: database  # optional
```

See the [identity chart configuration](../charts/dsx-identity/README.md#configuration)
for the worker registration template and operator tenant settings.

The paired host identity is a controller-derived static entry parented to
the external `dpu_hw` agent identified by the normalized DPU serial. DPUs are
Kubernetes nodes in their Kamaji clusters. The DPU agent's Kubernetes
WorkloadAttestor identifies the IMDS Pod, and [SPIFFE CSI](https://github.com/spiffe/spiffe-csi) exposes the agent's
Workload API socket to that Pod.

The [NVIDIA Workload Identity
Controller](../cmd/dsx-identity-controller/README.md) derives SPIRE registration
entries from DPU and Launch Layer sources.

```yaml
apiVersion: launchlayer.nvidia.com/v1alpha1
kind: VirtualInstance
metadata:
  annotations:
    launchlayer.nvidia.com/org-id: tenant-2
    launchlayer.nvidia.com/role: trainer  # optional
spec:
  instance_id: vm-1234
  node_name: host-1234
status:
  pod_uid: <virt-launcher-pod-uid>
```

Both Launch Layer resource kinds accept the optional
`launchlayer.nvidia.com/role` annotation. A nonempty role appends `/role/<role>`
to the subject. Roles contain 1-63 lowercase letters, digits, underscores or
hyphens and start with a letter or digit. Values are preserved exactly.
An absent or empty role omits the suffix. Invalid roles remove the derived
registration entry until the source is corrected.

For the VM above, the local and exchanged subjects are:

```text
spiffe://<local-domain>/tenant/tenant-2/service/dsx/vm/vm-1234/role/trainer
spiffe://tenant-2.<region>.dsx.<partition>/vm/vm-1234/role/trainer
```

The exchanged `dsx.nvidia.com/resource` claim includes
`{"resource_type":"role","resource_id":"trainer"}` as its final pair.
Relying parties evaluate the tenant and role together.

`BareMetalInstance` uses the same tenant annotation, `spec.instance_id`, and
`status.primary_dpu.machine_id` plus `status.primary_dpu.serial_number`. The
configured operator tenant determines a VM entry's worker parent. The Identity
Controller owns these entries' SPIFFE IDs, selectors, hints, and class names.

## DPU identities

DPF registers the DPU Agent process and opted-in DPUService workloads beneath
the DPU's `spiffe://<local-trust-domain>/spire/agent/dpu_hw/<normalized-serial>`
agent ID. These workload subjects use the `/dpu/<normalized-serial>` resource
prefix. The paired host's `/bm/<dpu-machine-id>` identity is a separate entry
owned by the NVIDIA Workload Identity Controller. A complete tenant
`BareMetalInstance` replaces that operator host entry with
`/bm/<spec.instance_id>` under the assigned tenant.

See the [identity chart configuration](../charts/dsx-identity/README.md#dpf-identity-configuration)
for the DPF templates used to produce these subjects.

## NVIDIA Workload Identity Keys and Event Bus

[NVIDIA Workload Identity Keys](../cmd/dsx-identity-keys/README.md) validates
local SPIRE JWT-SVIDs and issues DSX JWT-SVIDs. By default it publishes public
signing keys to the [Event Bus](https://docs.nvidia.com/dsx-exchange/architecture#dsx-event-bus) and serves aggregate [JWKS](https://www.rfc-editor.org/rfc/rfc7517#section-5). With [NATS](https://docs.nats.io/nats-concepts/jetstream/key-value-store) disabled it
serves only local public signing keys. The external Gateway terminates TLS and
applies source-aware request quotas. Clients configure the Gateway HTTPS
endpoint.

## DSX IMDS host and VM flows

[DSX IMDS](../cmd/metadata-service/README.md) serves the tokenless host and VM
identity API. The [`dsx_imds` NodeAttestor
agent](../cmd/dsx-imds-node-attestor-agent/README.md) and [`dsx_imds`
NodeAttestor server](../cmd/dsx-imds-node-attestor-server/README.md) bootstrap
the host SPIRE agent. The [NVIDIA Workload Identity Keys Proxy CSI
Driver](../cmd/dsx-identity-keys-proxy-csi/README.md) carries VM exchange
requests, and [DSX IMDS Network Setup](../cmd/dsx-imds-network-setup/README.md)
routes the VM's metadata traffic to `169.254.169.254` over IPv4 and
`fd00:76b9::254` over IPv6. It configures the address families present in the
primary [OVN](https://github.com/ovn-kubernetes/ovn-kubernetes) allocation, including IPv4-only, IPv6-only, and dual-stack guests.
