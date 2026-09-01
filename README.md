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

The manager, runner, and Ptah executor images must be selected explicitly. All
three are required to use immutable SHA-256 references. The executor version
is explicit too: it must identify the verified build in the selected executor
digest and is never inferred from an image tag or supplied by a chart default.

```sh
helm upgrade --install ptah-operator ./charts/ptah-operator \
  --namespace ptah-system \
  --create-namespace \
  --set-string image.digest=sha256:<operator-image-digest> \
  --set-string execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:<operator-image-digest> \
  --set-string execution.executorImage=ghcr.io/stokaro/ptah@sha256:<ptah-image-digest> \
  --set-string execution.ptahVersion=<ptah-version>
```

The supplied version is recorded in plans, approvals, Jobs, and applied status
alongside the executor digest. Verify both values from the executor's release
provenance before installation; changing the digest requires verifying and
supplying its version again.

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
kubectl -n application patch configmap ptah-verification-policy \
  --type=merge -p '{"immutable":true}'
kubectl apply -f examples/ptahschema.yaml
kubectl -n application get ptahschema application -w
```

Replace every placeholder in the example first. For private registries, add a
same-namespace `registryAuthFrom` reference; the API supports environment-key
Secrets and standard Docker config JSON Secrets. Either representation must
contain a fixed `registry` key whose authority-only `host[:port]` value exactly
matches the OCI client's effective request authority (`registry-1.docker.io`
for an `oci://docker.io/...` source). A Docker config Secret keeps its standard
`.dockerconfigjson` data in addition to that owner-controlled grant. If authenticated
`plainHTTP` is unavoidable, the authentication Secret must also contain
`allowPlainHTTP: "true"`; anonymous plain HTTP needs no Secret grant.
`clientCertificateFrom` is currently rejected because the pinned executor
cannot constrain a client certificate across cross-host redirects.
Verification-policy
ConfigMaps must be immutable. To change a policy, create a new ConfigMap name
and update the schema reference; delete-and-recreate is intentionally not
treated as the same policy.

## Documentation

- [Architecture and state machine](docs/architecture.md)
- [Security model](docs/security.md)
- [Exact-plan approvals](docs/approvals.md)
- [Operations and failure recovery](docs/operations.md)
- [Kubernetes support policy](docs/kubernetes-support.md)
- [Database support and privileges](docs/database-support.md)
- [Releases and provenance](docs/releases.md)

`PtahMigration` is deliberately not folded into `PtahSchema`. A future
versioned-migration controller can reuse the OCI transport, credential
isolation, execution protocol, and target coordination primitives while
retaining its own API and state machine.
