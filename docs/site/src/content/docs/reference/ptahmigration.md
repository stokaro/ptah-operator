---
title: PtahMigration
description: Every field of the PtahMigration resource, generated from the API types.
---

`PtahMigration` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

A `PtahMigration` runs an ordered sequence and records what it ran. Where a
`PtahSchema` compares a declaration against the database, this one asks the
database which versions it already has and applies the rest in order.

### The smallest resource that runs

An artifact holding the sequence, the policy it must satisfy, and the database
to run it against. `spec.policy.apply` defaults to `OnApproval`: the resource
reads the history, plans the versions that are missing, and waits.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/orders-migrations:1.4.0
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
```

### Beside a PtahSchema, over one database

Both resources name the same `coordinationKey` and both set `sharedRealm`, so
they take turns under one lease rather than running at once. The operator
checks that a person decided to share; it cannot check that the areas they
write to are really disjoint.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    sharedRealm: true
    urlFrom:
      name: application-database
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/orders-migrations@sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  policy:
    # A migration artifact carries arbitrary SQL, and no analyzer calls
    # arbitrary SQL safe, so the conservative setting is also the default.
    apply: OnApproval
    # The wait for the database's own migration lock. This is not the
    # Kubernetes Lease: two controllers that never run at the same time still
    # need the database to serialize them.
    lockTimeout: 5m
  interval: 10m
  execution:
    activeDeadlineSeconds: 900
    serviceAccountName: ptah-execution
```

### An engine that will not run DDL inside a transaction

MySQL commits implicitly on DDL, so a file that fails halfway leaves what ran
in place. `transactionMode: none` says that plainly instead of promising a
rollback the engine will not perform, and a `checkpoint` in the sequence is
where the author chose to make that recoverable.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: MySQL
    coordinationKey: production/orders-primary
    urlFrom:
      name: orders-database
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/orders-migrations:1.4.0
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  policy:
    apply: OnApproval
    transactionMode: none
  suspend: false
