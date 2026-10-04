{{/* Direct Secret references must reach the Pod guard even without operator
labels or owners. Keep the old object covered so identity removal cannot bypass
admission. The API tests exercise each reference form through this expression. */}}
{{- define "ptah-operator.resultCredentialReferences" -}}
{{- $pod := . -}}
({{ $pod }} != null && has({{ $pod }}.spec) && (
  (has({{ $pod }}.spec.imagePullSecrets) && {{ $pod }}.spec.imagePullSecrets.exists(s, s.name.startsWith('ptah-result-key-')))
  {{- range $containers := list "containers" "initContainers" "ephemeralContainers" }} ||
  (has({{ $pod }}.spec.{{ $containers }}) && {{ $pod }}.spec.{{ $containers }}.exists(c,
    (has(c.env) && c.env.exists(e, has(e.valueFrom) && has(e.valueFrom.secretKeyRef) && e.valueFrom.secretKeyRef.name.startsWith('ptah-result-key-'))) ||
    (has(c.envFrom) && c.envFrom.exists(e, has(e.secretRef) && e.secretRef.name.startsWith('ptah-result-key-')))))
  {{- end }} ||
  (has({{ $pod }}.spec.volumes) && {{ $pod }}.spec.volumes.exists(v,
    (has(v.secret) && v.secret.secretName.startsWith('ptah-result-key-')) ||
    (has(v.projected) && v.projected.sources.exists(s, has(s.secret) && s.secret.name.startsWith('ptah-result-key-'))) ||
    (has(v.csi) && has(v.csi.nodePublishSecretRef) && v.csi.nodePublishSecretRef.name.startsWith('ptah-result-key-')) ||
    (has(v.azureFile) && v.azureFile.secretName.startsWith('ptah-result-key-'))
    {{- range $source := list "cephfs" "cinder" "flexVolume" "iscsi" "rbd" "scaleIO" "storageos" }} ||
    (has(v.{{ $source }}) && has(v.{{ $source }}.secretRef) && v.{{ $source }}.secretRef.name.startsWith('ptah-result-key-'))
    {{- end }}
  ))
))
{{- end -}}
