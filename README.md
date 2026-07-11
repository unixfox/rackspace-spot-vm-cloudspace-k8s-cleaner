# rackspace-spot-vm-cloudspace-k8s-cleaner

A small Kubernetes controller that cleans up **stale k3s nodes** whose backing
**Rackspace Spot VM** (from a [VM Cloudspace][spot-vm-docs]) has disappeared.

Rackspace Spot VMs are ephemeral/spot-priced — they can be preempted at any
time. When a VM vanishes, its corresponding k3s node stays in the cluster as a
`NotReady` orphan, polluting `kubectl get nodes` and confusing schedulers and
autoscalers. This controller removes those orphaned node objects.

## How it works

Every `CLEAN_INTERVAL` (default `60s`), the cleaner:

1. Calls the [Rackspace Spot Go SDK][sdk] (`ListVMCloudSpaces`) for your
   organization (`SPOT_ORG`) and builds the set of **alive VMs per pool** from
   each VM Cloudspace's `AssignedServers` (keyed by `NodePoolName`).
2. Lists k3s nodes carrying the `rackspace-spot/vm-pool-name` label
   (configurable via `SPOT_VM_POOL_LABEL`).
3. A node is a **deletion candidate** when:
   - its backing Spot VM is **no longer in the alive set** for that node's pool,
     **and**
   - the node's `Ready` condition is **not `True`** (`NotReady`).
4. A candidate is only deleted after `GRACE_TICKS` (default `2`, ≈ 2 minutes)
   consecutive "gone + NotReady" observations, to survive transient Spot API
   hiccups or momentary VM-report lag.
5. If a `ListVMCloudSpaces` call fails, the whole tick is skipped (no
   deletions, grace counters untouched) to avoid wrongful deletes.
6. `DELETE` the node object only — no pod drain/eviction (the VM is already
   gone, so its pods are already lost; draining would just slow things down and
   could hang on PodDisruptionBudgets).

Only nodes with the `rackspace-spot/vm-pool-name` label are ever considered, so
non-Rackspace-Spot nodes (control plane, on-prem, other-cloud workers) are
never touched.

## Matching a k3s node to a Spot VM (`MATCH_BY`)

The cleaner must know which alive Spot VM backs a given k3s node. Two modes,
selected by the `MATCH_BY` env var:

### `MATCH_BY=name` (default)

The k3s **node name** equals the Spot **VM server name** (the map key in
`VMCloudSpace.AssignedServers`, also `VMAssignedServer.ServerName`).

This is the cleanest mode. Rackspace Spot commonly sets the VM hostname to its
server name at boot; since k3s derives the node name from the hostname by
default, you often get matching node names for free. If yours do not, set the
hostname (and thus `K3S_NODE_NAME`) explicitly in cloud-init:

```yaml
#cloud-config
runcmd:
  - NODE_NAME=$(curl -s http://169.254.169.254/latest/meta-data/hostname || hostname -s)
  - echo "$NODE_NAME" > /etc/hostname
  - hostname "$NODE_NAME"
  # then start k3s agent with --node-name "$NODE_NAME"
```

### `MATCH_BY=ip`

The k3s node's **InternalIP** address equals the Spot VM's
`VMAssignedServer.IPAddress`. Use this if you cannot make the node name match
the VM server name. Requires only the pool label on the node (see below); the
VM always knows its own IP.

## Required cloud-init labels

Every Spot VM that joins your k3s cluster **must** be labeled with the VM pool
name so the cleaner can (a) scope to spot nodes and (b) narrow the alive-VM
check to the right pool. Set it via the k3s agent `--node-label` flag in your
cloud-init `runcmd`:

```yaml
#cloud-config
runcmd:
  - |
    curl -sfL https://get.k3s.io | K3S_URL=https://<your-k3s-server>:6443 \
      K3S_TOKEN=<your-k3s-token> sh -s - agent \
      --node-label rackspace-spot/vm-pool-name=<vm-pool-name>
```

Replace `<vm-pool-name>` with the **VMPool name** in Rackspace Spot (the same
name reported as `VMAssignedServer.NodePoolName`). If you run several pools,
each pool's cloud-init sets its own pool name.

