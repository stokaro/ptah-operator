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
