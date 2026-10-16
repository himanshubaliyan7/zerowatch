# ZeroWatch Helm chart

Deploys ZeroWatch on Kubernetes: the server (one binary, five roles), an optional node agent DaemonSet, and an optional model forecaster.

```
deploy/helm/
  verify.sh                 lint + helm template + kubeconform, all in Docker
  kind-e2e.sh               install both modes in a throwaway kind cluster
  zerowatch/
    Chart.yaml  values.yaml  values.schema.json  README.md
    templates/
      _helpers.tpl          names, labels, shared env (every ZW_* setting), probes, seeds
      single.yaml           mode=single: Deployment (role=all) + PVC
      storage.yaml          mode=cluster: StatefulSet (role=storage) + headless Service + PDB
      stateless.yaml        mode=cluster: ingest and query Deployments + optional HPAs
      services.yaml         UI/API, ingest (OTLP/Prom/HTTP), StatsD (UDP), single-mode gossip
      ingress.yaml          UI/API on /, ingest paths to the ingest service
      agent-daemonset.yaml  node agent (role=supervisor)
      forecaster.yaml       model sidecar Deployment + Service
      networkpolicy.yaml  servicemonitor.yaml  secret.yaml  serviceaccount.yaml  NOTES.txt
      tests/test-connection.yaml   `helm test`: GET /readyz
```

The image comes from `deploy/docker/Dockerfile`: a static, `-tags ebpf` binary with the UI embedded, on distroless (non-root, about 29 MB).

```bash
docker build -f deploy/docker/Dockerfile -t registry.example.com/zerowatch:0.4.0 .
```

## Modes

| | `mode: single` (default) | `mode: cluster` |
|---|---|---|
| Server | 1 Deployment, `role=all`, `Recreate` | StatefulSet `role=storage` (default 3), Deployments `role=ingest` (2) and `role=query` (1) |
| Data | PVC at `/data` (the memory engine keeps samples in RAM; the PVC holds SQLite metadata) | PVC per storage pod; series spread on the hash ring |
| Gossip | only when the agent is on (the pod seeds itself) | memberlist on 7946 TCP+UDP; seeds = every storage pod's DNS name via the headless service |
| UI/API service | the pod | query pods |
| Ingest service | the pod | ingest pods |

Services: `<release>-zerowatch` (UI/API, 8080), `<release>-zerowatch-ingest` (8080 for `/cloudwatch` and native ingest, 4318 OTLP, 8428 Prom remote write), `<release>-zerowatch-statsd` (8125/UDP).

Probes: liveness `GET /healthz`, readiness `GET /readyz` (pings the metadata store). Both bypass API-key auth.

## Examples

Single node, dev:

```bash
helm install zw deploy/helm/zerowatch -n zw --create-namespace \
  --set image.repository=registry.example.com/zerowatch --set image.tag=0.4.0
kubectl -n zw port-forward svc/zw-zerowatch 8080:8080
```

Cluster with the node agent, a secret and shared Postgres metadata:

```bash
kubectl -n zw create secret generic zw-secrets \
  --from-literal=apiKeys='a-very-long-api-key-0001:1' \
  --from-literal=clusterSecret="$(openssl rand -hex 24)" \
  --from-literal=metadataDSN='postgres://zw:pw@postgres:5432/zerowatch'
helm install zw deploy/helm/zerowatch -n zw \
  --set mode=cluster --set storageNodes.replicas=3 \
  --set agent.enabled=true \
  --set secrets.existingSecret=zw-secrets
```

Kafka commit log, forecaster, ingress:

```yaml
mode: cluster
kafka:
  enabled: true
  brokers: ["redpanda-0.redpanda.kafka.svc:9092"]
forecaster:
  enabled: true
  model: chronos-bolt-small     # image zerowatch-forecaster:chronos-bolt-small
ingress:
  enabled: true
  className: nginx
  hosts: [{ host: zerowatch.example.com }]
  tls: [{ secretName: zerowatch-tls, hosts: [zerowatch.example.com] }]
```

External ClickHouse (keep one storage replica, see below):

```yaml
storage:
  backend: clickhouse
  clickhouse: { url: "http://clickhouse.db.svc:8123", database: zerowatch }
secrets: { existingSecret: zw-secrets }   # keys clickhouseUser, clickhousePassword
```

## Secrets

`secrets.existingSecret` names a Secret you manage. Every key is optional and maps to an env var (`secrets.keys` renames the keys):

| Key | Env | Use |
|---|---|---|
| `apiKeys` | `ZW_API_KEYS` | `key:tenant,...`; empty means auth is off |
| `awsCredentials` | `ZW_AWS_CREDENTIALS` | `AKID:SECRET:tenant,...`, SigV4 on `/cloudwatch` |
| `clusterSecret` | `ZW_CLUSTER_SECRET` | protects `/internal/v1/*` between nodes |
| `metadataDSN` | `ZW_METADATA_DSN` | `postgres://...` (overridden by `metadata.dsn`) |
| `clickhouseUser`, `clickhousePassword` | `ZW_CLICKHOUSE_USER/PASSWORD` | |
| `reportTargets` | `ZW_REPORT_TARGETS` | `slack:<url>,webhook:<url>,log` |
| `awsAccessKeyId`, `awsSecretAccessKey`, `awsSessionToken` | `AWS_*` | SNS alarm actions |