> Without this label, a node is invisible to the cleaner and will **not** be
> removed even if its VM disappears.

## Configuration reference

| Env var | Default | Required | Description |
|---|---|---|---|
| `SPOT_REFRESH_TOKEN` | — | yes | Rackspace Spot refresh token. |
| `SPOT_ORG` | — | yes | Rackspace Spot organization name (single org). |
| `SPOT_BASE_URL` | `https://spot.rackspace.com` | no | Spot API base URL. |
| `SPOT_AUTH_URL` | `https://login.spot.rackspace.com` | no | Spot OAuth base URL. |
| `SPOT_VM_POOL_LABEL` | `rackspace-spot/vm-pool-name` | no | Node label whose presence marks a spot node; its value is the pool name. |
| `MATCH_BY` | `name` | no | Node↔VM match: `name` or `ip`. |
| `CLEAN_INTERVAL` | `60s` | no | Reconcile ticker interval. |
| `GRACE_TICKS` | `2` | no | Consecutive "gone + NotReady" ticks before delete. |
| `DRY_RUN` | `false` | no | If true, log what would be deleted but do not delete. Recommended for first rollout. |

## Deployment

1. **Build and push** the image (sample multi-stage `Dockerfile`):

   ```dockerfile
   FROM golang:1.23 AS build
   WORKDIR /src
   COPY . .
   RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/cleaner .

   FROM gcr.io/distroless/static-debian12:nonroot
   COPY --from=build /out/cleaner /cleaner
   ENTRYPOINT ["/cleaner"]
   ```

   ```bash
   docker build -t ghcr.io/yourorg/spot-vm-cleaner:v0.1.0 .
   docker push ghcr.io/yourorg/spot-vm-cleaner:v0.1.0
   ```

2. **Create the refresh-token Secret** (copy `deploy/secret.example.yaml` to
   `deploy/secret.yaml`, edit in your token):

   ```bash
   kubectl apply -f deploy/secret.yaml
   ```

3. **Edit `deploy/deployment.yaml`**:

   - set `SPOT_ORG` to your Spot org,
   - set the container `image` to your pushed image,
   - (optional) set `DRY_RUN=true` for the first rollout.

4. **Apply the manifests**:

   ```bash
   kubectl apply -f deploy/serviceaccount.yaml \
                 -f deploy/clusterrole.yaml \
                 -f deploy/clusterrolebinding.yaml \
                 -f deploy/deployment.yaml
   ```

5. **Watch the logs** (with `DRY_RUN=true` first to confirm the right nodes
   would be deleted):

   ```bash
   kubectl -n kube-system logs -f deploy/spot-vm-cleaner
   ```

   Once you are satisfied, flip `DRY_RUN=false` and roll the Deployment.

## RBAC

The cleaner's ServiceAccount is granted exactly:

- `nodes`: `get`, `list`, `watch`, `delete`

(See `deploy/clusterrole.yaml`.) It does not read Secrets, ConfigMaps, Pods,
or anything else — least privilege.

## Local build & test

```bash
go mod tidy
go build ./...
go vet ./...
gofmt -l .
go test ./...
```

## Dependencies

- [rackspace-spot/spot-go-sdk][sdk] v0.2.0 — Rackspace Spot Go SDK.
- [`k8s.io/client-go`][client-go] v0.31.x — Kubernetes client (in-cluster, ServiceAccount auth).

## Troubleshooting

- **No nodes ever deleted**: confirm your Spot VM nodes carry the
  `rackspace-spot/vm-pool-name` label with a value matching the VMPool name in
  Spot; confirm `MATCH_BY` matches how you set node names/IPs; run with
  `DRY_RUN=false` after seeing candidates in logs.
- **Wrong nodes at risk**: if `ListVMCloudSpaces` returns empty on a transient
  API error, the tick is skipped (no deletes). If you see repeated "reconcile
  tick skipped due to error" logs, verify `SPOT_REFRESH_TOKEN`/`SPOT_ORG` and
  Spot platform connectivity.

[spot-vm-docs]: https://spot.rackspace.com/docs/en/virtual-machines
[sdk]: https://github.com/rackspace-spot/spot-go-sdk
[client-go]: https://pkg.go.dev/k8s.io/client-go