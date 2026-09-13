---
title: First schema
description: One worked example, from Secret and policy to a converged PtahSchema.
---

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

The `examples/` directory named here is the one in the repository.

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

What the schema reports while it converges is
[the status progression](../use/operations.md#normal-status-progression), and
what each reason means is
[condition reasons](../troubleshoot/condition-reasons.md).

## It stops, and waits for you

The example sets `apply: OnApproval`, which is also the default. The operator
resolves the artifact, observes the database and publishes a plan, and then
stops: `ApprovalRequired` becomes `True` and the phase is `AwaitingApproval`.
Nothing has run against the database yet.

Read the plan before approving it. The SQL is in controller-owned ConfigMaps
rather than in the status, so this reads the first chunk of the current plan:

```sh
PLAN=$(kubectl -n application get ptahschema application -o jsonpath='{.status.plan.name}')
CHUNK=$(kubectl -n application get ptahschemaplan "$PLAN" -o jsonpath='{.spec.chunks[0].name}')
kubectl -n application get configmap "$CHUNK" -o jsonpath='{.binaryData.chunk}' \
  | base64 -d | jq -r '.statements[] | "\(.severity)\t\(.sql)"'
```

An approval names the schema, the plan and that plan's fingerprint. Everything
else on it is stamped by the admission webhook from the plan being approved,
which is why this creates it from what the API server already holds rather than
from values copied by hand:

```sh
kubectl -n application get ptahschema application -o json > schema.json
kubectl -n application get ptahschemaplan \
  "$(jq -r .status.plan.name schema.json)" -o json > plan.json
jq -n --slurpfile schema schema.json --slurpfile plan plan.json '
  {
    apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchemaApproval",
    metadata: {name: "application-first"},
    spec: {
      schemaRef: {name: $schema[0].metadata.name, uid: $schema[0].metadata.uid},
      planRef:   {name: $plan[0].metadata.name,   uid: $plan[0].metadata.uid},
      planFingerprint: $plan[0].spec.fingerprint
    }
  }' | kubectl -n application create -f -
```

Creating it requires a binding that grants the approval verbs;
`examples/approver-plan-reader-role.yaml` is the reader half and
[exact-plan approvals](../use/approvals.md) is why the binding is separate from
the one that writes the desired state.

With the decision recorded the plan runs, and convergence is proved by a second
observation rather than by the Job finishing:

```sh
kubectl -n application get ptahschema application \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

`InSync=True` with reason `ScopedConverged` is the end of it. `Applying=False`
with reason `JobCompleted` beside it is the distinction worth reading twice: the
Job finished, and that is not the same claim.

Watch the whole sequence, including this one, in
[the recorded runs](../../demo/).