```

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.artifact` | `object`, required | Artifact is the OCI migration directory this history is matched against. It reuses the schema path's credential-isolated source contract: fetching an artifact never hands registry credentials to the process that runs SQL. |
| `spec.artifact.ociRef` | `string`, required | OCIRef is a desired-schema artifact reference. It may name a tag or a digest; every later operation receives only the resolved digest. |
| `spec.artifact.registryAuthFrom` | `object` | RegistryAuthFrom names the Secret an operation Pod reads the registry credential from. The process that runs SQL never receives it. |
| `spec.artifact.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` | DockerConfigJSONKey is the Secret key holding a Docker config document. |
| `spec.artifact.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | Mode says how the credential reaches the executor: as environment variables, or as a Docker config file. |
| `spec.artifact.registryAuthFrom.name` | `string`, required | Name of the Secret the registry credential is read from. The manager never reads it; the operation Pod does. |
| `spec.artifact.registryAuthFrom.passwordKey` | `string`, default `password` | PasswordKey is the Secret key holding the password. |
| `spec.artifact.registryAuthFrom.registryKey` | `string`, one of `registry`, default `registry` | RegistryKey is retained for source compatibility. The key is fixed so the Secret owner, rather than a PtahSchema author, controls the authority grant. The referenced Secret must contain an authority-only host[:port] value. RegistryKey is the Secret key naming the registry the credential is for. |
| `spec.artifact.registryAuthFrom.tokenKey` | `string`, default `token` | TokenKey is the Secret key holding a bearer token, where one is used instead of a username and password. |
| `spec.artifact.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. UsernameKey is the Secret key holding the username. |
| `spec.artifact.transport` | `object` | Transport is how the registry is reached: plain HTTP, a custom CA, a client certificate. |
| `spec.artifact.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `spec.artifact.transport.caFrom.key` | `string`, required | The key to select. |
| `spec.artifact.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.artifact.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `spec.artifact.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `spec.artifact.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` | CertificateKey is the Secret key holding the certificate. |
| `spec.artifact.transport.clientCertificateFrom.name` | `string`, required | Name of the Secret holding the client certificate. |
| `spec.artifact.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` | PrivateKeyKey is the Secret key holding its private key. |
| `spec.artifact.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `spec.artifact.verificationPolicyFrom` | `object`, required | VerificationPolicyFrom names the immutable ConfigMap holding the policy the artifact must satisfy. Editing it retires the plans computed under the previous version rather than letting them apply. |
| `spec.artifact.verificationPolicyFrom.key` | `string`, required | The key to select. |
| `spec.artifact.verificationPolicyFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.artifact.verificationPolicyFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `spec.execution` | `object`, default `{}` | Execution shapes the Job a run happens in: its deadlines, its resources and the scheduling it inherits. |
| `spec.execution.activeDeadlineSeconds` | `integer`, default `900` | ActiveDeadlineSeconds is how long one operation Job may run before Kubernetes ends it. An apply that hits this leaves an uncertain outcome, which returns to observation rather than to a replay. |
| `spec.execution.affinity` | `object` | Affinity is scheduling affinity for those Pods. |
| `spec.execution.affinity.nodeAffinity` | `object` | Describes node affinity scheduling rules for the pod. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution` | `[]object` | The scheduler will prefer to schedule pods to nodes that satisfy the affinity expressions specified by this field, but it may choose a node that violates one or more of the expressions. The node that is most preferred is the one with the greatest sum of weights, i.e. for each node that meets all of the scheduling requirements (resource request, requiredDuringScheduling affinity expressions, etc.), compute a sum by iterating through the elements of this field and adding "weight" to the sum if the node matches the corresponding matchExpressions; the node(s) with the highest sum are the most preferred. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference` | `object`, required | A node selector term, associated with the corresponding weight. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchExpressions` | `[]object` | A list of node selector requirements by node's labels. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchExpressions[].key` | `string`, required | The label key that the selector applies to. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchExpressions[].operator` | `string`, required | Represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists, DoesNotExist. Gt, and Lt. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchExpressions[].values` | `[]string` | An array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. If the operator is Gt or Lt, the values array must have a single element, which will be interpreted as an integer. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchFields` | `[]object` | A list of node selector requirements by node's fields. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchFields[].key` | `string`, required | The label key that the selector applies to. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchFields[].operator` | `string`, required | Represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists, DoesNotExist. Gt, and Lt. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].preference.matchFields[].values` | `[]string` | An array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. If the operator is Gt or Lt, the values array must have a single element, which will be interpreted as an integer. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].weight` | `integer`, required | Weight associated with matching the corresponding nodeSelectorTerm, in the range 1-100. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution` | `object` | If the affinity requirements specified by this field are not met at scheduling time, the pod will not be scheduled onto the node. If the affinity requirements specified by this field cease to be met at some point during pod execution (e.g. due to an update), the system may or may not try to eventually evict the pod from its node. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms` | `[]object`, required | Required. A list of node selector terms. The terms are ORed. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchExpressions` | `[]object` | A list of node selector requirements by node's labels. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchExpressions[].key` | `string`, required | The label key that the selector applies to. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchExpressions[].operator` | `string`, required | Represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists, DoesNotExist. Gt, and Lt. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchExpressions[].values` | `[]string` | An array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. If the operator is Gt or Lt, the values array must have a single element, which will be interpreted as an integer. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchFields` | `[]object` | A list of node selector requirements by node's fields. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchFields[].key` | `string`, required | The label key that the selector applies to. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchFields[].operator` | `string`, required | Represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists, DoesNotExist. Gt, and Lt. |
| `spec.execution.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchFields[].values` | `[]string` | An array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. If the operator is Gt or Lt, the values array must have a single element, which will be interpreted as an integer. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAffinity` | `object` | Describes pod affinity scheduling rules (e.g. co-locate this pod in the same node, zone, etc. as some other pod(s)). |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution` | `[]object` | The scheduler will prefer to schedule pods to nodes that satisfy the affinity expressions specified by this field, but it may choose a node that violates one or more of the expressions. The node that is most preferred is the one with the greatest sum of weights, i.e. for each node that meets all of the scheduling requirements (resource request, requiredDuringScheduling affinity expressions, etc.), compute a sum by iterating through the elements of this field and adding "weight" to the sum if the node has pods which matches the corresponding podAffinityTerm; the node(s) with the highest sum are the most preferred. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm` | `object`, required | Required. A pod affinity term, associated with the corresponding weight. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector` | `object` | A label query over a set of resources, in this case pods. If it's null, this PodAffinityTerm matches with no Pods. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.matchLabelKeys` | `[]string` | MatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key in (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both matchLabelKeys and labelSelector. Also, matchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.mismatchLabelKeys` | `[]string` | MismatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key notin (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both mismatchLabelKeys and labelSelector. Also, mismatchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector` | `object` | A label query over the set of namespaces that the term applies to. The term is applied to the union of the namespaces selected by this field and the ones listed in the namespaces field. null selector and null or empty namespaces list means "this pod's namespace". An empty selector ({}) matches all namespaces. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaces` | `[]string` | namespaces specifies a static list of namespace names that the term applies to. The term is applied to the union of the namespaces listed in this field and the ones selected by namespaceSelector. null or empty namespaces list and null namespaceSelector means "this pod's namespace". |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.topologyKey` | `string`, required | This pod should be co-located (affinity) or not co-located (anti-affinity) with the pods matching the labelSelector in the specified namespaces, where co-located is defined as running on a node whose value of the label with key topologyKey matches that of any node on which any of the selected pods is running. Empty topologyKey is not allowed. |
| `spec.execution.affinity.podAffinity.preferredDuringSchedulingIgnoredDuringExecution[].weight` | `integer`, required | weight associated with matching the corresponding podAffinityTerm, in the range 1-100. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution` | `[]object` | If the affinity requirements specified by this field are not met at scheduling time, the pod will not be scheduled onto the node. If the affinity requirements specified by this field cease to be met at some point during pod execution (e.g. due to a pod label update), the system may or may not try to eventually evict the pod from its node. When there are multiple elements, the lists of nodes corresponding to each podAffinityTerm are intersected, i.e. all terms must be satisfied. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector` | `object` | A label query over a set of resources, in this case pods. If it's null, this PodAffinityTerm matches with no Pods. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].matchLabelKeys` | `[]string` | MatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key in (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both matchLabelKeys and labelSelector. Also, matchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].mismatchLabelKeys` | `[]string` | MismatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key notin (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both mismatchLabelKeys and labelSelector. Also, mismatchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector` | `object` | A label query over the set of namespaces that the term applies to. The term is applied to the union of the namespaces selected by this field and the ones listed in the namespaces field. null selector and null or empty namespaces list means "this pod's namespace". An empty selector ({}) matches all namespaces. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaces` | `[]string` | namespaces specifies a static list of namespace names that the term applies to. The term is applied to the union of the namespaces listed in this field and the ones selected by namespaceSelector. null or empty namespaces list and null namespaceSelector means "this pod's namespace". |
| `spec.execution.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[].topologyKey` | `string`, required | This pod should be co-located (affinity) or not co-located (anti-affinity) with the pods matching the labelSelector in the specified namespaces, where co-located is defined as running on a node whose value of the label with key topologyKey matches that of any node on which any of the selected pods is running. Empty topologyKey is not allowed. |
| `spec.execution.affinity.podAntiAffinity` | `object` | Describes pod anti-affinity scheduling rules (e.g. avoid putting this pod in the same node, zone, etc. as some other pod(s)). |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution` | `[]object` | The scheduler will prefer to schedule pods to nodes that satisfy the anti-affinity expressions specified by this field, but it may choose a node that violates one or more of the expressions. The node that is most preferred is the one with the greatest sum of weights, i.e. for each node that meets all of the scheduling requirements (resource request, requiredDuringScheduling anti-affinity expressions, etc.), compute a sum by iterating through the elements of this field and subtracting "weight" from the sum if the node has pods which matches the corresponding podAffinityTerm; the node(s) with the highest sum are the most preferred. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm` | `object`, required | Required. A pod affinity term, associated with the corresponding weight. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector` | `object` | A label query over a set of resources, in this case pods. If it's null, this PodAffinityTerm matches with no Pods. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.labelSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.matchLabelKeys` | `[]string` | MatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key in (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both matchLabelKeys and labelSelector. Also, matchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.mismatchLabelKeys` | `[]string` | MismatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key notin (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both mismatchLabelKeys and labelSelector. Also, mismatchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector` | `object` | A label query over the set of namespaces that the term applies to. The term is applied to the union of the namespaces selected by this field and the ones listed in the namespaces field. null selector and null or empty namespaces list means "this pod's namespace". An empty selector ({}) matches all namespaces. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaceSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.namespaces` | `[]string` | namespaces specifies a static list of namespace names that the term applies to. The term is applied to the union of the namespaces listed in this field and the ones selected by namespaceSelector. null or empty namespaces list and null namespaceSelector means "this pod's namespace". |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].podAffinityTerm.topologyKey` | `string`, required | This pod should be co-located (affinity) or not co-located (anti-affinity) with the pods matching the labelSelector in the specified namespaces, where co-located is defined as running on a node whose value of the label with key topologyKey matches that of any node on which any of the selected pods is running. Empty topologyKey is not allowed. |
| `spec.execution.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[].weight` | `integer`, required | weight associated with matching the corresponding podAffinityTerm, in the range 1-100. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution` | `[]object` | If the anti-affinity requirements specified by this field are not met at scheduling time, the pod will not be scheduled onto the node. If the anti-affinity requirements specified by this field cease to be met at some point during pod execution (e.g. due to a pod label update), the system may or may not try to eventually evict the pod from its node. When there are multiple elements, the lists of nodes corresponding to each podAffinityTerm are intersected, i.e. all terms must be satisfied. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector` | `object` | A label query over a set of resources, in this case pods. If it's null, this PodAffinityTerm matches with no Pods. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].labelSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].matchLabelKeys` | `[]string` | MatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key in (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both matchLabelKeys and labelSelector. Also, matchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].mismatchLabelKeys` | `[]string` | MismatchLabelKeys is a set of pod label keys to select which pods will be taken into consideration. The keys are used to lookup values from the incoming pod labels, those key-value labels are merged with `labelSelector` as `key notin (value)` to select the group of existing pods which pods will be taken into consideration for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming pod labels will be ignored. The default value is empty. The same key is forbidden to exist in both mismatchLabelKeys and labelSelector. Also, mismatchLabelKeys cannot be set when labelSelector isn't set. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector` | `object` | A label query over the set of namespaces that the term applies to. The term is applied to the union of the namespaces selected by this field and the ones listed in the namespaces field. null selector and null or empty namespaces list means "this pod's namespace". An empty selector ({}) matches all namespaces. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions` | `[]object` | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions[].key` | `string`, required | key is the label key that the selector applies to. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions[].operator` | `string`, required | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchExpressions[].values` | `[]string` | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaceSelector.matchLabels` | `object` | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].namespaces` | `[]string` | namespaces specifies a static list of namespace names that the term applies to. The term is applied to the union of the namespaces listed in this field and the ones selected by namespaceSelector. null or empty namespaces list and null namespaceSelector means "this pod's namespace". |
| `spec.execution.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[].topologyKey` | `string`, required | This pod should be co-located (affinity) or not co-located (anti-affinity) with the pods matching the labelSelector in the specified namespaces, where co-located is defined as running on a node whose value of the label with key topologyKey matches that of any node on which any of the selected pods is running. Empty topologyKey is not allowed. |
| `spec.execution.connectTimeout` | `string`, default `10s` | ConnectTimeout bounds opening the database connection, so an unreachable database fails in seconds rather than holding the Job to its deadline. |
| `spec.execution.failureRetryInterval` | `string`, default `30s` | FailureRetryInterval is how long the controller waits after a failed operation before trying the same one again. |
| `spec.execution.imagePullSecrets` | `[]object` | ImagePullSecrets are the pull Secrets those Pods use. |
| `spec.execution.imagePullSecrets[].name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.execution.nodeSelector` | `object` | NodeSelector restricts where operation Pods may be scheduled. |
| `spec.execution.priorityClassName` | `string` | PriorityClassName is the scheduling priority they run at. |
| `spec.execution.resources` | `object` | Resources are the requests and limits of the container that runs SQL. |
| `spec.execution.resources.claims` | `[]object` | Claims lists the names of resources, defined in spec.resourceClaims, that are used by this container. This field depends on the DynamicResourceAllocation feature gate. This field is immutable. It can only be set for containers. |
| `spec.execution.resources.claims[].name` | `string`, required | Name must match the name of one entry in pod.spec.resourceClaims of the Pod where this field is used. It makes that resource available inside a container. |
| `spec.execution.resources.claims[].request` | `string` | Request is the name chosen for a request in the referenced claim. If empty, everything from the claim is made available, otherwise only the result of this request. |
| `spec.execution.resources.limits` | `object` | Limits describes the maximum amount of compute resources allowed. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.execution.resources.requests` | `object` | Requests describes the minimum amount of compute resources required. If Requests is omitted for a container, it defaults to Limits if that is explicitly specified, otherwise to an implementation-defined value. Requests cannot exceed Limits. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.execution.runtimeClassName` | `string` | RuntimeClassName selects the container runtime they use. The admission snapshot records what the cluster resolved, so a class that changed under a claim is refused rather than run. |
| `spec.execution.serviceAccountName` | `string` | ServiceAccountName is the identity operation Pods run as. It is theirs rather than the manager's, and it needs no Kubernetes permission at all. |
| `spec.execution.tolerations` | `[]object` | Tolerations are the taints those Pods tolerate. |
| `spec.execution.tolerations[].effect` | `string` | Effect indicates the taint effect to match. Empty means match all taint effects. When specified, allowed values are NoSchedule, PreferNoSchedule and NoExecute. |
| `spec.execution.tolerations[].key` | `string` | Key is the taint key that the toleration applies to. Empty means match all taint keys. If the key is empty, operator must be Exists; this combination means to match all values and all keys. |
| `spec.execution.tolerations[].operator` | `string` | Operator represents a key's relationship to the value. Valid operators are Exists, Equal, Lt, and Gt. Defaults to Equal. Exists is equivalent to wildcard for value, so that a pod can tolerate all taints of a particular category. Lt and Gt perform numeric comparisons (requires feature gate TaintTolerationComparisonOperators). |
| `spec.execution.tolerations[].tolerationSeconds` | `integer` | TolerationSeconds represents the period of time the toleration (which must be of effect NoExecute, otherwise this field is ignored) tolerates the taint. By default, it is not set, which means tolerate the taint forever (do not evict). Zero and negative values will be treated as 0 (evict immediately) by the system. |
| `spec.execution.tolerations[].value` | `string` | Value is the taint value the toleration matches to. If the operator is Exists, the value should be empty, otherwise just a regular string. |
| `spec.interval` | `string`, default `10m` | Interval is the cadence for resolving a mutable tag and re-reading the history. |
| `spec.policy` | `object`, default `{}` | Policy decides what may run without a person: the apply mode, the approval requirement, and the locks a run takes. |
| `spec.policy.apply` | `string`, one of `Never`, `OnApproval`, `Always`, default `OnApproval` | Apply defaults to OnApproval. A migration artifact carries arbitrary SQL, and no analyzer classifies arbitrary SQL as safe, so the conservative setting is the default one rather than the one an operator opts into. |
| `spec.policy.lockTimeout` | `string`, default `5m` | LockTimeout bounds the wait for the database's own migration lock. It is the lock the engine takes, not the Kubernetes Lease: two controllers that never run at the same time still need the database to serialize them. |
| `spec.policy.transactionMode` | `string`, one of `file`, `none` | TransactionMode is how Ptah is asked to wrap the run. The spellings are Ptah's own, because a second vocabulary for the same idea is a second place to hold in agreement with something this repository does not own. Unset means the operator passes no mode and Ptah chooses, which is what every migration does today. That is deliberate and not an oversight: a default here would change how every stored resource already runs, and it would pin this API to a default Ptah is free to move. It is on the policy rather than on the execution block because the two kinds share that block, and a PtahSchema runs no migrations and has no mode to choose. What the policy already says -- apply, lockTimeout -- is the same kind of statement: how a run is permitted to be made. A MySQL-family database refuses "file" whenever an interceptor is installed, which is why this is not a preference. See #132. |
| `spec.suspend` | `boolean`, default `false` | Suspend prevents new Jobs. A Job already applying is observed to a terminal result: a migration that is running is never abandoned, because the database would be left in a state nothing recorded. |
| `spec.target` | `object`, required | Target is the database this sequence runs against, named through a Secret the manager never reads. |
| `spec.target.coordinationKey` | `string`, required | CoordinationKey is a non-secret, stable identifier for the physical database realm. Every schema that can reach the same database through an alias, proxy, or different credential must use exactly the same key. |
| `spec.target.engine` | `string`, required | Engine is the database this target speaks. An engine outside the supported set is refused with a condition rather than attempted. |
| `spec.target.sharedRealm` | `boolean`, default `false` | SharedRealm declares that this resource manages only part of the database its coordination key names, and that every other resource managing that database has declared the same. It defaults to false, and a realm that more than one resource claims is refused while any claimant leaves it false. Serialization is not ownership: two resources that never run at the same time still undo each other's work by taking turns, so the operator blocks them rather than letting them alternate. A resource that runs nothing claims nothing. Deleting one leaves the realm, and so does suspending it: suspension is how a resource steps aside without being deleted. Resuming it puts it back in the census, and the conflict is refused then, before any Job. The declaration is what is verified, not the disjointness. No analyzer can tell whether two sets of arbitrary SQL touch the same rows, and a field that claimed otherwise would be the wrong kind of assurance. What it buys is that sharing is deliberate on every side: one resource that has not declared it blocks all of them, itself included. |
| `spec.target.urlFrom` | `object`, required | URLFrom names the Secret key holding the connection URL. The manager has no permission to read it: the operation Pod resolves it, and the URL never reaches status, an Event or a command line. |
| `spec.target.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `spec.target.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.target.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.activeOperation` | `object` | ActiveOperation is the claim the controller is currently carrying out, and nil when nothing is in flight. |
| `status.activeOperation.admissionSnapshot` | `object` | AdmissionSnapshot is the Pod envelope resolved before dispatch and bound into the Job and its Pod template. It is what lets Pod admission permit the built-in mutations that are modeled and safe while refusing any other change to what the Pod executes. A migration Pod is judged by the same envelope as a schema Pod, because it is the same kind of Pod. |
| `status.activeOperation.admissionSnapshot.alwaysPullImagesEnabled` | `boolean`, required | AlwaysPullImagesEnabled records whether kube-apiserver runs the AlwaysPullImages admission plugin. AlwaysPullImagesEnabled records whether the cluster rewrites every imagePullPolicy to Always. |
| `status.activeOperation.admissionSnapshot.defaultNotReadyTolerationSeconds` | `integer`, required | DefaultNotReadyTolerationSeconds is that plugin's not-ready value. |
| `status.activeOperation.admissionSnapshot.defaultTolerationsEnabled` | `boolean`, required | DefaultTolerationsEnabled records whether kube-apiserver runs the DefaultTolerationSeconds admission plugin. DefaultTolerationsEnabled and the two values below record what the cluster's DefaultTolerationSeconds plugin does, so a toleration the Pod did not ask for is recognized rather than refused. |
| `status.activeOperation.admissionSnapshot.defaultUnreachableTolerationSeconds` | `integer`, required | DefaultUnreachableTolerationSeconds is that plugin's unreachable value. |
| `status.activeOperation.admissionSnapshot.digest` | `string`, required | Digest covers this whole snapshot, so a Pod can be checked against it without re-reading the cluster objects it describes. |
| `status.activeOperation.admissionSnapshot.extendedResourceTolerationEnabled` | `boolean`, required | ExtendedResourceTolerationEnabled records whether kube-apiserver runs the ExtendedResourceToleration admission plugin. ExtendedResourceTolerationEnabled records whether the cluster adds a toleration per extended resource a Pod requests. |
| `status.activeOperation.admissionSnapshot.limitRanges` | `[]object` | LimitRanges are the namespace defaults that would be applied to the Pod. |
| `status.activeOperation.admissionSnapshot.limitRanges[].defaultLimits` | `object` | DefaultLimits are the limits it would add. |
| `status.activeOperation.admissionSnapshot.limitRanges[].defaultRequests` | `object` | DefaultRequests are the requests it would add to a container that asks for none. |
| `status.activeOperation.admissionSnapshot.limitRanges[].object` | `object`, required | Object is the LimitRange this was read from. |
| `status.activeOperation.admissionSnapshot.limitRanges[].object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.activeOperation.admissionSnapshot.limitRanges[].object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.limitRanges[].object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.activeOperation.admissionSnapshot.priorityClass` | `object`, required | PriorityClass is the scheduling priority it resolved to. |
| `status.activeOperation.admissionSnapshot.priorityClass.name` | `string` | Name is that class, as the Pod requests it. |
| `status.activeOperation.admissionSnapshot.priorityClass.object` | `object` | Object is the PriorityClass this was read from, where a class was named. |
| `status.activeOperation.admissionSnapshot.priorityClass.object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.activeOperation.admissionSnapshot.priorityClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.priorityClass.object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.activeOperation.admissionSnapshot.priorityClass.preemptionPolicy` | `string`, one of `Never`, `PreemptLowerPriority` | PreemptionPolicy is what that class says about preempting others. |
| `status.activeOperation.admissionSnapshot.priorityClass.value` | `integer`, required | Value is the priority it resolved to. |
| `status.activeOperation.admissionSnapshot.runtimeClass` | `object` | RuntimeClass is the container runtime it resolved to, where one is named. |
| `status.activeOperation.admissionSnapshot.runtimeClass.handler` | `string`, required | Handler is the runtime handler it names. |
| `status.activeOperation.admissionSnapshot.runtimeClass.nodeSelector` | `object` | NodeSelector is the scheduling the class forces. |
| `status.activeOperation.admissionSnapshot.runtimeClass.object` | `object`, required | Object is the RuntimeClass this was read from, by name and UID. |
| `status.activeOperation.admissionSnapshot.runtimeClass.object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.activeOperation.admissionSnapshot.runtimeClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.runtimeClass.object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.activeOperation.admissionSnapshot.runtimeClass.overhead` | `object` | Overhead is the per-Pod resource overhead the class adds. |
| `status.activeOperation.admissionSnapshot.runtimeClass.overheadDefined` | `boolean` | OverheadDefined distinguishes an absent RuntimeClass overhead stanza from a present but empty one; Kubernetes admission preserves that distinction. OverheadDefined separates a class with no overhead from one whose overhead is zero. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations` | `[]object` | Tolerations are the tolerations it adds. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].effect` | `string` | Effect indicates the taint effect to match. Empty means match all taint effects. When specified, allowed values are NoSchedule, PreferNoSchedule and NoExecute. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].key` | `string` | Key is the taint key that the toleration applies to. Empty means match all taint keys. If the key is empty, operator must be Exists; this combination means to match all values and all keys. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].operator` | `string` | Operator represents a key's relationship to the value. Valid operators are Exists, Equal, Lt, and Gt. Defaults to Equal. Exists is equivalent to wildcard for value, so that a pod can tolerate all taints of a particular category. Lt and Gt perform numeric comparisons (requires feature gate TaintTolerationComparisonOperators). |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].tolerationSeconds` | `integer` | TolerationSeconds represents the period of time the toleration (which must be of effect NoExecute, otherwise this field is ignored) tolerates the taint. By default, it is not set, which means tolerate the taint forever (do not evict). Zero and negative values will be treated as 0 (evict immediately) by the system. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].value` | `string` | Value is the taint value the toleration matches to. If the operator is Exists, the value should be empty, otherwise just a regular string. |
| `status.activeOperation.admissionSnapshot.serviceAccount` | `object`, required | ServiceAccount is the identity the Pod runs as, as it resolved. |
| `status.activeOperation.admissionSnapshot.serviceAccount.imagePullSecrets` | `[]object` | ImagePullSecrets are the pull Secrets it contributes to the Pod. |
| `status.activeOperation.admissionSnapshot.serviceAccount.imagePullSecrets[].name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.admissionSnapshot.serviceAccount.object` | `object`, required | Object is the ServiceAccount the Pod runs as. |
| `status.activeOperation.admissionSnapshot.serviceAccount.object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.activeOperation.admissionSnapshot.serviceAccount.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.serviceAccount.object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.activeOperation.admissionSnapshot.templateDigest` | `string`, required | TemplateDigest binds the canonical, API-defaulted pre-admission Job Pod template. The self-referential snapshot annotation and four exact API-server-generated Job identity labels are omitted and validated separately against the current Job name and UID. TemplateDigest covers the Pod template the operator asked for, before the cluster's own admission had a chance to change it. |
| `status.activeOperation.admissionSnapshot.version` | `string`, required, one of `v1` | Version is the snapshot format this record was written in. |
| `status.activeOperation.approvalRef` | `object` | ApprovalRef is the approval that authorized this Apply, recorded before dispatch so the run is attributable to the decision that permitted it. |
| `status.activeOperation.approvalRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `status.activeOperation.approvalRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `status.activeOperation.attempt` | `integer`, required | Attempt counts this claim among the retries of the same operation. |
| `status.activeOperation.coordinationDigest` | `string` | CoordinationDigest is that realm, hashed. |
| `status.activeOperation.dispatchNotAfter` | `string` | DispatchNotAfter and ExecutionNotAfter bound the claim in time. |
| `status.activeOperation.dispatchStarted` | `boolean` | DispatchStarted records that the one permitted Job create attempt was made. An Apply that crossed this boundary is never recreated, because whether it ran is a question for the database rather than for a retry. |
| `status.activeOperation.executionBindingID` | `string` | ExecutionBindingID is the epoch this claim was authorized under. A rollout that changes any execution component retires the claim rather than letting its Job finish under new bytes. |
| `status.activeOperation.executionNotAfter` | `string` | ExecutionNotAfter is when the authorized run itself expires. |
| `status.activeOperation.id` | `string`, required | ID is this attempt's identity, distinct from every other attempt of the same operation. |
| `status.activeOperation.inputFingerprint` | `string`, required | InputFingerprint is what the operation was decided from. An input that changed while the Job ran is what makes its result stale rather than wrong. |
| `status.activeOperation.jobName` | `string`, required | JobName is the deterministic name this claim's Job takes. It is written before the Job is created. |
| `status.activeOperation.jobUID` | `string` | JobUID is the exact Job the claim is bound to, once one exists. A Job with the right name and another UID is a different Job. |
| `status.activeOperation.leaseContinuityLost` | `boolean` | LeaseContinuityLost records that the epoch changed under this claim. |
| `status.activeOperation.leaseDurationSeconds` | `integer` | LeaseDurationSeconds is how long that acquisition was taken for. |
| `status.activeOperation.leaseEpoch` | `string` | LeaseEpoch is the database lock acquisition this claim was authorized under, and LeaseDurationSeconds how long that acquisition was taken for. A result produced across an epoch change is discarded rather than read: the lock it held was somebody else's by then. |
| `status.activeOperation.planRef` | `object` | PlanRef is the immutable plan an Apply carries out. |
| `status.activeOperation.planRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `status.activeOperation.planRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `status.activeOperation.source` | `object` | Source is the credential-free artifact binding this operation uses: the resolved digest and the selectors needed to fetch it. Every operation after Resolve carries one, so a newer generation cannot send newly selected credentials to the old artifact's registry. |
| `status.activeOperation.source.digest` | `string`, required | Digest is that digest on its own. |
| `status.activeOperation.source.registryAuthFrom` | `object` | RegistryAuthFrom names the Secret an operation Pod reads the registry credential from. It is a selector, never the credential. |
| `status.activeOperation.source.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` | DockerConfigJSONKey is the Secret key holding a Docker config document. |
| `status.activeOperation.source.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | Mode says how the credential reaches the executor: as environment variables, or as a Docker config file. |
| `status.activeOperation.source.registryAuthFrom.name` | `string`, required | Name of the Secret the registry credential is read from. The manager never reads it; the operation Pod does. |
| `status.activeOperation.source.registryAuthFrom.passwordKey` | `string`, default `password` | PasswordKey is the Secret key holding the password. |
| `status.activeOperation.source.registryAuthFrom.registryKey` | `string`, one of `registry`, default `registry` | RegistryKey is retained for source compatibility. The key is fixed so the Secret owner, rather than a PtahSchema author, controls the authority grant. The referenced Secret must contain an authority-only host[:port] value. RegistryKey is the Secret key naming the registry the credential is for. |
| `status.activeOperation.source.registryAuthFrom.tokenKey` | `string`, default `token` | TokenKey is the Secret key holding a bearer token, where one is used instead of a username and password. |
| `status.activeOperation.source.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. UsernameKey is the Secret key holding the username. |
| `status.activeOperation.source.resolvedReference` | `string`, required | ResolvedReference is the artifact with its tag replaced by a digest. |
| `status.activeOperation.source.transport` | `object` | Transport is how the registry is reached: plain HTTP, a custom CA. |
| `status.activeOperation.source.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `status.activeOperation.source.transport.caFrom.key` | `string`, required | The key to select. |
| `status.activeOperation.source.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.source.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `status.activeOperation.source.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `status.activeOperation.source.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` | CertificateKey is the Secret key holding the certificate. |
| `status.activeOperation.source.transport.clientCertificateFrom.name` | `string`, required | Name of the Secret holding the client certificate. |
| `status.activeOperation.source.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` | PrivateKeyKey is the Secret key holding its private key. |
| `status.activeOperation.source.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `status.activeOperation.startedAt` | `string`, required | StartedAt is when the claim was written, which is before the Job exists. |
| `status.activeOperation.target` | `object` | Target is the key-free database binding, and CoordinationDigest the realm the operation serializes against. |
| `status.activeOperation.target.engine` | `string`, required | Engine is the database this binding speaks. |
| `status.activeOperation.target.urlFrom` | `object`, required | URLFrom is the Secret key the operation Pod reads the URL from. |
| `status.activeOperation.target.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.activeOperation.target.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.target.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.activeOperation.type` | `string`, required, one of `Resolve`, `Verify`, `History`, `Apply` | Type is the operation this claim authorizes. |
| `status.artifact` | `object` | Artifact is the resolved, credential-free artifact binding every later operation of this cycle uses. A tag resolves once; the digest is what travels. |
| `status.artifact.digest` | `string`, required | Digest is that digest on its own. |
| `status.artifact.registryAuthFrom` | `object` | RegistryAuthFrom names the Secret an operation Pod reads the registry credential from. It is a selector, never the credential. |
| `status.artifact.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` | DockerConfigJSONKey is the Secret key holding a Docker config document. |
| `status.artifact.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | Mode says how the credential reaches the executor: as environment variables, or as a Docker config file. |
| `status.artifact.registryAuthFrom.name` | `string`, required | Name of the Secret the registry credential is read from. The manager never reads it; the operation Pod does. |
| `status.artifact.registryAuthFrom.passwordKey` | `string`, default `password` | PasswordKey is the Secret key holding the password. |
| `status.artifact.registryAuthFrom.registryKey` | `string`, one of `registry`, default `registry` | RegistryKey is retained for source compatibility. The key is fixed so the Secret owner, rather than a PtahSchema author, controls the authority grant. The referenced Secret must contain an authority-only host[:port] value. RegistryKey is the Secret key naming the registry the credential is for. |
| `status.artifact.registryAuthFrom.tokenKey` | `string`, default `token` | TokenKey is the Secret key holding a bearer token, where one is used instead of a username and password. |
| `status.artifact.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. UsernameKey is the Secret key holding the username. |
| `status.artifact.resolvedReference` | `string`, required | ResolvedReference is the artifact with its tag replaced by a digest. |
| `status.artifact.transport` | `object` | Transport is how the registry is reached: plain HTTP, a custom CA. |
| `status.artifact.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `status.artifact.transport.caFrom.key` | `string`, required | The key to select. |
| `status.artifact.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.artifact.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `status.artifact.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `status.artifact.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` | CertificateKey is the Secret key holding the certificate. |
| `status.artifact.transport.clientCertificateFrom.name` | `string`, required | Name of the Secret holding the client certificate. |
| `status.artifact.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` | PrivateKeyKey is the Secret key holding its private key. |
| `status.artifact.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `status.conditions` | `[]object` | Conditions are the readable verdicts: whether the artifact resolved and verified, whether the history could be read, whether a plan is ready, whether an approval is required, and whether the sequence is in sync. |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.executionBinding` | `object` | ExecutionBinding is the component identity this resource's work is bound to: the manager that authorized it and the executor and runner that will carry it out. It is the same contract the schema path publishes, because the question it answers is the same one: a rollout that changed any of them has to invalidate a plan rather than execute it under new bytes. |
| `status.executionBinding.controllerImage` | `string` | ControllerImage identifies the exact manager container content that interpreted controller state and authorized this evidence epoch. |
| `status.executionBinding.controllerRevision` | `string` | ControllerRevision identifies the exact manager build that interpreted controller state. It is provenance metadata in addition to ControllerImage, not a substitute for the image content digest. |
| `status.executionBinding.controllerStateVersion` | `integer` | ControllerStateVersion versions manager-side reconciliation semantics independently of the data-plane runner protocol. |
| `status.executionBinding.epoch` | `string`, required | Epoch is this binding's identity. It changes on every component transition, a rollback to identical versions included, so evidence from before a rollout is historical rather than current. |
| `status.executionBinding.executorImage` | `string`, required | ExecutorImage is the digest-pinned image carrying that build. |
| `status.executionBinding.ptahVersion` | `string`, required | PtahVersion is the Ptah build this epoch executes with. |
| `status.executionBinding.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervises it. |
| `status.executionBinding.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. |
| `status.history` | `object` | History is the last reading of the database's own revision table. |
| `status.history.appliedCount` | `integer`, required | AppliedCount and PendingCount describe the artifact against this history. |
| `status.history.checkpointVersion` | `integer` | CheckpointVersion is the checkpoint covering the versions below it, and zero where none applies. It stays set after the bootstrap has run: the coverage is what keeps those versions applied rather than pending. |
| `status.history.contractVersion` | `integer`, required | ContractVersion is the version of Ptah's status document this summary was read from. A version the controller does not know is refused before the database is touched rather than read as if it meant the same thing. |
| `status.history.currentVersion` | `integer`, required | CurrentVersion is the highest version the revision table records. |
| `status.history.dirty` | `boolean` | Dirty reports a revision row a failed or interrupted run left behind. Nothing applies while one exists. |
| `status.history.fingerprint` | `string`, required | Fingerprint is this exact reading of the revision table. A plan names it as its premise, and a history that moved between planning and execution invalidates the plan rather than being applied to. |
| `status.history.modifiedVersions` | `[]integer` | ModifiedVersions names applied migrations whose files no longer account for them. It is the refusal a versioned workflow exists to make, so the versions are published rather than summarized. |
| `status.history.observedAt` | `string`, required | ObservedAt is when the history was read. |
| `status.history.outOfOrderVersions` | `[]integer` | OutOfOrderVersions names migrations the artifact carries below a version the database has already applied. Linear execution refuses them, so the versions are published rather than counted: what a person decides here depends on which migration arrived late. |
| `status.history.pendingCount` | `integer`, required | PendingCount is how many the artifact still has to apply, as Ptah selects them rather than as a subtraction of the two counts. |
| `status.history.targetIdentityDigest` | `string`, required | TargetIdentityDigest is the credential-free identity of the database this history was read from, as the executor derived it. A plan that was made against one database is never executed against another. |
| `status.lastRun` | `object` | LastRun is the evidence of the most recent execution, kept across later reconciliations so an operator can see what happened without the Job. |
| `status.lastRun.appliedVersions` | `[]integer` | AppliedVersions names the selected migrations the history recorded afterwards, so a migration the history already held is not reported as this run's work. |
| `status.lastRun.finishedAt` | `string` | FinishedAt is when its result was read. An unfinished run has none. |
| `status.lastRun.jobName` | `string` | JobName and JobUID identify the execution this evidence came from. The UID is what makes a replacement Job with the same name a different run. |
| `status.lastRun.jobUID` | `string` | JobUID is that Job's UID. |
| `status.lastRun.message` | `string` | Message is a safe explanation. It never carries database rows, and never carries the SQL a migration ran. |
| `status.lastRun.outcome` | `string`, required, one of `UpToDate`, `Applied`, `Failed`, `Partial`, `Unknown` | Outcome is the verdict Ptah read from the revision table. |
| `status.lastRun.startedAt` | `string`, required | StartedAt is when the run began. |
| `status.nextReconciliationTime` | `string` | NextReconciliationTime is when the controller intends to look again. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the spec generation this status describes. |
| `status.phase` | `string`, one of `Pending`, `Resolving`, `Verifying`, `Reading`, `Planning`, `AwaitingApproval`, `Blocked`, `Applying`, `VerifyingHistory`, `InSync`, `Suspended`, `Failed` | Phase is where the resource stands. It is a summary for a reader: the conditions below are what a decision reads. |
| `status.plan` | `object` | Plan names the immutable plan object the controller published for the current pending sequence, and is cleared once that sequence is gone. |
| `status.plan.name` | `string`, required | Name of the referenced object in the same namespace. |
| `status.plan.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `status.unresolvedRun` | `object` | UnresolvedRun is the execution nobody could account for, and is absent while there is none. It is written when a run ends Partial or Unknown, and removed only when a read-only reading of the same database finds nothing of this artifact left to apply. While it is here nothing is planned and nothing runs, whatever the conditions happen to say. |
| `status.unresolvedRun.jobName` | `string` | JobName and JobUID identify the execution. The UID is what makes a replacement Job with the same name a different run. |
| `status.unresolvedRun.jobUID` | `string` | JobUID is that Job's UID. |
| `status.unresolvedRun.operationID` | `string` | OperationID is the Apply claim that ran, so this record names one attempt rather than the resource in general. |
| `status.unresolvedRun.outcome` | `string`, required, one of `UpToDate`, `Applied`, `Failed`, `Partial`, `Unknown` | Outcome is what the run's own evidence said, and is always Partial or Unknown: Partial committed some of a migration's statements and not the rest, and Unknown could not be read at all. No other outcome leaves the database in a state nobody can name, so no other outcome is recorded here. |
| `status.unresolvedRun.planRef` | `object` | PlanRef names the plan the run was carrying out, which is the work that may have reached the database. |
| `status.unresolvedRun.planRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `status.unresolvedRun.planRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `status.unresolvedRun.recordedAt` | `string`, required | RecordedAt is when the controller wrote this record. |
| `status.unresolvedRun.targetIdentityDigest` | `string` | TargetIdentityDigest is the credential-free identity of the database the run was dispatched against, taken from the claim rather than from the run: an unresolved run is one whose own account was never read, so what it reached is exactly what is unknown. The reading that settles this record has to be of that database: a history read somewhere else says nothing about what this run did. |

