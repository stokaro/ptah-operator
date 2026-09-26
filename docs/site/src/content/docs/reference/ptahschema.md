---
title: PtahSchema
description: Every field of the PtahSchema resource, generated from the API types.
---

`PtahSchema` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

Three shapes, from the least a cluster accepts to the one a shared production
database needs. Every field not named here takes the default the table below
records.

### The smallest resource that runs

A target, a desired-state artifact, and the policy the artifact must satisfy.
Nothing applies by itself: `spec.policy.apply` defaults to `OnApproval`, so
this resource plans, reports the drift it found, and waits for a
`PtahSchemaApproval` naming that plan.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: application
spec:
  target:
    engine: PostgreSQL
    # Every resource that can write to this physical database must use this
    # exact key, whatever DNS alias, proxy or credential it reaches it through.
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  desired:
    ociRef: oci://ghcr.io/example/application-schema:1.4.0
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
```

### Unattended, where nobody is waiting to approve

`apply: Always` skips the approval and applies what it planned. It stays safe
to leave running because the two fences below it hold: a destructive change is
refused rather than applied, and a table named in `protectedTables` is refused
even when it is not.

Suitable for a development or staging database. On a production database it
means an artifact push is a schema change with no person between the two.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: staging
spec:
  target:
    engine: PostgreSQL
    coordinationKey: staging/application-primary
    urlFrom:
      name: application-database
      key: url
  desired:
    ociRef: oci://ghcr.io/example/application-schema:main
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  interval: 2m
  policy:
    apply: Always
    # A plan that drops or rewrites anything is refused, not applied.
    allowDestructive: false
    # Report drift only where it destroys something, so an unattended resource
    # is not noisy about additions it is about to make anyway.
    driftSeverity: destructive
    # Tables the declaration does not own. Anything else changes them.
    exclude:
      - schema_migrations
```

### A database more than one resource manages

`sharedRealm` is the declaration that taking turns is intended. Every claimant
of the same `coordinationKey` has to set it: with one left `false`, all of them
are refused rather than allowed to undo each other's work.

`protectedTables` is a fence with no override. A plan that would touch one of
these leaves the resource `Blocked` with reason `ProtectedTable`, and no
approval and no policy lifts it.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    # The PtahMigration over the same database says this too.
    sharedRealm: true
    urlFrom:
      name: application-database
      key: url
  desired:
    # A digest pins the artifact for good; a tag is resolved to one per cycle.
    ociRef: oci://ghcr.io/example/application-schema@sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  policy:
    apply: OnApproval
    allowDestructive: false
    protectedTables:
      - billing_ledger
      - audit_log
  interval: 10m
  execution:
    activeDeadlineSeconds: 900
    # The identity the operation Jobs run as, which is never the controller's
    # own: a Job that could act as the controller could write the status that
    # judges it.
    serviceAccountName: ptah-execution
