---
title: PtahSchema
description: Every field of the PtahSchema resource, generated from the API types.
---

`PtahSchema` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.desired` | `object`, required | Desired is the OCI artifact that declares the schema, and the rows a declaration names, to converge it to. |
| `spec.desired.ociRef` | `string`, required | OCIRef is a desired-schema artifact reference. It may name a tag or a digest; every later operation receives only the resolved digest. |
| `spec.desired.registryAuthFrom` | `object` | RegistryAuthSource describes a Secret without requiring the controller to read it. The kubelet projects only the selected credential representation into a Job, while every mode also projects the fixed registry authority grant to the runner. |
| `spec.desired.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` |  |
| `spec.desired.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | RegistryAuthMode selects one standard Kubernetes Secret representation. |
| `spec.desired.registryAuthFrom.name` | `string`, required |  |
| `spec.desired.registryAuthFrom.passwordKey` | `string`, default `password` |  |
| `spec.desired.registryAuthFrom.registryKey` | `string`, one of `registry`, default `registry` | RegistryKey is retained for source compatibility. The key is fixed so the Secret owner, rather than a PtahSchema author, controls the authority grant. The referenced Secret must contain an authority-only host[:port] value. |
| `spec.desired.registryAuthFrom.tokenKey` | `string`, default `token` |  |
| `spec.desired.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. |
| `spec.desired.transport` | `object` | OCITransportSpec configures private and air-gapped registries without allowing arbitrary files or commands into the execution Pod. |
| `spec.desired.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `spec.desired.transport.caFrom.key` | `string`, required | The key to select. |
| `spec.desired.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.desired.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `spec.desired.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `spec.desired.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` |  |
| `spec.desired.transport.clientCertificateFrom.name` | `string`, required |  |
| `spec.desired.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` |  |
| `spec.desired.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `spec.desired.verificationPolicyFrom` | `object`, required | Selects a key from a ConfigMap. |
| `spec.desired.verificationPolicyFrom.key` | `string`, required | The key to select. |
| `spec.desired.verificationPolicyFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.desired.verificationPolicyFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `spec.dev` | `object` | Dev is a scratch database Ptah may use where a comparison needs one. It is never the target, and nothing it holds is kept. |
| `spec.dev.urlFrom` | `object`, required | SecretKeySelector selects a key of a Secret. |
| `spec.dev.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `spec.dev.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.dev.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `spec.execution` | `object`, default `{}` | Default the object itself so the API server also applies the nested execution defaults when a manifest omits the whole block. |
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
| `spec.interval` | `string`, default `10m` | Interval is the cadence for resolving mutable tags and observing drift. |
| `spec.policy` | `object`, default `{}` | Policy decides what may happen without a person: whether a plan applies itself, whether a destructive one is permitted at all, what counts as drift, and which tables are fenced off entirely. |
| `spec.policy.allowDestructive` | `boolean`, default `false` | AllowDestructive permits a plan that drops or rewrites something. It is permission for the category, not for a plan: a destructive plan still needs an approval where the apply policy asks for one. |
| `spec.policy.apply` | `string`, one of `Never`, `OnApproval`, `Always`, default `OnApproval` | Apply decides when a current plan may run: never, only with an approval naming its exact bytes, or as soon as it is ready. |
| `spec.policy.driftSeverity` | `string`, one of `all`, `destructive`, default `all` | DriftSeverity decides which differences count as drift worth applying: every difference, or only the destructive ones. |
| `spec.policy.exclude` | `[]string` | Exclude defines the single authoritative managed scope. Raw drift is observed without exclusions, then a read-only plan classifies this exact scope as changed or converged. |
| `spec.policy.lockTimeout` | `string`, default `30s` | LockTimeout is how long an operation waits for the database's own lock before giving up, so a busy database delays a run rather than stalling it for the Job's whole deadline. |
| `spec.policy.protectedTables` | `[]string` | ProtectedTables fences declared row sets off from the declarative path. A plan that would change a listed table is refused rather than rated, and there is no override: an approval, allowDestructive and a permissive severity are all answers to "how risky is this", and a fence is the statement that no such answer exists for these rows. Where the change is wanted, the entry goes, or the rows are written as a migration. An entry names a table, or a schema and a table, the way the declaration does. Matching is Ptah's, which is case-insensitive, and an entry on a table the artifact already agrees with refuses nothing -- which is what lets a fence sit in a policy permanently. |
| `spec.policy.transactionMode` | `string`, one of `all`, `file`, `none`, default `file` | TransactionMode is how the statements are wrapped: all in one transaction, one per file, or none at all. An engine that refuses a mode decides over this rather than around it. |
| `spec.suspend` | `boolean`, default `false` | Suspend prevents new Jobs. A Job already applying is observed to a terminal result and is never replaced by a destructive cleanup action. |
| `spec.target` | `object`, required | Target is the database to converge, named through a Secret the manager itself has no permission to read. |
| `spec.target.coordinationKey` | `string`, required | CoordinationKey is a non-secret, stable identifier for the physical database realm. Every schema that can reach the same database through an alias, proxy, or different credential must use exactly the same key. |
| `spec.target.engine` | `string`, required | DatabaseEngine names a database family. The API accepts bounded engine names so the controller can report unsupported families through status instead of turning a durable desired-state object into an admission-time dead end. |
| `spec.target.sharedRealm` | `boolean`, default `false` | SharedRealm declares that this resource manages only part of the database its coordination key names, and that every other resource managing that database has declared the same. It defaults to false, and a realm that more than one resource claims is refused while any claimant leaves it false. Serialization is not ownership: two resources that never run at the same time still undo each other's work by taking turns, so the operator blocks them rather than letting them alternate. A resource that runs nothing claims nothing. Deleting one leaves the realm, and so does suspending it: suspension is how a resource steps aside without being deleted. Resuming it puts it back in the census, and the conflict is refused then, before any Job. The declaration is what is verified, not the disjointness. No analyzer can tell whether two sets of arbitrary SQL touch the same rows, and a field that claimed otherwise would be the wrong kind of assurance. What it buys is that sharing is deliberate on every side: one resource that has not declared it blocks all of them, itself included. |
| `spec.target.urlFrom` | `object`, required | SecretKeySelector selects a key of a Secret. |
| `spec.target.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `spec.target.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `spec.target.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.activeOperation` | `object` | ActiveOperation is the claim for the operation in flight. It is written before the Job exists, which is what lets the controller tell a Job it created from one it has not created yet. |
| `status.activeOperation.admissionSnapshot` | `object` | AdmissionSnapshot is persisted before dispatch and is bound into the Job and Pod template annotations. It permits only modeled, safe built-in admission mutations while retaining exact validation for executable and security-sensitive Pod fields. |
| `status.activeOperation.admissionSnapshot.alwaysPullImagesEnabled` | `boolean`, required | AlwaysPullImagesEnabled records whether kube-apiserver runs the AlwaysPullImages admission plugin. |
| `status.activeOperation.admissionSnapshot.defaultNotReadyTolerationSeconds` | `integer`, required |  |
| `status.activeOperation.admissionSnapshot.defaultTolerationsEnabled` | `boolean`, required | DefaultTolerationsEnabled records whether kube-apiserver runs the DefaultTolerationSeconds admission plugin. |
| `status.activeOperation.admissionSnapshot.defaultUnreachableTolerationSeconds` | `integer`, required |  |
| `status.activeOperation.admissionSnapshot.digest` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.extendedResourceTolerationEnabled` | `boolean`, required | ExtendedResourceTolerationEnabled records whether kube-apiserver runs the ExtendedResourceToleration admission plugin. |
| `status.activeOperation.admissionSnapshot.limitRanges` | `[]object` |  |
| `status.activeOperation.admissionSnapshot.limitRanges[].defaultLimits` | `object` |  |
| `status.activeOperation.admissionSnapshot.limitRanges[].defaultRequests` | `object` |  |
| `status.activeOperation.admissionSnapshot.limitRanges[].object` | `object`, required | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.activeOperation.admissionSnapshot.limitRanges[].object.name` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.limitRanges[].object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.limitRanges[].object.uid` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.priorityClass` | `object`, required | PriorityClassAdmissionSnapshot records the exact values injected by the Priority admission plugin. Object is absent only when the cluster has no global default and the Job does not request a named PriorityClass. |
| `status.activeOperation.admissionSnapshot.priorityClass.name` | `string` |  |
| `status.activeOperation.admissionSnapshot.priorityClass.object` | `object` | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.activeOperation.admissionSnapshot.priorityClass.object.name` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.priorityClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.priorityClass.object.uid` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.priorityClass.preemptionPolicy` | `string`, one of `Never`, `PreemptLowerPriority` | PreemptionPolicy describes a policy for if/when to preempt a pod. |
| `status.activeOperation.admissionSnapshot.priorityClass.value` | `integer`, required |  |
| `status.activeOperation.admissionSnapshot.runtimeClass` | `object` | RuntimeClassAdmissionSnapshot records the exact scheduling and overhead mutation selected before dispatch. Handler is deliberately retained as credential-free audit evidence even though it is not copied into PodSpec. |
| `status.activeOperation.admissionSnapshot.runtimeClass.handler` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.runtimeClass.nodeSelector` | `object` |  |
| `status.activeOperation.admissionSnapshot.runtimeClass.object` | `object`, required | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.activeOperation.admissionSnapshot.runtimeClass.object.name` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.runtimeClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.runtimeClass.object.uid` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.runtimeClass.overhead` | `object` |  |
| `status.activeOperation.admissionSnapshot.runtimeClass.overheadDefined` | `boolean` | OverheadDefined distinguishes an absent RuntimeClass overhead stanza from a present but empty one; Kubernetes admission preserves that distinction. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations` | `[]object` |  |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].effect` | `string` | Effect indicates the taint effect to match. Empty means match all taint effects. When specified, allowed values are NoSchedule, PreferNoSchedule and NoExecute. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].key` | `string` | Key is the taint key that the toleration applies to. Empty means match all taint keys. If the key is empty, operator must be Exists; this combination means to match all values and all keys. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].operator` | `string` | Operator represents a key's relationship to the value. Valid operators are Exists, Equal, Lt, and Gt. Defaults to Equal. Exists is equivalent to wildcard for value, so that a pod can tolerate all taints of a particular category. Lt and Gt perform numeric comparisons (requires feature gate TaintTolerationComparisonOperators). |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].tolerationSeconds` | `integer` | TolerationSeconds represents the period of time the toleration (which must be of effect NoExecute, otherwise this field is ignored) tolerates the taint. By default, it is not set, which means tolerate the taint forever (do not evict). Zero and negative values will be treated as 0 (evict immediately) by the system. |
| `status.activeOperation.admissionSnapshot.runtimeClass.tolerations[].value` | `string` | Value is the taint value the toleration matches to. If the operator is Exists, the value should be empty, otherwise just a regular string. |
| `status.activeOperation.admissionSnapshot.serviceAccount` | `object`, required | ServiceAccountAdmissionSnapshot binds the non-secret ServiceAccount fields that built-in admission may copy into a Pod. |
| `status.activeOperation.admissionSnapshot.serviceAccount.imagePullSecrets` | `[]object` |  |
| `status.activeOperation.admissionSnapshot.serviceAccount.imagePullSecrets[].name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.admissionSnapshot.serviceAccount.object` | `object`, required | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.activeOperation.admissionSnapshot.serviceAccount.object.name` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.serviceAccount.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.activeOperation.admissionSnapshot.serviceAccount.object.uid` | `string`, required |  |
| `status.activeOperation.admissionSnapshot.templateDigest` | `string`, required | TemplateDigest binds the canonical, API-defaulted pre-admission Job Pod template. The self-referential snapshot annotation and four exact API-server-generated Job identity labels are omitted and validated separately against the current Job name and UID. |
| `status.activeOperation.admissionSnapshot.version` | `string`, required, one of `v1` |  |
| `status.activeOperation.attempt` | `integer`, required |  |
| `status.activeOperation.coordinationDigest` | `string` | Plan and Apply operations persist their credential-free lock binding so later spec changes cannot redirect or shorten protection for a running Job. |
| `status.activeOperation.dispatchNotAfter` | `string` | DispatchNotAfter is the immutable last instant at which the Apply runner may start its mutating child. Missing or untrusted terminal Pod evidence keeps proof behind the complete execution horizon below. |
| `status.activeOperation.dispatchStarted` | `boolean` | DispatchStarted is persisted immediately before the one permitted Job create attempt. A missing Apply Job after this boundary is outcome-unknown and must never be recreated. |
| `status.activeOperation.executionBindingID` | `string` | ExecutionBindingID binds every Job and result to the durable evidence epoch that authorized its claim. |
| `status.activeOperation.executionNotAfter` | `string` | ExecutionNotAfter is enforced by the runner as the mutating child context deadline, independently of relative Job and Pod deadlines. |
| `status.activeOperation.id` | `string`, required |  |
| `status.activeOperation.inputFingerprint` | `string`, required |  |
| `status.activeOperation.jobName` | `string`, required |  |
| `status.activeOperation.jobUID` | `string` | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `status.activeOperation.leaseContinuityLost` | `boolean` | LeaseContinuityLost is set before any result can be harvested when the persisted epoch no longer owns an uninterrupted lock interval. |
| `status.activeOperation.leaseDurationSeconds` | `integer` |  |
| `status.activeOperation.leaseEpoch` | `string` | LeaseEpoch is persisted before dispatch. A different epoch invalidates every result produced by this operation. |
| `status.activeOperation.observationConnectTimeout` | `string` |  |
| `status.activeOperation.observationDev` | `object` | DatabaseTargetRef is a database URL reference used for optional rehearsal. |
| `status.activeOperation.observationDev.urlFrom` | `object`, required | SecretKeySelector selects a key of a Secret. |
| `status.activeOperation.observationDev.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.activeOperation.observationDev.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.observationDev.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.activeOperation.observationExclude` | `[]string` |  |
| `status.activeOperation.observationLockTimeout` | `string` |  |
| `status.activeOperation.observationProtectedTables` | `[]string` |  |
| `status.activeOperation.observationSeverity` | `string` |  |
| `status.activeOperation.source` | `object` | Source snapshots artifact access for mandatory post-Apply observation. It contains Kubernetes selectors only, never Secret contents. |
| `status.activeOperation.source.digest` | `string`, required |  |
| `status.activeOperation.source.registryAuthFrom` | `object` | RegistryAuthSource describes a Secret without requiring the controller to read it. The kubelet projects only the selected credential representation into a Job, while every mode also projects the fixed registry authority grant to the runner. |
| `status.activeOperation.source.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` |  |
| `status.activeOperation.source.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | RegistryAuthMode selects one standard Kubernetes Secret representation. |
| `status.activeOperation.source.registryAuthFrom.name` | `string`, required |  |
| `status.activeOperation.source.registryAuthFrom.passwordKey` | `string`, default `password` |  |
| `status.activeOperation.source.registryAuthFrom.registryKey` | `string`, one of `registry`, default `registry` | RegistryKey is retained for source compatibility. The key is fixed so the Secret owner, rather than a PtahSchema author, controls the authority grant. The referenced Secret must contain an authority-only host[:port] value. |
| `status.activeOperation.source.registryAuthFrom.tokenKey` | `string`, default `token` |  |
| `status.activeOperation.source.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. |
| `status.activeOperation.source.resolvedReference` | `string`, required |  |
| `status.activeOperation.source.transport` | `object` | OCITransportSpec configures private and air-gapped registries without allowing arbitrary files or commands into the execution Pod. |
| `status.activeOperation.source.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `status.activeOperation.source.transport.caFrom.key` | `string`, required | The key to select. |
| `status.activeOperation.source.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.source.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `status.activeOperation.source.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `status.activeOperation.source.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` |  |
| `status.activeOperation.source.transport.clientCertificateFrom.name` | `string`, required |  |
| `status.activeOperation.source.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` |  |
| `status.activeOperation.source.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `status.activeOperation.startedAt` | `string`, required |  |
| `status.activeOperation.target` | `object` | DatabaseTargetBinding is the key-free immutable target snapshot stored in status. CoordinationKey must never be copied into status; its digest is persisted separately. |
| `status.activeOperation.target.engine` | `string`, required | DatabaseEngine names a database family. The API accepts bounded engine names so the controller can report unsupported families through status instead of turning a durable desired-state object into an admission-time dead end. |
| `status.activeOperation.target.urlFrom` | `object`, required | SecretKeySelector selects a key of a Secret. |
| `status.activeOperation.target.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.activeOperation.target.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.activeOperation.target.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.activeOperation.targetIdentityDigest` | `string` |  |
| `status.activeOperation.terminationGracePeriodSeconds` | `integer` |  |
| `status.activeOperation.type` | `string`, required, one of `Resolve`, `Verify`, `Observe`, `Plan`, `Apply` | OperationType identifies one serialized execution Job. |
| `status.activeOperation.verificationPolicyDigest` | `string` |  |
| `status.activeOperation.verificationPolicyUID` | `string` | VerificationPolicyUID and VerificationPolicyDigest bind a Verify Job to the immutable ConfigMap version inspected before dispatch. |
| `status.applied` | `object` | Applied is the last apply that was independently observed to have converged, which is a different claim from a Job that exited zero. |
| `status.applied.artifactDigest` | `string`, required |  |
| `status.applied.completedAt` | `string`, required |  |
| `status.applied.controllerImage` | `string` |  |
| `status.applied.controllerRevision` | `string` |  |
| `status.applied.controllerStateVersion` | `integer` |  |
| `status.applied.coordinationDigest` | `string`, required |  |
| `status.applied.executionBindingID` | `string` |  |
| `status.applied.executorImage` | `string`, required |  |
| `status.applied.planFingerprint` | `string`, required |  |
| `status.applied.planRef` | `object` | PlanRef names the stored plan this apply ran. It is what a reader addresses to see the SQL that was applied, instead of searching the namespace for a fingerprint. Optional, because a record written before the field existed carries only PlanFingerprint. It does not replace that fingerprint: the reference says which object to read and the fingerprint says whether the object read is the one this record was written for. |
| `status.applied.planRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `status.applied.planRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `status.applied.ptahVersion` | `string`, required |  |
| `status.applied.runnerImage` | `string`, required |  |
| `status.applied.runnerProtocolVersion` | `integer`, required |  |
| `status.applied.targetIdentityDigest` | `string`, required |  |
| `status.conditions` | `[]object` | Conditions are the readable verdicts: whether the engine is supported, the artifact resolved and verified, the database was reachable, drift was found, a plan is ready, an approval is required, the schema is in sync, and whether the last reconciliation failed. |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.executionBinding` | `object` | ExecutionBinding is the durable identity of the controller/runtime epoch authorized to produce new reconciliation evidence. Retained evidence stays historical until refreshed. Epoch changes on every component transition, including a rollback to identical values. |
| `status.executionBinding.controllerImage` | `string` | ControllerImage identifies the exact manager container content that interpreted controller state and authorized this evidence epoch. |
| `status.executionBinding.controllerRevision` | `string` | ControllerRevision identifies the exact manager build that interpreted controller state. It is provenance metadata in addition to ControllerImage, not a substitute for the image content digest. |
| `status.executionBinding.controllerStateVersion` | `integer` | ControllerStateVersion versions manager-side reconciliation semantics independently of the data-plane runner protocol. |
| `status.executionBinding.epoch` | `string`, required |  |
| `status.executionBinding.executorImage` | `string`, required |  |
| `status.executionBinding.ptahVersion` | `string`, required |  |
| `status.executionBinding.runnerImage` | `string`, required |  |
| `status.executionBinding.runnerProtocolVersion` | `integer`, required |  |
| `status.lastAttemptTime` | `string` | LastAttemptTime is when the controller last tried to do something. |
| `status.lastSuccessfulReconciliation` | `string` | LastSuccessfulReconciliation is when it last completed a cycle with nothing left to do. |
| `status.nextReconciliationTime` | `string` | NextReconciliationTime is the durable earliest time for the next scheduled read-only reconciliation. Event-driven safety work may run sooner. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the spec generation this status describes. |
| `status.pendingLockRelease` | `object` | PendingLockRelease keeps the exact Lease owner and epoch durable until an idempotent release succeeds. It closes the manager-crash window between a terminal status transition and clearing the owner-neutral Lease. |
| `status.pendingLockRelease.coordinationDigest` | `string`, required |  |
| `status.pendingLockRelease.leaseDurationSeconds` | `integer`, required |  |
| `status.pendingLockRelease.leaseEpoch` | `string`, required |  |
| `status.pendingLockRelease.operationID` | `string`, required |  |
| `status.pendingObservation` | `object` | PendingObservation is durable proof work created after an Apply Job may have mutated the database. It is independent of Phase so retries and suspension cannot accidentally permit another mutation first. |
| `status.pendingObservation.admissionSnapshot` | `object` | AdmissionSnapshot retains the exact pre-admission Pod template identity after ActiveOperation is cleared. Current-format Apply Job cleanup after an execution-binding change fails closed when this evidence is absent; older supported Job envelopes use their separate compatibility contract. |
| `status.pendingObservation.admissionSnapshot.alwaysPullImagesEnabled` | `boolean`, required | AlwaysPullImagesEnabled records whether kube-apiserver runs the AlwaysPullImages admission plugin. |
| `status.pendingObservation.admissionSnapshot.defaultNotReadyTolerationSeconds` | `integer`, required |  |
| `status.pendingObservation.admissionSnapshot.defaultTolerationsEnabled` | `boolean`, required | DefaultTolerationsEnabled records whether kube-apiserver runs the DefaultTolerationSeconds admission plugin. |
| `status.pendingObservation.admissionSnapshot.defaultUnreachableTolerationSeconds` | `integer`, required |  |
| `status.pendingObservation.admissionSnapshot.digest` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.extendedResourceTolerationEnabled` | `boolean`, required | ExtendedResourceTolerationEnabled records whether kube-apiserver runs the ExtendedResourceToleration admission plugin. |
| `status.pendingObservation.admissionSnapshot.limitRanges` | `[]object` |  |
| `status.pendingObservation.admissionSnapshot.limitRanges[].defaultLimits` | `object` |  |
| `status.pendingObservation.admissionSnapshot.limitRanges[].defaultRequests` | `object` |  |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object` | `object`, required | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object.name` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.limitRanges[].object.uid` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.priorityClass` | `object`, required | PriorityClassAdmissionSnapshot records the exact values injected by the Priority admission plugin. Object is absent only when the cluster has no global default and the Job does not request a named PriorityClass. |
| `status.pendingObservation.admissionSnapshot.priorityClass.name` | `string` |  |
| `status.pendingObservation.admissionSnapshot.priorityClass.object` | `object` | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.pendingObservation.admissionSnapshot.priorityClass.object.name` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.priorityClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.priorityClass.object.uid` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.priorityClass.preemptionPolicy` | `string`, one of `Never`, `PreemptLowerPriority` | PreemptionPolicy describes a policy for if/when to preempt a pod. |
| `status.pendingObservation.admissionSnapshot.priorityClass.value` | `integer`, required |  |
| `status.pendingObservation.admissionSnapshot.runtimeClass` | `object` | RuntimeClassAdmissionSnapshot records the exact scheduling and overhead mutation selected before dispatch. Handler is deliberately retained as credential-free audit evidence even though it is not copied into PodSpec. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.handler` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.runtimeClass.nodeSelector` | `object` |  |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object` | `object`, required | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object.name` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.object.uid` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.runtimeClass.overhead` | `object` |  |
| `status.pendingObservation.admissionSnapshot.runtimeClass.overheadDefined` | `boolean` | OverheadDefined distinguishes an absent RuntimeClass overhead stanza from a present but empty one; Kubernetes admission preserves that distinction. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations` | `[]object` |  |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].effect` | `string` | Effect indicates the taint effect to match. Empty means match all taint effects. When specified, allowed values are NoSchedule, PreferNoSchedule and NoExecute. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].key` | `string` | Key is the taint key that the toleration applies to. Empty means match all taint keys. If the key is empty, operator must be Exists; this combination means to match all values and all keys. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].operator` | `string` | Operator represents a key's relationship to the value. Valid operators are Exists, Equal, Lt, and Gt. Defaults to Equal. Exists is equivalent to wildcard for value, so that a pod can tolerate all taints of a particular category. Lt and Gt perform numeric comparisons (requires feature gate TaintTolerationComparisonOperators). |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].tolerationSeconds` | `integer` | TolerationSeconds represents the period of time the toleration (which must be of effect NoExecute, otherwise this field is ignored) tolerates the taint. By default, it is not set, which means tolerate the taint forever (do not evict). Zero and negative values will be treated as 0 (evict immediately) by the system. |
| `status.pendingObservation.admissionSnapshot.runtimeClass.tolerations[].value` | `string` | Value is the taint value the toleration matches to. If the operator is Exists, the value should be empty, otherwise just a regular string. |
| `status.pendingObservation.admissionSnapshot.serviceAccount` | `object`, required | ServiceAccountAdmissionSnapshot binds the non-secret ServiceAccount fields that built-in admission may copy into a Pod. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.imagePullSecrets` | `[]object` |  |
| `status.pendingObservation.admissionSnapshot.serviceAccount.imagePullSecrets[].name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object` | `object`, required | AdmissionObjectBinding identifies one API object whose credential-free contents contributed to the resolved Pod admission envelope. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object.name` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object.resourceVersion` | `string`, required | ResourceVersion is opaque, but bounded here so hostile metadata cannot make the status object grow without limit. |
| `status.pendingObservation.admissionSnapshot.serviceAccount.object.uid` | `string`, required |  |
| `status.pendingObservation.admissionSnapshot.templateDigest` | `string`, required | TemplateDigest binds the canonical, API-defaulted pre-admission Job Pod template. The self-referential snapshot annotation and four exact API-server-generated Job identity labels are omitted and validated separately against the current Job name and UID. |
| `status.pendingObservation.admissionSnapshot.version` | `string`, required, one of `v1` |  |
| `status.pendingObservation.applyGeneration` | `integer`, required |  |
| `status.pendingObservation.applyJobName` | `string` | ApplyJobName and ApplyJobUID identify the Kubernetes Job independently of mutable labels so every exact-owner Pod can be tracked until the immutable execution horizon has elapsed. |
| `status.pendingObservation.applyJobUID` | `string` | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `status.pendingObservation.applyOperationID` | `string`, required |  |
| `status.pendingObservation.applyPodCount` | `integer` |  |
| `status.pendingObservation.applyPodUIDs` | `[]string` | ApplyPodUIDs and ApplyPodCount preserve the terminal Pod evidence seen at the mutation boundary. More than one Pod always forces outcome-unknown proof even for a one-shot Job. |
| `status.pendingObservation.connectTimeout` | `string` |  |
| `status.pendingObservation.coordinationDigest` | `string`, required |  |
| `status.pendingObservation.dev` | `object` | DatabaseTargetRef is a database URL reference used for optional rehearsal. |
| `status.pendingObservation.dev.urlFrom` | `object`, required | SecretKeySelector selects a key of a Secret. |
| `status.pendingObservation.dev.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.pendingObservation.dev.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.dev.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.pendingObservation.driftSeverity` | `string` |  |
| `status.pendingObservation.exclude` | `[]string` |  |
| `status.pendingObservation.leaseDurationSeconds` | `integer`, required | LeaseDurationSeconds is the immutable duration claimed for the Apply operation. The same holder remains active through convergence proof. |
| `status.pendingObservation.leaseEpoch` | `string` | LeaseEpoch identifies the uninterrupted database-realm lock acquisition carried from Apply through its complete read-only convergence proof. |
| `status.pendingObservation.lockTimeout` | `string` |  |
| `status.pendingObservation.observeAfter` | `string` | ObserveAfter delays proof when the Kubernetes Job identity or create result is uncertain. Until this time, the original mutating Pod could still be within its immutable active deadline. |
| `status.pendingObservation.outcome` | `string`, required, one of `ApplySucceeded`, `OutcomeUnknown` | PendingObservationOutcome records why read-only convergence proof is mandatory before another mutation may be considered. |
| `status.pendingObservation.plan` | `object`, required | CurrentPlanStatus is a compact reference to an immutable PtahSchemaPlan. |
| `status.pendingObservation.plan.actualStateFingerprint` | `string`, required | ActualStateFingerprint is the observed state it was planned from. |
| `status.pendingObservation.plan.approval` | `object` | Approval is the decision that authorized this plan, where one was made. |
| `status.pendingObservation.plan.approval.approvedAt` | `string`, required |  |
| `status.pendingObservation.plan.approval.approver` | `object`, required | ApprovalIdentity is stamped from the authenticated admission request. The API client does not choose these fields. |
| `status.pendingObservation.plan.approval.approver.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `status.pendingObservation.plan.approval.approver.uid` | `string` | UID of that user, where the authenticator provides one. |
| `status.pendingObservation.plan.approval.approver.username` | `string`, required | Username the API server authenticated the request as. |
| `status.pendingObservation.plan.approval.name` | `string`, required |  |
| `status.pendingObservation.plan.approval.uid` | `string`, required | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `status.pendingObservation.plan.artifactDigest` | `string`, required | ArtifactDigest is the artifact the plan was computed from. |
| `status.pendingObservation.plan.contentDigest` | `string`, required | ContentDigest is the digest of the plan bytes. |
| `status.pendingObservation.plan.controllerImage` | `string` | ControllerImage is the digest-pinned manager that published it. |
| `status.pendingObservation.plan.controllerRevision` | `string` | ControllerRevision is that manager's revision. |
| `status.pendingObservation.plan.controllerStateVersion` | `integer` | ControllerStateVersion is the state semantics it writes. |
| `status.pendingObservation.plan.coordinationDigest` | `string`, required | CoordinationDigest is the database realm it takes its turn in. |
| `status.pendingObservation.plan.createdAt` | `string`, required | CreatedAt is the plan object's own creation time, copied like every other field here, so an audit of this record and of the plan it names cannot disagree about when the plan came into being. |
| `status.pendingObservation.plan.desiredStateFingerprint` | `string`, required | DesiredStateFingerprint is the state the artifact declared. |
| `status.pendingObservation.plan.destructive` | `boolean`, required | Destructive says the plan drops or rewrites something. |
| `status.pendingObservation.plan.executionBindingID` | `string` | ExecutionBindingID is the execution epoch it belongs to. |
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
| `status.pendingObservation.source` | `object`, required | OCIArtifactAccessBinding is a credential-free, immutable snapshot of the exact artifact and Kubernetes credential selectors needed to fetch it. It is persisted across post-Apply proof so a newer generation cannot send newly selected credentials to the old artifact's registry. |
| `status.pendingObservation.source.digest` | `string`, required |  |
| `status.pendingObservation.source.registryAuthFrom` | `object` | RegistryAuthSource describes a Secret without requiring the controller to read it. The kubelet projects only the selected credential representation into a Job, while every mode also projects the fixed registry authority grant to the runner. |
| `status.pendingObservation.source.registryAuthFrom.dockerConfigJSONKey` | `string`, default `.dockerconfigjson` |  |
| `status.pendingObservation.source.registryAuthFrom.mode` | `string`, one of `Environment`, `DockerConfigJSON`, default `Environment` | RegistryAuthMode selects one standard Kubernetes Secret representation. |
| `status.pendingObservation.source.registryAuthFrom.name` | `string`, required |  |
| `status.pendingObservation.source.registryAuthFrom.passwordKey` | `string`, default `password` |  |
| `status.pendingObservation.source.registryAuthFrom.registryKey` | `string`, one of `registry`, default `registry` | RegistryKey is retained for source compatibility. The key is fixed so the Secret owner, rather than a PtahSchema author, controls the authority grant. The referenced Secret must contain an authority-only host[:port] value. |
| `status.pendingObservation.source.registryAuthFrom.tokenKey` | `string`, default `token` |  |
| `status.pendingObservation.source.registryAuthFrom.usernameKey` | `string`, default `username` | Environment mode supports username/password or an identity token. Keys are optional so a single Secret shape can use either credential form. |
| `status.pendingObservation.source.resolvedReference` | `string`, required |  |
| `status.pendingObservation.source.transport` | `object` | OCITransportSpec configures private and air-gapped registries without allowing arbitrary files or commands into the execution Pod. |
| `status.pendingObservation.source.transport.caFrom` | `object` | CAFrom selects a custom CA bundle. When registryAuthFrom is present, that same Secret must contain caSHA256 with the exact lowercase SHA-256 digest of the selected bytes. |
| `status.pendingObservation.source.transport.caFrom.key` | `string`, required | The key to select. |
| `status.pendingObservation.source.transport.caFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.source.transport.caFrom.optional` | `boolean` | Specify whether the ConfigMap or its key must be defined |
| `status.pendingObservation.source.transport.clientCertificateFrom` | `object` | ClientCertificateFrom is reserved for a future executor contract that can select a client certificate by the effective TLS authority on every request, including redirects. The current API rejects this field. |
| `status.pendingObservation.source.transport.clientCertificateFrom.certificateKey` | `string`, default `tls.crt` |  |
| `status.pendingObservation.source.transport.clientCertificateFrom.name` | `string`, required |  |
| `status.pendingObservation.source.transport.clientCertificateFrom.privateKeyKey` | `string`, default `tls.key` |  |
| `status.pendingObservation.source.transport.plainHTTP` | `boolean`, default `false` | PlainHTTP is intended only for explicitly trusted test or air-gapped networks. HTTPS remains the default. When registryAuthFrom is present, its Secret must also contain allowPlainHTTP with the exact value "true". |
| `status.pendingObservation.target` | `object`, required | DatabaseTargetBinding is the key-free immutable target snapshot stored in status. CoordinationKey must never be copied into status; its digest is persisted separately. |
| `status.pendingObservation.target.engine` | `string`, required | DatabaseEngine names a database family. The API accepts bounded engine names so the controller can report unsupported families through status instead of turning a durable desired-state object into an admission-time dead end. |
| `status.pendingObservation.target.urlFrom` | `object`, required | SecretKeySelector selects a key of a Secret. |
| `status.pendingObservation.target.urlFrom.key` | `string`, required | The key of the secret to select from. Must be a valid secret key. |
| `status.pendingObservation.target.urlFrom.name` | `string`, default `` | Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty. Instances of this type with an empty value here are almost certainly wrong. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names |
| `status.pendingObservation.target.urlFrom.optional` | `boolean` | Specify whether the Secret or its key must be defined |
| `status.phase` | `string`, one of `Pending`, `Resolving`, `Verifying`, `Observing`, `Planning`, `ReadyToApply`, `AwaitingApproval`, `Blocked`, `Applying`, `VerifyingConvergence`, `InSync`, `Suspended`, `Failed` | Phase is where the resource stands, as one word for a reader. The conditions below are what a decision reads: a resource legitimately passes through several phases while one refusal stays true. |
| `status.plan` | `object` | Plan is the published plan waiting to run, where there is one. |
| `status.plan.actualStateFingerprint` | `string`, required | ActualStateFingerprint is the observed state it was planned from. |
| `status.plan.approval` | `object` | Approval is the decision that authorized this plan, where one was made. |
| `status.plan.approval.approvedAt` | `string`, required |  |
| `status.plan.approval.approver` | `object`, required | ApprovalIdentity is stamped from the authenticated admission request. The API client does not choose these fields. |
| `status.plan.approval.approver.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `status.plan.approval.approver.uid` | `string` | UID of that user, where the authenticator provides one. |
| `status.plan.approval.approver.username` | `string`, required | Username the API server authenticated the request as. |
| `status.plan.approval.name` | `string`, required |  |
| `status.plan.approval.uid` | `string`, required | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `status.plan.artifactDigest` | `string`, required | ArtifactDigest is the artifact the plan was computed from. |
| `status.plan.contentDigest` | `string`, required | ContentDigest is the digest of the plan bytes. |
| `status.plan.controllerImage` | `string` | ControllerImage is the digest-pinned manager that published it. |
| `status.plan.controllerRevision` | `string` | ControllerRevision is that manager's revision. |
| `status.plan.controllerStateVersion` | `integer` | ControllerStateVersion is the state semantics it writes. |
| `status.plan.coordinationDigest` | `string`, required | CoordinationDigest is the database realm it takes its turn in. |
| `status.plan.createdAt` | `string`, required | CreatedAt is the plan object's own creation time, copied like every other field here, so an audit of this record and of the plan it names cannot disagree about when the plan came into being. |
| `status.plan.desiredStateFingerprint` | `string`, required | DesiredStateFingerprint is the state the artifact declared. |
| `status.plan.destructive` | `boolean`, required | Destructive says the plan drops or rewrites something. |
| `status.plan.executionBindingID` | `string` | ExecutionBindingID is the execution epoch it belongs to. |
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
| `status.source.artifactType` | `string` |  |
| `status.source.digest` | `string` |  |
| `status.source.mediaType` | `string` |  |
| `status.source.requestedReference` | `string` |  |
| `status.source.resolvedAt` | `string` |  |
| `status.source.resolvedReference` | `string` |  |
| `status.source.size` | `integer` |  |
| `status.source.verificationPolicyDigest` | `string` |  |
| `status.source.verificationPolicyUID` | `string` | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `status.source.verified` | `boolean` |  |
| `status.source.verifiedAt` | `string` |  |
| `status.target` | `object` | Target is what the last observation found in the database. |
| `status.target.coordinationDigest` | `string` |  |
| `status.target.driftFindingCount` | `integer` |  |
| `status.target.driftFindings` | `[]object` | DriftFindings contains only category-level aggregates. The total count above covers the complete report even when this list is truncated. |
| `status.target.driftFindings[].category` | `string`, required, one of `columns_added`, `columns_modified`, `columns_removed`, `constraints_added`, `constraints_removed`, `data_rows_deleted`, `data_rows_inserted`, `data_rows_updated`, `enum_values_added`, `enum_values_removed`, `enums_added`, `enums_removed`, `extensions_added`, `extensions_modified`, `extensions_removed`, `functions_added`, `functions_modified`, `functions_removed`, `indexes_added`, `indexes_removed`, `rls_enabled_tables_added`, `rls_enabled_tables_removed`, `rls_policies_added`, `rls_policies_modified`, `rls_policies_removed`, `roles_added`, `roles_modified`, `roles_removed`, `table_constraints_added`, `table_constraints_removed`, `tables_added`, `tables_removed`, `unique_protections_removed`, `vector_dimension_changed` |  |
| `status.target.driftFindings[].count` | `integer`, required |  |
| `status.target.driftFindings[].severity` | `string`, required, one of `safe`, `info`, `warning`, `error`, `destructive` |  |
| `status.target.driftFindingsTruncated` | `boolean` |  |
| `status.target.driftReportDigest` | `string` |  |
| `status.target.engine` | `string` | DatabaseEngine names a database family. The API accepts bounded engine names so the controller can report unsupported families through status instead of turning a durable desired-state object into an admission-time dead end. |
| `status.target.highestDriftSeverity` | `string` |  |
| `status.target.identityDigest` | `string` |  |
| `status.target.lastObservedAt` | `string` |  |

