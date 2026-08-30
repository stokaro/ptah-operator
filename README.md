# Ptah Operator

Ptah Operator is a Kubernetes-native control plane for continuously converging
PostgreSQL and MySQL schemas from immutable OCI artifacts. Database work runs
in short-lived, hardened Jobs; the controller itself has no permission to read
database Secrets.

The API is currently `v1alpha1`. Treat it as an implementation preview until
the complete database end-to-end matrix is green and a release is published.

## Reconciliation model

```text
resolve tag to digest -> verify artifact -> observe database -> publish plan
       ^                                                        |
       |                         approval (when required) <------+
       |                                                        |
       +--- verify convergence <- apply exact approved plan <---+
```

The controller records a claim before creating each Job, permits at most one
active Job per `PtahSchema`, and resumes that claim after a restart. A process
exit is never treated as proof of convergence: every apply is followed by a
new read-only observation.

Key safety properties:

- OCI tags are resolved once and all later artifact access uses the digest.
- Plans bind exact bytes to artifact, target, observed state, policy, Ptah
  version, executor image, runner image, and runner protocol.
- Approvals are separate immutable resources stamped with authenticated
  admission identity.
- Destructive plans are disabled by default and still require an exact-plan
  approval when enabled.
- Database and registry credentials are isolated from each other and are never
  placed in status, Events, plan resources, or command arguments.
- Deletion and suspension never execute cleanup SQL.
- An uncertain apply outcome always returns to observation instead of replay.

## Install from this checkout

The manager, runner, and Ptah executor images must be selected explicitly. The
execution images are required to use immutable `@sha256:` references.

```sh
helm upgrade --install ptah-operator ./charts/ptah-operator \
  --namespace ptah-system \
  --create-namespace \
  --set-string image.digest=sha256:<operator-image-digest> \
  --set-string execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:<operator-image-digest> \
  --set-string execution.executorImage=ghcr.io/stokaro/ptah@sha256:<ptah-image-digest> \
  --set-string execution.ptahVersion=<ptah-version>
```

The chart supports the Kubernetes window documented in
[Kubernetes support](docs/kubernetes-support.md). It intentionally does not
bind the optional approver ClusterRole to any identity.

## First schema

Create the database URL as a namespaced Secret and the non-secret verification
policy as a ConfigMap, then apply the example resource:

```sh
kubectl -n application create secret generic application-database \
  --from-literal=url='<database-url>'
kubectl -n application create configmap ptah-verification-policy \
  --from-file=policy.yaml=examples/verification-policy.yaml
kubectl apply -f examples/ptahschema.yaml
kubectl -n application get ptahschema application -w
```

Replace every placeholder in the example first. For private registries, add a
same-namespace `registryAuthFrom` reference; the API supports environment-key
Secrets and standard Docker config JSON Secrets.

## Documentation

- [Architecture and state machine](docs/architecture.md)
- [Security model](docs/security.md)
- [Exact-plan approvals](docs/approvals.md)
- [Operations and failure recovery](docs/operations.md)
- [Kubernetes support policy](docs/kubernetes-support.md)

`PtahMigration` is deliberately not folded into `PtahSchema`. A future
versioned-migration controller can reuse the OCI transport, credential
isolation, execution protocol, and target coordination primitives while
retaining its own API and state machine.