Without `existingSecret` the chart creates `<release>-zerowatch` from `secrets.values` (dev only) and **generates `clusterSecret`** when none is given, keeping it across upgrades via `lookup`. A cluster secret is required whenever the agent runs. Member-IP auth fails for hostNetwork agents: a pod sees the node's pod-CIDR gateway as the source address, not the node IP the agent gossips. In kind every agent write came back `403 not a cluster member`. With `existingSecret`, put `clusterSecret` in it yourself.

## Node agent (`agent.enabled`)

A DaemonSet running `--role=supervisor` on every node (tolerates all taints by default). It discovers services, attaches the eBPF probe or the `/proc` fallback, tails logs, samples host metrics (`ZeroWatch/Host`), and builds dashboards and alarms. It joins the gossip ring with the storage seeds, so its writes go straight to the ring owners (or to Kafka when `kafka.enabled`).

- `hostPID: true`, so it sees every process. `hostNetwork: true` by default, so topology and the `/proc` probe see the node's sockets and the Host dimension is the node's hostname. On the host network it listens on `agent.httpPort` 18080 and `agent.gossipPort` 17946 so it doesn't clash with node ports.
- Mounts (read-only): `/proc` → `/host/proc`, `/sys` → `/host/sys`, `/` → `/host` (statfs for disk usage, `--host-root=/host`), `/sys/kernel/tracing`, `/sys/kernel/debug`, `/var/log` (covers `/var/log/pods` and `/var/log/containers`). `mounts.dockerContainers` adds `/var/lib/docker/containers`.
- The gossip member name (`ZW_NODE_NAME`) is the pod name by default (`agent.memberName: pod`); `node` uses `spec.nodeName`. The `Host` dimension is the hostname either way: the node's with hostNetwork, the pod's without (there is no `--hostname` flag yet).
- It runs as uid 0 with a read-only root filesystem and drops all capabilities except `agent.capabilities`.

**eBPF needs:** Linux 5.8+ with BTF (`/sys/kernel/btf/vmlinux`), and:
- `CAP_BPF` and `CAP_PERFMON` to load programs and attach tracepoints;
- `CAP_SYS_RESOURCE` for the memlock limit on older kernels;
- `CAP_SYS_PTRACE` to read other processes' `/proc/<pid>/fd` (topology);
- tracefs mounted on the node at `/sys/kernel/tracing` (or debugfs at `/sys/kernel/debug`).

Some runtimes and kernels need more; `agent.privileged: true` is the fallback. When any of this is missing the agent logs a warning and uses the `/proc` probe (connection-level metrics, not true RED). See `ebpf/README.md`.

## Security defaults

- Server and forecaster pods run as uid 65532, non-root, with a read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp, and no service-account token. `/data` and `/tmp` are volumes.
- `networkPolicy.enabled` allows the UI/API and ingest ports from `networkPolicy.ingressFrom` (anyone when empty), and gossip and internal RPC only from this release's pods plus `networkPolicy.nodeCIDRs`. Add the node CIDRs when the agent runs on the host network.
- `serviceMonitor.enabled` renders a ServiceMonitor, but ZeroWatch has no Prometheus `/metrics` endpoint yet.

## Gossip start-up (`seedWait`)

ZeroWatch resolves and joins its `--cluster-seeds` once, at start-up, and doesn't retry. Pods that start together race the seed DNS records, and CoreDNS caches the early NXDOMAIN answers (`cache 30`). Storage, ingest, query and agent pods therefore run a `wait-for-seeds` busybox init container. It exits once a seed accepts TCP and no lookup has failed for `seedWait.settleSeconds` (35). If nothing failed it exits at once, and `storage-0` never waits. Expect up to about a minute of extra start-up on a fresh install. A Go-side join retry would make this unnecessary.

## Caveats

- **Metadata is per pod unless you share it.** With the default SQLite, dashboards and alarms that the agents synthesize land in each agent's own store and never reach the UI. Edits on one query replica aren't seen by another either. Use a shared `postgres://` DSN for the agent and for `query.replicas > 1`.
- **Alarms and reports run on every query replica**, with no leader election, so more than one replica sends duplicate notifications.
- **ClickHouse with several storage replicas:** all of them point at one database, and fan-out reads repeat rows. Use one storage replica or `mode: single`.
- `oidc` and `tiering` values are placeholders (coming in wave 4). Wire them with `extraArgs` / `extraEnv` until the flags exist.
- Out of scope, listed as next: a mutating admission webhook for runtime injection (JVM, Node, Python agents).

## Verify

```bash
bash deploy/helm/verify.sh      # helm lint + template (5 value sets) + kubeconform -strict, k8s 1.30
```

`deploy/helm/kind-e2e.sh` (usage in its header) creates a kind cluster `w4-helm`, installs both modes with the agent, checks the UI, `/api/v1/stats` and `/api/v1/cluster`, then deletes the cluster.
