---
title: Configuration
description: What the chart takes, what it requires, and what it deliberately has no default for.
---

Every setting below is a Helm value on `charts/ptah-operator`. Three of them
have no default and the chart refuses to render without them, because each
names something the operator must not guess.

## The three values with no default

`image.digest` is the manager's registry manifest digest. A tag is not
accepted, and a Docker image ID is not a registry manifest digest.

`execution.executorImage` is the Ptah build the operator runs, by digest.
`execution.runnerImage` is the runner beside it, also by digest.

`execution.ptahVersion` is the identity of the build inside that executor
digest. It is verified from the image's own provenance and never inferred from
a tag, so the chart has no default to fall back on. Which build has actually
been run against this operator is [Ptah compatibility](../../support/ptah/).

All three are recorded in every plan, approval, Job and applied status, so a
reader of a converged schema can see exactly what produced it.

## Every value

<!-- BEGIN GENERATED VALUES -->
| Value | Default | What it does |
| --- | --- | --- |
| `replicaCount` | `2` | Manager replicas. Scaling this release is how the operator runs highly available; a second release in the same cluster is not supported. More than one replica requires leaderElection. |
| `image` |  | The image every runtime component runs from: the manager, the certificate rotator and the CRD manager are three commands in one image. |
| `image.repository` | `ghcr.io/stokaro/ptah-operator` | Repository the manager image is pulled from. |
| `image.tag` | `"0.1.0"` | Recorded for a reader. The digest below is what is actually pulled. |
| `image.digest` | `""` | Required registry manifest digest, including for local development. A Docker image ID is not a registry manifest digest. |
| `image.pullPolicy` | `IfNotPresent` | Pull policy for the manager image. AlwaysPullImages in the cluster rewrites this, which is what admission.alwaysPullImagesEnabled declares. |
| `imagePullSecrets` | `[]` | Pull Secrets added to every Pod this chart creates. |
| `nameOverride` | `""` | Replaces the chart name in generated object names. |
| `fullnameOverride` | `""` | Replaces the complete generated name, release prefix included. |
| `execution` |  | The data plane: the images a database operation runs in, and the Ptah build they carry. The manager dispatches Jobs from these and runs no SQL itself. |
| `execution.executorImage` | `""` | Both execution images are required and must include @sha256:<64 hex>. ptahVersion is also required, is limited to 128 bytes, and must identify the Ptah build verified for the exact executorImage digest; the chart deliberately has no default. |
| `execution.runnerImage` | `""` | The runner image, built from this operator's source, which supervises the executor inside a task Pod and returns its result frame. |
| `execution.ptahVersion` | `""` | The Ptah build the executorImage digest carries. A plan records it, and an apply is refused when it no longer matches. |
| `admission` |  | What the cluster's own admission plugins do to the Pods this operator creates. The chart's guards assert the resulting Pod rather than the intent, so these declare what the API server is configured to do. |
| `admission.defaultTolerationsEnabled` | `true` | These values must match kube-apiserver's enabled admission plugins and DefaultTolerationSeconds flags. Zero means an exact zero-second NoExecute toleration; it does not disable the plugin. |
| `admission.defaultNotReadyTolerationSeconds` | `300` | The DefaultTolerationSeconds value for node.kubernetes.io/not-ready. |
| `admission.defaultUnreachableTolerationSeconds` | `300` | The DefaultTolerationSeconds value for node.kubernetes.io/unreachable. |
| `admission.extendedResourceTolerationEnabled` | `false` | Whether ExtendedResourceToleration is enabled, which adds a toleration for every extended resource a Pod requests. |
| `admission.alwaysPullImagesEnabled` | `false` | Whether AlwaysPullImages is enabled, which rewrites every imagePullPolicy to Always. The guards accept that rewrite only when this says so. |
| `leaderElection` | `true` | Required whenever replicaCount is greater than one. Disable only for an isolated single-replica installation. Exactly one Helm release is supported per cluster; scale this release for high availability. |
| `coordination` |  | Where the operator's Leases live. |
| `coordination.namespace` | `""` | Leave empty to use the Helm release namespace. This namespace contains both database target Leases and the manager leader-election Lease for the cluster's one supported operator release. |
| `serviceAccount` |  | The identity the runtime components run as. |
| `serviceAccount.create` | `true` | With create=false, name is a stable base, not a complete object name. For every release sequence N, pre-create the dedicated ServiceAccount <name>-v<N>. The chart never reuses or deletes these user-owned identities. Any additional RoleBinding or ClusterRoleBinding for an active or retained epoch blocks fail-closed upgrade and uninstall. |
| `serviceAccount.name` | `""` | Base name for the per-sequence ServiceAccounts. Empty means the generated release name. |
| `serviceAccount.annotations` | `{}` | Annotations added to the ServiceAccounts the chart creates, which is where a cloud identity binding goes. |
| `podAnnotations` | `{}` | Annotations added to the manager and rotator Pods. |
| `podLabels` | `{}` | Labels added to the manager and rotator Pods, beside the chart's own. |
| `resources` |  | Manager container resources. The manager reconciles and dispatches; the database work happens in task Pods with resources of their own. |
| `resources.requests` |  | What the manager container is guaranteed. |
| `resources.requests.cpu` | `50m` | CPU request for the manager container. |
| `resources.requests.memory` | `96Mi` | Memory request for the manager container. |
| `resources.limits` |  | The ceiling. CPU is deliberately unlimited: throttling a leader is how a Lease is lost. |
| `resources.limits.memory` | `256Mi` | Memory limit for the manager container. |
| `nodeSelector` | `{}` | Node labels the manager and rotator Pods are scheduled onto. |
| `tolerations` | `[]` | Taints the manager and rotator Pods tolerate, beside the ones admission.defaultTolerations describes. |
| `affinity` | `{}` | Affinity for the manager and rotator Pods. Replicas already spread across nodes by default; this is for anything narrower. |
| `priorityClassName` | `""` | Assign this class to the runtime Deployments and to the weight-zero CRD reconcile hook after preflight verifies the live class. Earlier bootstrap, preflight, and uninstall hooks remain classless; Pod admission may apply the cluster's global default class to those hooks. |
| `priorityClassValue` | `0` | Pin the scheduling semantics of priorityClassName. Use 0 and PreemptLowerPriority when no class is configured. The preflight refuses a named class whose live value or effective preemption policy differs, so a PriorityClass change cannot silently alter an approved runtime contract. |
| `priorityClassPreemptionPolicy` | `PreemptLowerPriority` | The preemption policy the live PriorityClass must declare, checked the same way as priorityClassValue. |
| `metrics` |  | The manager's metrics endpoint. |
| `metrics.bindAddress` | `":8080"` | Address the manager serves metrics on. The guards expect port 8080 on the Pod, so change the Service below rather than this. |
| `metrics.service` |  | A Service in front of the metrics port, for a scraper that needs one. |
| `metrics.service.enabled` | `true` | Whether to create the metrics Service. |
| `metrics.service.port` | `8080` | Port the metrics Service listens on. |
| `monitoring` |  | Scrape configuration for a Prometheus that discovers its targets from ServiceMonitor objects. |
| `monitoring.serviceMonitor` |  | A ServiceMonitor, which is how Prometheus reaches each manager Pod behind the metrics Service rather than one replica behind its address. |
| `monitoring.serviceMonitor.enabled` | `false` | Whether to create a ServiceMonitor. Off by default: the object needs the Prometheus Operator CRDs, and an install into a cluster without them fails on a kind the API server does not serve. |
| `monitoring.serviceMonitor.labels` | `{}` | Labels the Prometheus instance selects ServiceMonitors by. Empty means the object is created and selected by nothing, which is why a deployment that turns this on usually has to set them. |
| `monitoring.serviceMonitor.interval` | `""` | How often to scrape. Unset leaves the interval to the Prometheus configuration rather than pinning it here. |
| `monitoring.serviceMonitor.scrapeTimeout` | `""` | How long a scrape may take. Unset leaves it to Prometheus. |
| `webhook` |  | The admission webhook server inside the manager, which is what refuses an approval that does not name an exact plan. |
| `webhook.port` | `9443` | Port the manager serves admission on. |
| `webhook.timeoutSeconds` | `5` | Timeout the API server applies to the fast admission paths. |
| `webhook.controllerWriteTimeoutSeconds` | `30` | The manager-write validator may read one schema, one plan, and up to 16 immutable plan chunks. Kubernetes caps admission timeouts at 30 seconds; keep this fail-closed path at that bound independently of the fast paths. |
| `webhook.existingSecret` | `""` | Name of a pre-provisioned kubernetes.io/tls Secret. The Secret must also contain ca.crt. Set caBundle to render the chart without cluster lookup. |
| `webhook.caBundle` | `""` | PEM-encoded CA certificate for existingSecret. Leave empty during a connected Helm install to read ca.crt from that Secret. |
| `certificateRotation` |  | The serving certificates the webhook presents, and the component that issues and replaces them without anybody holding a private key. |
| `certificateRotation.enabled` | `true` | Built-in management is automatically omitted when webhook.existingSecret is set. The manager never receives Secret-read permission. |
| `certificateRotation.recreateMissingSecret` | `false` | Opt in to recreating a deleted chart-generated Secret. This necessarily grants the rotator namespace-wide Secret CREATE in RBAC; a fail-closed admission policy narrows its use to the exact generated TLS Secret. |
| `certificateRotation.interval` | `"6h"` | How long the rotator waits after a reconciliation that changed nothing. |
| `certificateRotation.operationTimeout` | `"15m"` | Ceiling on one reconciliation, including the probes that prove every endpoint serves the new certificate. |
| `certificateRotation.retryInitial` | `"5s"` | First backoff delay after a failed reconciliation. |
| `certificateRotation.retryMax` | `"5m"` | Ceiling on the backoff after repeated failures. |
| `certificateRotation.healthPort` | `8081` | Port the rotator serves its own health and readiness probes on. |
| `certificateRotation.candidatePort` | `9444` | Port the rotator serves candidate admission TLS on while it proves a new certificate before adopting it. |
| `certificateRotation.admissionConvergence` |  | A transition is accepted only after every directly addressed API server observes both canary webhooks continuously for this stability window. |
| `certificateRotation.admissionConvergence.stabilityDuration` | `"10s"` | How long the candidate must be observed continuously, unbroken. |
| `certificateRotation.admissionConvergence.pollInterval` | `"1s"` | How often the rotator asks each API-server endpoint during that window. |
| `certificateRotation.admissionConvergence.requestTimeout` | `"5s"` | Bounds one endpoint observation: marker GET plus both denial probes. |
| `certificateRotation.renewalThreshold` | `"720h"` | Rotate a certificate once no more than this much validity remains. |
| `certificateRotation.servingCertificateValidity` | `"2160h"` | Validity of a newly issued serving certificate. |
| `certificateRotation.caCertificateValidity` | `"26280h"` | Validity of a newly issued CA certificate. |
| `certificateRotation.probeTimeout` | `"5m"` | Ceiling on waiting for every webhook endpoint to serve a replacement certificate before the rotation is abandoned. |
| `certificateRotation.probeInterval` | `"2s"` | How often those endpoints are probed while waiting. |
| `certificateRotation.leaseDuration` | `"10m"` | Duration of the Lease that makes one rotator the one doing the work. |
| `certificateRotation.leaseAcquireTimeout` | `"30s"` | How long a rotator waits for that Lease before giving up this pass. |
| `certificateRotation.resources` |  | Rotator container resources. It sleeps between passes and does a few seconds of work in each. |
| `certificateRotation.resources.requests` |  | What the rotator container is guaranteed. |
| `certificateRotation.resources.requests.cpu` | `10m` | CPU request for the rotator container. |
| `certificateRotation.resources.requests.memory` | `32Mi` | Memory request for the rotator container. |
| `certificateRotation.resources.limits` |  | The ceiling, memory only, for the same reason as the manager. |
| `certificateRotation.resources.limits.memory` | `64Mi` | Memory limit for the rotator container. |
| `approverClusterRole` |  | A ClusterRole for the people who approve changes: read schemas, plans and approvals, and create an approval. Bind it yourself; the chart binds nobody. |
| `approverClusterRole.create` | `true` | Whether to create that ClusterRole. |
| `podDisruptionBudget` |  | A PodDisruptionBudget for the manager, so a drain cannot take every replica at once. |
| `podDisruptionBudget.enabled` | `true` | Whether to create the PodDisruptionBudget. |
| `podDisruptionBudget.minAvailable` | `1` | Replicas that must stay available during a voluntary disruption. |
<!-- END GENERATED VALUES -->

The table is generated from `charts/ptah-operator/values.yaml`, which is where
each of these is declared. Run `npm run values:write` in `docs/site` after
changing a value, and the documentation gate refuses a stale table.