```

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.desired` | `object`, required | This resource requires: desired.verificationPolicyFrom must name a required ConfigMap key; where `desired.transport` and `desired.transport.caFrom` are set, desired.transport.caFrom must name a required ConfigMap key. Desired is the OCI artifact that declares the schema, and the rows a declaration names, to converge it to. |
| `spec.desired.ociRef` | `string`, required | OCIRef is a desired-schema artifact reference. It may name a tag or a digest; every later operation receives only the resolved digest. |
| `spec.desired.registryAuthFrom` | `object` | RegistryAuthFrom names the Secret an operation Pod reads the registry credential from. The process that runs SQL never receives it. |
| `spec.desired.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` | DockerConfigJSONKey is the Secret key holding a Docker config document. |
| `spec.desired.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | Mode says how the credential reaches the executor: as environment variables, or as a Docker config file. |
| `spec.desired.registryAuthFrom.name` | `string`, required | Name of the Secret the registry credential is read from. The manager never reads it; the operation Pod does. |
| `spec.desired.registryAuthFrom.passwordKey` | `string`, default `password` | PasswordKey is the Secret key holding the password. |
| `spec.desired.registryAuthFrom.tokenKey` | `string`, default `token` | TokenKey is the Secret key holding a bearer token, where one is used instead of a username and password. |
| `spec.desired.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. UsernameKey is the Secret key holding the username. |
| `spec.desired.transport` | `object` | This resource requires: where `desired.transport` and `desired.transport.caFrom` are set, desired.transport.caFrom must name a required ConfigMap key. Transport is how the registry is reached: plain HTTP, a custom CA, a client certificate. |
| `spec.desired.transport.caFrom` | `object` | This resource requires: where `desired.transport` and `desired.transport.caFrom` are set, desired.transport.caFrom must name a required ConfigMap key. CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `spec.desired.transport.caFrom.key` | `string`, required | This resource requires: where `desired.transport` and `desired.transport.caFrom` are set, desired.transport.caFrom must name a required ConfigMap key. The key to select from the ConfigMap's Data field. Keys in the BinaryData field are not currently propagated to container env vars. |
| `spec.desired.transport.caFrom.name` | `string`, default `` | This resource requires: where `desired.transport` and `desired.transport.caFrom` are set, desired.transport.caFrom must name a required ConfigMap key. Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.desired.transport.caFrom.optional` | `boolean` | This resource requires: where `desired.transport` and `desired.transport.caFrom` are set, desired.transport.caFrom must name a required ConfigMap key. Specify whether the ConfigMap or its key must be defined |
| `spec.desired.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `spec.desired.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` | CertificateKey is the Secret key holding the certificate. |
| `spec.desired.transport.clientCertificateFrom.name` | `string`, required | Name of the Secret holding the client certificate. |
| `spec.desired.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` | PrivateKeyKey is the Secret key holding its private key. |
| `spec.desired.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `spec.desired.verificationPolicyFrom` | `object`, required | This resource requires: desired.verificationPolicyFrom must name a required ConfigMap key. VerificationPolicyFrom names the immutable ConfigMap holding the policy the artifact must satisfy. Editing it retires the plans computed under the previous version rather than letting them apply. |
| `spec.desired.verificationPolicyFrom.key` | `string`, required | This resource requires: desired.verificationPolicyFrom must name a required ConfigMap key. The key to select from the ConfigMap's Data field. Keys in the BinaryData field are not currently propagated to container env vars. |
| `spec.desired.verificationPolicyFrom.name` | `string`, default `` | This resource requires: desired.verificationPolicyFrom must name a required ConfigMap key. Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.desired.verificationPolicyFrom.optional` | `boolean` | This resource requires: desired.verificationPolicyFrom must name a required ConfigMap key. Specify whether the ConfigMap or its key must be defined |
| `spec.dev` | `object` | This resource requires: where `dev` is set, dev.urlFrom must name a required Secret key. Dev is a scratch database Ptah may use where a comparison needs one. It is never the target, and nothing it holds is kept. |
| `spec.dev.urlFrom` | `object`, required | This resource requires: where `dev` is set, dev.urlFrom must name a required Secret key. URLFrom names the Secret key holding this database's connection URL. |
| `spec.dev.urlFrom.key` | `string`, required | This resource requires: where `dev` is set, dev.urlFrom must name a required Secret key. The key of the secret to select from. Must be a valid secret key. |
| `spec.dev.urlFrom.name` | `string`, default `` | This resource requires: where `dev` is set, dev.urlFrom must name a required Secret key. Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.dev.urlFrom.optional` | `boolean` | This resource requires: where `dev` is set, dev.urlFrom must name a required Secret key. Specify whether the Secret or its key must be defined |
| `spec.execution` | `object`, default `{}` | This resource requires: where `execution`, `execution.connectTimeout` and `execution.activeDeadlineSeconds` are set, execution.connectTimeout must not exceed execution.activeDeadlineSeconds; where `policy`, `policy.lockTimeout`, `execution` and `execution.activeDeadlineSeconds` are set, policy.lockTimeout must not exceed execution.activeDeadlineSeconds. Default the object itself so the API server also applies the nested execution defaults when a manifest omits the whole block. |
| `spec.execution.activeDeadlineSeconds` | `integer`, default `900` | This resource requires: where `execution`, `execution.connectTimeout` and `execution.activeDeadlineSeconds` are set, execution.connectTimeout must not exceed execution.activeDeadlineSeconds; where `policy`, `policy.lockTimeout`, `execution` and `execution.activeDeadlineSeconds` are set, policy.lockTimeout must not exceed execution.activeDeadlineSeconds. ActiveDeadlineSeconds is how long one operation Job may run before Kubernetes ends it. An apply that hits this leaves an uncertain outcome, which returns to observation rather than to a replay. |
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
| `spec.execution.connectTimeout` | `string`, default `10s` | This resource requires: where `execution`, `execution.connectTimeout` and `execution.activeDeadlineSeconds` are set, execution.connectTimeout must not exceed execution.activeDeadlineSeconds. ConnectTimeout bounds opening the database connection, so an unreachable database fails in seconds rather than holding the Job to its deadline. |
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
| `spec.interval` | `string`, default `10m` | Interval is the cadence for resolving mutable tags and observing drift. |
| `spec.policy` | `object`, default `{}` | This resource requires: where `policy`, `policy.lockTimeout`, `execution` and `execution.activeDeadlineSeconds` are set, policy.lockTimeout must not exceed execution.activeDeadlineSeconds. Policy decides what may happen without a person: whether a plan applies itself, whether a destructive one is permitted at all, what counts as drift, and which tables are fenced off entirely. |
| `spec.policy.allowDestructive` | `boolean`, default `false` | AllowDestructive permits a plan that drops or rewrites something. It is permission for the category, not for a plan: a destructive plan still needs an approval where the apply policy asks for one. |
| `spec.policy.apply` | `string`, one of `Never`, `OnApproval`, `Always`, default `OnApproval` | Apply decides when a current plan may run: never, only with an approval naming its exact bytes, or as soon as it is ready. |
| `spec.policy.driftSeverity` | `string`, one of `all`, `destructive`, default `all` | DriftSeverity decides which differences count as drift worth applying: every difference, or only the destructive ones. |
| `spec.policy.exclude` | `[]string` | Exclude defines the single authoritative managed scope. Raw drift is observed without exclusions, then a read-only plan classifies this exact scope as changed or converged. Exclude is the managed scope the apply was computed under. |
| `spec.policy.lockTimeout` | `string`, default `30s` | This resource requires: where `policy`, `policy.lockTimeout`, `execution` and `execution.activeDeadlineSeconds` are set, policy.lockTimeout must not exceed execution.activeDeadlineSeconds. LockTimeout is how long an operation waits for the database's own lock before giving up, so a busy database delays a run rather than stalling it for the Job's whole deadline. |
| `spec.policy.protectedTables` | `[]string` | ProtectedTables fences declared row sets off from the declarative path. A plan that would change a listed table is refused rather than rated, and there is no override: an approval, allowDestructive and a permissive severity are all answers to "how risky is this", and a fence is the statement that no such answer exists for these rows. Where the change is wanted, the entry goes, or the rows are written as a migration. An entry names a table, or a schema and a table, the way the declaration does. Matching is Ptah's, which is case-insensitive, and an entry on a table the artifact already agrees with refuses nothing -- which is what lets a fence sit in a policy permanently. |
| `spec.policy.transactionMode` | `string`, one of `all`, `file`, `none`, default `file` | TransactionMode is how the statements are wrapped: all in one transaction, one per file, or none at all. An engine that refuses a mode decides over this rather than around it. |
| `spec.suspend` | `boolean`, default `false` | Suspend prevents new Jobs. A Job already applying is observed to a terminal result and is never replaced by a destructive cleanup action. |
| `spec.target` | `object`, required | This resource requires: target.urlFrom must name a required Secret key. Target is the database to converge, named through a Secret the manager itself has no permission to read. |
| `spec.target.coordinationKey` | `string`, required | CoordinationKey is a non-secret, stable identifier for the physical database realm. Every schema that can reach the same database through an alias, proxy, or different credential must use exactly the same key. |
| `spec.target.engine` | `string`, required | Engine is the database this target speaks. An engine outside the supported set is refused with a condition rather than attempted. |
| `spec.target.sharedRealm` | `boolean`, default `false` | SharedRealm declares that this resource manages only part of the database its coordination key names, and that every other resource managing that database has declared the same. It defaults to false. A realm more than one resource claims is refused while any claimant leaves it false -- including the resource that did declare it. Deleting a resource leaves the realm, and so does suspending it; resuming puts it back, and the conflict is refused then, before any Job. What is verified is the declaration, never the disjointness: nothing can tell whether two sets of arbitrary SQL touch the same rows. The concurrency section of the architecture reference says why taking turns is not enough. |
| `spec.target.urlFrom` | `object`, required | This resource requires: target.urlFrom must name a required Secret key. URLFrom names the Secret key holding the connection URL. The manager has no permission to read it: the operation Pod resolves it, and the URL never reaches status, an Event or a command line. |
| `spec.target.urlFrom.key` | `string`, required | This resource requires: target.urlFrom must name a required Secret key. The key of the secret to select from. Must be a valid secret key. |
| `spec.target.urlFrom.name` | `string`, default `` | This resource requires: target.urlFrom must name a required Secret key. Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.target.urlFrom.optional` | `boolean` | This resource requires: target.urlFrom must name a required Secret key. Specify whether the Secret or its key must be defined |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.activeOperation` | `object` | ActiveOperation is the claim for the operation in flight. It is written before the Job exists, which is what lets the controller tell a Job it created from one it has not created yet. |
| `status.activeOperation.admissionSnapshot` | `object` | AdmissionSnapshot is persisted before dispatch and is bound into the Job and Pod template annotations. It permits only modeled, safe built-in admission mutations while retaining exact validation for executable and security-sensitive Pod fields. |
| `status.activeOperation.admissionSnapshot.alwaysPullImagesEnabled` | `boolean`, required | AlwaysPullImagesEnabled records whether kube-apiserver runs the AlwaysPullImages admission plugin, which rewrites every imagePullPolicy to Always. |
| `status.activeOperation.admissionSnapshot.defaultNotReadyTolerationSeconds` | `integer`, required | DefaultNotReadyTolerationSeconds is that plugin's not-ready value. |
| `status.activeOperation.admissionSnapshot.defaultTolerationsEnabled` | `boolean`, required | DefaultTolerationsEnabled records whether kube-apiserver runs the DefaultTolerationSeconds admission plugin. It and the two values below say what that plugin does, so a toleration the Pod did not ask for is recognized rather than refused. |
| `status.activeOperation.admissionSnapshot.defaultUnreachableTolerationSeconds` | `integer`, required | DefaultUnreachableTolerationSeconds is that plugin's unreachable value. |
| `status.activeOperation.admissionSnapshot.digest` | `string`, required | Digest covers this whole snapshot, so a Pod can be checked against it without re-reading the cluster objects it describes. |
| `status.activeOperation.admissionSnapshot.extendedResourceTolerationEnabled` | `boolean`, required | ExtendedResourceTolerationEnabled records whether kube-apiserver runs the ExtendedResourceToleration admission plugin, which adds a toleration per extended resource a Pod requests. |
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
| `status.activeOperation.attempt` | `integer`, required | Attempt counts this claim among the retries of the same operation. |
| `status.activeOperation.coordinationDigest` | `string` | Plan and Apply operations persist their credential-free lock binding so later spec changes cannot redirect or shorten protection for a running Job. |
| `status.activeOperation.dispatchNotAfter` | `string` | DispatchNotAfter is the immutable last instant at which the Apply runner may start its mutating child. Missing or untrusted terminal Pod evidence keeps proof behind the complete execution horizon below. |
| `status.activeOperation.dispatchStarted` | `boolean` | DispatchStarted is persisted immediately before the one permitted Job create attempt. A missing Apply Job after this boundary is outcome-unknown and must never be recreated. |
| `status.activeOperation.executionBindingID` | `string` | ExecutionBindingID binds every Job and result to the durable evidence epoch that authorized its claim. |
| `status.activeOperation.executionNotAfter` | `string` | ExecutionNotAfter is enforced by the runner as the mutating child context deadline, independently of relative Job and Pod deadlines. |
| `status.activeOperation.id` | `string`, required | ID is this attempt's identity, distinct from every other attempt. |
| `status.activeOperation.inputFingerprint` | `string`, required | InputFingerprint is everything the claim was computed from. A changed input produces a new claim rather than reusing this one. |
| `status.activeOperation.jobName` | `string`, required | JobName is the Job this claim authorizes, named before it is created. |
| `status.activeOperation.jobUID` | `string` | JobUID is that Job's UID once it exists. A Job with the right name and another UID is somebody else's. |
| `status.activeOperation.leaseContinuityLost` | `boolean` | LeaseContinuityLost is set before any result can be harvested when the persisted epoch no longer owns an uninterrupted lock interval. |
| `status.activeOperation.leaseDurationSeconds` | `integer` | LeaseDurationSeconds is how long the database lock was taken for. |
| `status.activeOperation.leaseEpoch` | `string` | LeaseEpoch is persisted before dispatch. A different epoch invalidates every result produced by this operation. |
| `status.activeOperation.observationConnectTimeout` | `string` | ObservationConnectTimeout is the connect timeout it was dispatched with. |
| `status.activeOperation.observationDev` | `object` | ObservationDev is the scratch database it may use, where one is configured. |
| `status.activeOperation.observationDev.urlFrom` | `object`, required | URLFrom names the Secret key holding this database's connection URL. |
| `status.activeOperation.observationDev.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.activeOperation.observationDev.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.observationDev.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.activeOperation.observationExclude` | `[]string` | ObservationExclude is the managed scope the operation was given, copied so a later spec edit cannot change what a running Job was asked to read. |
| `status.activeOperation.observationLockTimeout` | `string` | ObservationLockTimeout is the database lock timeout it was dispatched with. |
| `status.activeOperation.observationProtectedTables` | `[]string` | ObservationProtectedTables is the fence the operation was dispatched under, so an edit to the policy cannot reach a Job already running. |
| `status.activeOperation.observationSeverity` | `string` | ObservationSeverity is the drift severity it was asked to report. |
| `status.activeOperation.source` | `object` | Source snapshots artifact access for mandatory post-Apply observation. It contains Kubernetes selectors only, never Secret contents. |
| `status.activeOperation.source.digest` | `string`, required | Digest is that digest on its own. |
| `status.activeOperation.source.registryAuthFrom` | `object` | RegistryAuthFrom names the Secret an operation Pod reads the registry credential from. It is a selector, never the credential. |
| `status.activeOperation.source.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` | DockerConfigJSONKey is the Secret key holding a Docker config document. |
| `status.activeOperation.source.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | Mode says how the credential reaches the executor: as environment variables, or as a Docker config file. |
| `status.activeOperation.source.registryAuthFrom.name` | `string`, required | Name of the Secret the registry credential is read from. The manager never reads it; the operation Pod does. |
| `status.activeOperation.source.registryAuthFrom.passwordKey` | `string`, default `password` | PasswordKey is the Secret key holding the password. |
| `status.activeOperation.source.registryAuthFrom.tokenKey` | `string`, default `token` | TokenKey is the Secret key holding a bearer token, where one is used instead of a username and password. |
| `status.activeOperation.source.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. UsernameKey is the Secret key holding the username. |
| `status.activeOperation.source.resolvedReference` | `string`, required | ResolvedReference is the artifact with its tag replaced by a digest. |
| `status.activeOperation.source.transport` | `object` | Transport is how the registry is reached: plain HTTP, a custom CA. |
| `status.activeOperation.source.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `status.activeOperation.source.transport.caFrom.key` | `string`, required | The key to select from the ConfigMap's Data field. Keys in the BinaryData field are not currently propagated to container env vars. |
| `status.activeOperation.source.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.source.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `status.activeOperation.source.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `status.activeOperation.source.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` | CertificateKey is the Secret key holding the certificate. |
| `status.activeOperation.source.transport.clientCertificateFrom.name` | `string`, required | Name of the Secret holding the client certificate. |
| `status.activeOperation.source.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` | PrivateKeyKey is the Secret key holding its private key. |
| `status.activeOperation.source.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `status.activeOperation.startedAt` | `string`, required | StartedAt is when the claim was written. |
| `status.activeOperation.target` | `object` | Target is the key-free binding the Job resolves its credential through. |
| `status.activeOperation.target.engine` | `string`, required | Engine is the database this binding speaks. |
| `status.activeOperation.target.urlFrom` | `object`, required | URLFrom is the Secret key the operation Pod reads the URL from. |
| `status.activeOperation.target.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.activeOperation.target.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.target.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.activeOperation.targetIdentityDigest` | `string` | TargetIdentityDigest is the database that claim is for. |
| `status.activeOperation.terminationGracePeriodSeconds` | `integer` | TerminationGracePeriodSeconds is how long the operation Pod is given to stop on its own before it is killed. |
| `status.activeOperation.type` | `string`, required, one of `Resolve`, `Verify`, `Observe`, `Plan`, `Apply` | Type is the operation this claim authorizes: resolving the artifact, verifying it, observing the database, planning, or applying. |
| `status.activeOperation.verificationPolicyDigest` | `string` | VerificationPolicyDigest is that ConfigMap's content at dispatch. |
| `status.activeOperation.verificationPolicyUID` | `string` | VerificationPolicyUID and VerificationPolicyDigest bind a Verify Job to the immutable ConfigMap version inspected before dispatch. |
| `status.applied` | `object` | Applied is the last apply that was independently observed to have converged, which is a different claim from a Job that exited zero. |
| `status.applied.artifactDigest` | `string`, required | ArtifactDigest is the artifact that was applied. |
| `status.applied.completedAt` | `string`, required | CompletedAt is when convergence was independently observed, not when the Job exited. |
| `status.applied.controllerImage` | `string`, required | ControllerImage is the digest-pinned manager that dispatched it. |
| `status.applied.controllerRevision` | `string`, required | ControllerRevision is that manager's revision. |
| `status.applied.controllerStateVersion` | `integer`, required | ControllerStateVersion is the state semantics it wrote. |
| `status.applied.coordinationDigest` | `string`, required | CoordinationDigest is the realm the apply held while it ran. |
| `status.applied.executionBindingID` | `string`, required | ExecutionBindingID is the epoch the apply ran under. |
| `status.applied.executorImage` | `string`, required | ExecutorImage is the digest-pinned image it ran in. |
| `status.applied.planFingerprint` | `string`, required | PlanFingerprint says whether the plan object read today is the one this record was written for. |
| `status.applied.planRef` | `object`, required | PlanRef names the stored plan this apply ran. It is what a reader addresses to see the SQL that was applied. It does not replace PlanFingerprint: the reference says which object to read and the fingerprint says whether the object read is the one this record was written for. |
| `status.applied.planRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `status.applied.planRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `status.applied.ptahVersion` | `string`, required | PtahVersion is the Ptah build that executed the statements. |
| `status.applied.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervised it. |
| `status.applied.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner spoke. |
| `status.applied.targetIdentityDigest` | `string`, required | TargetIdentityDigest is the database it converged. |
| `status.conditions` | `[]object` | Conditions are the readable verdicts: whether the engine is supported, the artifact resolved and verified, the database was reachable, drift was found, a plan is ready, an approval is required, the schema is in sync, and whether the last reconciliation failed. |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.executionBinding` | `object` | ExecutionBinding is the durable identity of the controller/runtime epoch authorized to produce new reconciliation evidence. Retained evidence stays historical until refreshed. Epoch changes on every component transition, including a rollback to identical values. |
| `status.executionBinding.controllerImage` | `string`, required | ControllerImage identifies the exact manager container content that interpreted controller state and authorized this evidence epoch. |
| `status.executionBinding.controllerRevision` | `string`, required | ControllerRevision identifies the exact manager build that interpreted controller state. It is provenance metadata in addition to ControllerImage, not a substitute for the image content digest. |
| `status.executionBinding.controllerStateVersion` | `integer`, required | ControllerStateVersion versions manager-side reconciliation semantics independently of the data-plane runner protocol. |
| `status.executionBinding.epoch` | `string`, required | Epoch is this binding's identity. It changes on every component transition, a rollback to identical versions included, so evidence from before a rollout is historical rather than current. |
| `status.executionBinding.executorImage` | `string`, required | ExecutorImage is the digest-pinned image carrying that build. |
| `status.executionBinding.ptahVersion` | `string`, required | PtahVersion is the Ptah build this epoch executes with. |
| `status.executionBinding.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervises it. |
| `status.executionBinding.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. |
| `status.lastAttemptTime` | `string` | LastAttemptTime is when the controller last tried to do something. |
| `status.lastSuccessfulReconciliation` | `string` | LastSuccessfulReconciliation is when it last completed a cycle with nothing left to do. |
| `status.nextReconciliationTime` | `string` | NextReconciliationTime is the durable earliest time for the next scheduled read-only reconciliation. Event-driven safety work may run sooner. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the spec generation this status describes. |
| `status.pendingLockRelease` | `object` | PendingLockRelease keeps the exact Lease owner and epoch durable until an idempotent release succeeds. It closes the manager-crash window between a terminal status transition and clearing the owner-neutral Lease. |
| `status.pendingLockRelease.coordinationDigest` | `string`, required | CoordinationDigest is the realm whose lock is still to be released. |
| `status.pendingLockRelease.leaseDurationSeconds` | `integer`, required | LeaseDurationSeconds is how long it was taken for. |
| `status.pendingLockRelease.leaseEpoch` | `string`, required | LeaseEpoch identifies that acquisition, so a release cannot free a lock somebody else acquired in the meantime. |
| `status.pendingLockRelease.operationID` | `string`, required | OperationID is the operation that took it. |
| `status.pendingObservation` | `object` | PendingObservation is durable proof work created after an Apply Job may have mutated the database. It is independent of Phase so retries and suspension cannot accidentally permit another mutation first. |
| `status.pendingObservation.admissionSnapshot` | `object` | AdmissionSnapshot retains the exact pre-admission Pod template identity after ActiveOperation is cleared. Apply Job cleanup after an execution-binding change fails closed when this evidence is absent. |
| `status.pendingObservation.admissionSnapshot.alwaysPullImagesEnabled` | `boolean`, required | AlwaysPullImagesEnabled records whether kube-apiserver runs the AlwaysPullImages admission plugin, which rewrites every imagePullPolicy to Always. |
| `status.pendingObservation.admissionSnapshot.defaultNotReadyTolerationSeconds` | `integer`, required | DefaultNotReadyTolerationSeconds is that plugin's not-ready value. |
| `status.pendingObservation.admissionSnapshot.defaultTolerationsEnabled` | `boolean`, required | DefaultTolerationsEnabled records whether kube-apiserver runs the DefaultTolerationSeconds admission plugin. It and the two values below say what that plugin does, so a toleration the Pod did not ask for is recognized rather than refused. |
| `status.pendingObservation.admissionSnapshot.defaultUnreachableTolerationSeconds` | `integer`, required | DefaultUnreachableTolerationSeconds is that plugin's unreachable value. |
| `status.pendingObservation.admissionSnapshot.digest` | `string`, required | Digest covers this whole snapshot, so a Pod can be checked against it without re-reading the cluster objects it describes. |
| `status.pendingObservation.admissionSnapshot.extendedResourceTolerationEnabled` | `boolean`, required | ExtendedResourceTolerationEnabled records whether kube-apiserver runs the ExtendedResourceToleration admission plugin, which adds a toleration per extended resource a Pod requests. |
| `status.pendingObservation.admissionSnapshot.limitRanges` | `[]object` | LimitRanges are the namespace defaults that would be applied to the Pod. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].defaultLimits` | `object` | DefaultLimits are the limits it would add. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].defaultRequests` | `object` | DefaultRequests are the requests it would add to a container that asks for none. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object` | `object`, required | Object is the LimitRange this was read from. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.pendingObservation.admissionSnapshot.priorityClass` | `object`, required | PriorityClass is the scheduling priority it resolved to. |
| `status.pendingObservation.admissionSnapshot.priorityClass.name` | `string` | Name is that class, as the Pod requests it. |
| `status.pendingObservation.admissionSnapshot.priorityClass.object` | `object` | Object is the PriorityClass this was read from, where a class was named. |
| `status.pendingObservation.admissionSnapshot.priorityClass.object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.pendingObservation.admissionSnapshot.priorityClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.priorityClass.object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.pendingObservation.admissionSnapshot.priorityClass.preemptionPolicy` | `string`, one of `Never`, `PreemptLowerPriority` | PreemptionPolicy is what that class says about preempting others. |
| `status.pendingObservation.admissionSnapshot.priorityClass.value` | `integer`, required | Value is the priority it resolved to. |
| `status.pendingObservation.admissionSnapshot.runtimeClass` | `object` | RuntimeClass is the container runtime it resolved to, where one is named. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.handler` | `string`, required | Handler is the runtime handler it names. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.nodeSelector` | `object` | NodeSelector is the scheduling the class forces. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object` | `object`, required | Object is the RuntimeClass this was read from, by name and UID. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.overhead` | `object` | Overhead is the per-Pod resource overhead the class adds. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.overheadDefined` | `boolean` | OverheadDefined distinguishes an absent RuntimeClass overhead stanza from a present but empty one; Kubernetes admission preserves that distinction. OverheadDefined separates a class with no overhead from one whose overhead is zero. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations` | `[]object` | Tolerations are the tolerations it adds. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].effect` | `string` | Effect indicates the taint effect to match. Empty means match all taint effects. When specified, allowed values are NoSchedule, PreferNoSchedule and NoExecute. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].key` | `string` | Key is the taint key that the toleration applies to. Empty means match all taint keys. If the key is empty, operator must be Exists; this combination means to match all values and all keys. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].operator` | `string` | Operator represents a key's relationship to the value. Valid operators are Exists, Equal, Lt, and Gt. Defaults to Equal. Exists is equivalent to wildcard for value, so that a pod can tolerate all taints of a particular category. Lt and Gt perform numeric comparisons (requires feature gate TaintTolerationComparisonOperators). |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].tolerationSeconds` | `integer` | TolerationSeconds represents the period of time the toleration (which must be of effect NoExecute, otherwise this field is ignored) tolerates the taint. By default, it is not set, which means tolerate the taint forever (do not evict). Zero and negative values will be treated as 0 (evict immediately) by the system. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].value` | `string` | Value is the taint value the toleration matches to. If the operator is Exists, the value should be empty, otherwise just a regular string. |
| `status.pendingObservation.admissionSnapshot.serviceAccount` | `object`, required | ServiceAccount is the identity the Pod runs as, as it resolved. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.imagePullSecrets` | `[]object` | ImagePullSecrets are the pull Secrets it contributes to the Pod. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.imagePullSecrets[].name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object` | `object`, required | Object is the ServiceAccount the Pod runs as. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object.name` | `string`, required | Name of the cluster object this snapshot was read from. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object.uid` | `string`, required | UID it had, so a recreated object is a different one. |
| `status.pendingObservation.admissionSnapshot.templateDigest` | `string`, required | TemplateDigest binds the canonical, API-defaulted pre-admission Job Pod template. The self-referential snapshot annotation and four exact API-server-generated Job identity labels are omitted and validated separately against the current Job name and UID. TemplateDigest covers the Pod template the operator asked for, before the cluster's own admission had a chance to change it. |
| `status.pendingObservation.admissionSnapshot.version` | `string`, required, one of `v1` | Version is the snapshot format this record was written in. |
| `status.pendingObservation.applyGeneration` | `integer`, required | ApplyGeneration is the spec generation the apply was dispatched for, so a newer desired state does not inherit this proof. |
| `status.pendingObservation.applyJobName` | `string` | ApplyJobName and ApplyJobUID identify the Kubernetes Job independently of mutable labels so every exact-owner Pod can be tracked until the immutable execution horizon has elapsed. |
| `status.pendingObservation.applyJobUID` | `string` | ApplyJobUID is that Job's UID. |
| `status.pendingObservation.applyOperationID` | `string`, required | ApplyOperationID is the apply attempt this proof belongs to. |
| `status.pendingObservation.applyPodCount` | `integer` | ApplyPodCount is how many of them there were. |
| `status.pendingObservation.applyPodUIDs` | `[]string` | ApplyPodUIDs and ApplyPodCount preserve the terminal Pod evidence seen at the mutation boundary. More than one Pod always forces outcome-unknown proof even for a one-shot Job. |
| `status.pendingObservation.connectTimeout` | `string` | ConnectTimeout is the connect timeout the proof is dispatched with. |
| `status.pendingObservation.coordinationDigest` | `string`, required | CoordinationDigest is the realm the apply held, kept so the proof runs under the same lock rather than racing somebody else's turn. |
| `status.pendingObservation.dev` | `object` | Dev is the scratch database the proof may use. |
| `status.pendingObservation.dev.urlFrom` | `object`, required | URLFrom names the Secret key holding this database's connection URL. |
| `status.pendingObservation.dev.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.pendingObservation.dev.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.dev.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.pendingObservation.driftSeverity` | `string` | DriftSeverity is the severity the proof reads at. |
| `status.pendingObservation.exclude` | `[]string` | Exclude is the managed scope the apply was computed under. |
| `status.pendingObservation.leaseDurationSeconds` | `integer`, required | LeaseDurationSeconds is the immutable duration claimed for the Apply operation. The same holder remains active through convergence proof. |
| `status.pendingObservation.leaseEpoch` | `string` | LeaseEpoch identifies the uninterrupted database-realm lock acquisition carried from Apply through its complete read-only convergence proof. |
| `status.pendingObservation.lockTimeout` | `string` | LockTimeout is the database lock timeout it is dispatched with. |
| `status.pendingObservation.observeAfter` | `string` | ObserveAfter delays proof when the Kubernetes Job identity or create result is uncertain. Until this time, the original mutating Pod could still be within its immutable active deadline. |
| `status.pendingObservation.outcome` | `string`, required, one of `ApplySucceeded`, `OutcomeUnknown` | Outcome is what is known about the apply this proof is for: that it succeeded, or that nobody can say. |
| `status.pendingObservation.plan` | `object`, required | Plan is the plan the apply carried out, kept here after the active operation is cleared so the proof knows what it is proving. |
| `status.pendingObservation.plan.actualStateFingerprint` | `string`, required | ActualStateFingerprint is the observed state it was planned from. |
| `status.pendingObservation.plan.approval` | `object` | Approval is the decision that authorized this plan, where one was made. |
| `status.pendingObservation.plan.approval.approvedAt` | `string`, required | ApprovedAt is when the decision was stamped. |
| `status.pendingObservation.plan.approval.approver` | `object`, required | Approver is who the API server authenticated. |
| `status.pendingObservation.plan.approval.approver.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `status.pendingObservation.plan.approval.approver.uid` | `string` | UID of that user, where the authenticator provides one. |
| `status.pendingObservation.plan.approval.approver.username` | `string`, required | Username the API server authenticated the request as. |
| `status.pendingObservation.plan.approval.name` | `string`, required | Name of the approval that authorized the apply. |
| `status.pendingObservation.plan.approval.uid` | `string`, required | UID it had, so a recreated approval is not read as the same decision. |
| `status.pendingObservation.plan.artifactDigest` | `string`, required | ArtifactDigest is the artifact the plan was computed from. |
| `status.pendingObservation.plan.contentDigest` | `string`, required | ContentDigest is the digest of the plan bytes. |
| `status.pendingObservation.plan.controllerImage` | `string`, required | ControllerImage is the digest-pinned manager that published it. |
| `status.pendingObservation.plan.controllerRevision` | `string`, required | ControllerRevision is that manager's revision. |
| `status.pendingObservation.plan.controllerStateVersion` | `integer`, required | ControllerStateVersion is the state semantics it writes. |
| `status.pendingObservation.plan.coordinationDigest` | `string`, required | CoordinationDigest is the database realm it takes its turn in. |
| `status.pendingObservation.plan.createdAt` | `string`, required | CreatedAt is the plan object's own creation time, copied like every other field here, so an audit of this record and of the plan it names cannot disagree about when the plan came into being. |
| `status.pendingObservation.plan.desiredStateFingerprint` | `string`, required | DesiredStateFingerprint is the state the artifact declared. |
| `status.pendingObservation.plan.destructive` | `boolean`, required | Destructive says the plan drops or rewrites something. |
| `status.pendingObservation.plan.executionBindingID` | `string`, required | ExecutionBindingID is the execution epoch it belongs to. |
| `status.pendingObservation.plan.executorImage` | `string`, required | ExecutorImage is the digest-pinned image that ran it. |
| `status.pendingObservation.plan.fingerprint` | `string`, required | Fingerprint is the plan's complete approval identity, and the fields below are that identity spelled out. They are copied here so a reader -- and an audit -- can see what is waiting without fetching the plan. |
| `status.pendingObservation.plan.name` | `string`, required | Name of the PtahSchemaPlan this record is about. |
| `status.pendingObservation.plan.policyFingerprint` | `string`, required | PolicyFingerprint is the spec.policy it was computed under. |
| `status.pendingObservation.plan.ptahVersion` | `string`, required | PtahVersion is the Ptah build that computed the plan. |
| `status.pendingObservation.plan.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervised it. |
| `status.pendingObservation.plan.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. |
| `status.pendingObservation.plan.statementCount` | `integer`, required | StatementCount is how many statements it holds. The statements themselves are not here: read them with kubectl ptah plan. |
| `status.pendingObservation.plan.targetIdentityDigest` | `string`, required | TargetIdentityDigest is the database it was computed against. |
| `status.pendingObservation.plan.uid` | `string`, required | UID it had when this record was written. |
| `status.pendingObservation.plan.verificationPolicyDigest` | `string`, required | VerificationPolicyDigest is that policy's content at the time. |
| `status.pendingObservation.plan.verificationPolicyUID` | `string`, required | VerificationPolicyUID is the policy object that accepted the artifact. |
| `status.pendingObservation.planRequired` | `boolean` | PlanRequired records that the raw drift read completed and the same immutable proof inputs now require authoritative managed-scope planning. |
| `status.pendingObservation.protectedTables` | `[]string` | ProtectedTables is the fence the in-flight plan was computed under. A policy edit while a plan is pending must not let it execute against a fence it never saw. |
| `status.pendingObservation.source` | `object`, required | Source is the artifact access the proof needs to re-read the desired state. It holds Kubernetes selectors, never Secret contents. |
| `status.pendingObservation.source.digest` | `string`, required | Digest is that digest on its own. |
| `status.pendingObservation.source.registryAuthFrom` | `object` | RegistryAuthFrom names the Secret an operation Pod reads the registry credential from. It is a selector, never the credential. |
| `status.pendingObservation.source.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` | DockerConfigJSONKey is the Secret key holding a Docker config document. |
| `status.pendingObservation.source.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | Mode says how the credential reaches the executor: as environment variables, or as a Docker config file. |
| `status.pendingObservation.source.registryAuthFrom.name` | `string`, required | Name of the Secret the registry credential is read from. The manager never reads it; the operation Pod does. |
| `status.pendingObservation.source.registryAuthFrom.passwordKey` | `string`, default `password` | PasswordKey is the Secret key holding the password. |
| `status.pendingObservation.source.registryAuthFrom.tokenKey` | `string`, default `token` | TokenKey is the Secret key holding a bearer token, where one is used instead of a username and password. |
| `status.pendingObservation.source.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. UsernameKey is the Secret key holding the username. |
| `status.pendingObservation.source.resolvedReference` | `string`, required | ResolvedReference is the artifact with its tag replaced by a digest. |
| `status.pendingObservation.source.transport` | `object` | Transport is how the registry is reached: plain HTTP, a custom CA. |
| `status.pendingObservation.source.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `status.pendingObservation.source.transport.caFrom.key` | `string`, required | The key to select from the ConfigMap's Data field. Keys in the BinaryData field are not currently propagated to container env vars. |
| `status.pendingObservation.source.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.source.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `status.pendingObservation.source.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `status.pendingObservation.source.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` | CertificateKey is the Secret key holding the certificate. |
| `status.pendingObservation.source.transport.clientCertificateFrom.name` | `string`, required | Name of the Secret holding the client certificate. |
| `status.pendingObservation.source.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` | PrivateKeyKey is the Secret key holding its private key. |
| `status.pendingObservation.source.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `status.pendingObservation.target` | `object`, required | Target is the key-free binding the proof reads the database through. |
| `status.pendingObservation.target.engine` | `string`, required | Engine is the database this binding speaks. |
| `status.pendingObservation.target.urlFrom` | `object`, required | URLFrom is the Secret key the operation Pod reads the URL from. |
| `status.pendingObservation.target.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.pendingObservation.target.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.target.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.phase` | `string`, one of `Pending`, `Resolving`, `Verifying`, `Observing`, `Planning`, `ReadyToApply`, `AwaitingApproval`, `Blocked`, `Applying`, `VerifyingConvergence`, `InSync`, `Suspended`, `Failed` | Phase is where the resource stands, as one word for a reader. The conditions below are what a decision reads: a resource legitimately passes through several phases while one refusal stays true. |
| `status.plan` | `object` | Plan is the published plan waiting to run, where there is one. |
| `status.plan.actualStateFingerprint` | `string`, required | ActualStateFingerprint is the observed state it was planned from. |
| `status.plan.approval` | `object` | Approval is the decision that authorized this plan, where one was made. |
| `status.plan.approval.approvedAt` | `string`, required | ApprovedAt is when the decision was stamped. |
| `status.plan.approval.approver` | `object`, required | Approver is who the API server authenticated. |
| `status.plan.approval.approver.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `status.plan.approval.approver.uid` | `string` | UID of that user, where the authenticator provides one. |
| `status.plan.approval.approver.username` | `string`, required | Username the API server authenticated the request as. |
| `status.plan.approval.name` | `string`, required | Name of the approval that authorized the apply. |
| `status.plan.approval.uid` | `string`, required | UID it had, so a recreated approval is not read as the same decision. |
| `status.plan.artifactDigest` | `string`, required | ArtifactDigest is the artifact the plan was computed from. |
| `status.plan.contentDigest` | `string`, required | ContentDigest is the digest of the plan bytes. |
| `status.plan.controllerImage` | `string`, required | ControllerImage is the digest-pinned manager that published it. |
| `status.plan.controllerRevision` | `string`, required | ControllerRevision is that manager's revision. |
| `status.plan.controllerStateVersion` | `integer`, required | ControllerStateVersion is the state semantics it writes. |
| `status.plan.coordinationDigest` | `string`, required | CoordinationDigest is the database realm it takes its turn in. |
| `status.plan.createdAt` | `string`, required | CreatedAt is the plan object's own creation time, copied like every other field here, so an audit of this record and of the plan it names cannot disagree about when the plan came into being. |
| `status.plan.desiredStateFingerprint` | `string`, required | DesiredStateFingerprint is the state the artifact declared. |
| `status.plan.destructive` | `boolean`, required | Destructive says the plan drops or rewrites something. |
| `status.plan.executionBindingID` | `string`, required | ExecutionBindingID is the execution epoch it belongs to. |
| `status.plan.executorImage` | `string`, required | ExecutorImage is the digest-pinned image that ran it. |
| `status.plan.fingerprint` | `string`, required | Fingerprint is the plan's complete approval identity, and the fields below are that identity spelled out. They are copied here so a reader -- and an audit -- can see what is waiting without fetching the plan. |
| `status.plan.name` | `string`, required | Name of the PtahSchemaPlan this record is about. |
| `status.plan.policyFingerprint` | `string`, required | PolicyFingerprint is the spec.policy it was computed under. |
| `status.plan.ptahVersion` | `string`, required | PtahVersion is the Ptah build that computed the plan. |
| `status.plan.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervised it. |
| `status.plan.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. |
| `status.plan.statementCount` | `integer`, required | StatementCount is how many statements it holds. The statements themselves are not here: read them with kubectl ptah plan. |
| `status.plan.targetIdentityDigest` | `string`, required | TargetIdentityDigest is the database it was computed against. |
| `status.plan.uid` | `string`, required | UID it had when this record was written. |
| `status.plan.verificationPolicyDigest` | `string`, required | VerificationPolicyDigest is that policy's content at the time. |
| `status.plan.verificationPolicyUID` | `string`, required | VerificationPolicyUID is the policy object that accepted the artifact. |
| `status.source` | `object` | Source is what the desired artifact resolved and verified to. |
| `status.source.artifactType` | `string` | ArtifactType says which format this is: a declared schema or a versioned migration directory. |
| `status.source.digest` | `string` | Digest is that digest on its own. |
| `status.source.mediaType` | `string` | MediaType is the manifest's media type. |
| `status.source.requestedReference` | `string` | RequestedReference is what spec.desired asked for, tag and all. |
| `status.source.resolvedAt` | `string` | ResolvedAt is when the tag was last resolved. |
| `status.source.resolvedReference` | `string` | ResolvedReference is the same artifact with the tag replaced by the digest it resolved to. Every later read uses this. |
| `status.source.size` | `integer` | Size is the manifest's size in bytes. |
| `status.source.verificationPolicyDigest` | `string` | VerificationPolicyDigest is that policy's content at the time. |
| `status.source.verificationPolicyUID` | `string` | VerificationPolicyUID is the policy object that accepted it. |
| `status.source.verified` | `boolean` | Verified says the verification policy accepted it. It goes false again when the policy changes, because the old answer was about the old rules. |
| `status.source.verifiedAt` | `string` | VerifiedAt is when the policy last accepted it. |
| `status.target` | `object` | Target is what the last observation found in the database. |
| `status.target.coordinationDigest` | `string` | CoordinationDigest is the realm this target serializes against, so two resources pointed at one database take turns. |
| `status.target.driftFindingCount` | `integer` | DriftFindingCount is how many findings the complete report held, whether or not the list below was truncated. |
| `status.target.driftFindings` | `[]object` | DriftFindings contains only category-level aggregates. The total count above covers the complete report even when this list is truncated. |
| `status.target.driftFindings[].category` | `string`, required, one of `columns_added`, `columns_modified`, `columns_removed`, `constraints_added`, `constraints_removed`, `data_rows_deleted`, `data_rows_inserted`, `data_rows_updated`, `enum_values_added`, `enum_values_removed`, `enums_added`, `enums_removed`, `extensions_added`, `extensions_modified`, `extensions_removed`, `functions_added`, `functions_modified`, `functions_removed`, `indexes_added`, `indexes_removed`, `rls_enabled_tables_added`, `rls_enabled_tables_removed`, `rls_policies_added`, `rls_policies_modified`, `rls_policies_removed`, `roles_added`, `roles_modified`, `roles_removed`, `table_constraints_added`, `table_constraints_removed`, `tables_added`, `tables_removed`, `unique_protections_removed`, `vector_dimension_changed` | Category is the kind of difference, never the object it was found in: a table name is part of the schema, and the status does not carry it. |
| `status.target.driftFindings[].count` | `integer`, required | Count is how many differences of that kind the report held. |
| `status.target.driftFindings[].severity` | `string`, required, one of `safe`, `info`, `warning`, `error`, `destructive` | Severity is how the category rates. |
| `status.target.driftFindingsTruncated` | `boolean` | DriftFindingsTruncated says the list above is a prefix of the report. |
| `status.target.driftReportDigest` | `string` | DriftReportDigest is the observed state the last plan was computed from. |
| `status.target.engine` | `string` | Engine is the database engine this target speaks. |
| `status.target.highestDriftSeverity` | `string` | HighestDriftSeverity is the worst category the report found. |
| `status.target.identityDigest` | `string` | IdentityDigest identifies the database without carrying anything that could reach it. |
| `status.target.lastObservedAt` | `string` | LastObservedAt is when that observation ran. |

