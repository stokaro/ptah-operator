package crdupgrade

import "fmt"

// The runtime verifier holds the rendered Helm helper to this expression.
// The webhook API tests prove its routing and the handler's refusals together.
func resultCredentialReferences(pod string) string {
	expression := fmt.Sprintf(`(%[1]s != null && has(%[1]s.spec) && (
 (has(%[1]s.spec.imagePullSecrets) && %[1]s.spec.imagePullSecrets.exists(s, s.name.startsWith('ptah-result-key-')))`, pod)
	for _, containers := range []string{"containers", "initContainers", "ephemeralContainers"} {
		expression += fmt.Sprintf(` ||
 (has(%[1]s.spec.%[2]s) && %[1]s.spec.%[2]s.exists(c,
 (has(c.env) && c.env.exists(e, has(e.valueFrom) && has(e.valueFrom.secretKeyRef) && e.valueFrom.secretKeyRef.name.startsWith('ptah-result-key-'))) ||
 (has(c.envFrom) && c.envFrom.exists(e, has(e.secretRef) && e.secretRef.name.startsWith('ptah-result-key-')))))`, pod, containers)
	}
	expression += fmt.Sprintf(` ||
 (has(%[1]s.spec.volumes) && %[1]s.spec.volumes.exists(v,
 (has(v.secret) && v.secret.secretName.startsWith('ptah-result-key-')) ||
 (has(v.projected) && v.projected.sources.exists(s, has(s.secret) && s.secret.name.startsWith('ptah-result-key-'))) ||
 (has(v.csi) && has(v.csi.nodePublishSecretRef) && v.csi.nodePublishSecretRef.name.startsWith('ptah-result-key-')) ||
 (has(v.azureFile) && v.azureFile.secretName.startsWith('ptah-result-key-'))`, pod)
	for _, source := range []string{"cephfs", "cinder", "flexVolume", "iscsi", "rbd", "scaleIO", "storageos"} {
		expression += fmt.Sprintf(` || (has(v.%[1]s) && has(v.%[1]s.secretRef) && v.%[1]s.secretRef.name.startsWith('ptah-result-key-'))`, source)
	}
	return expression + `))))`
}

var podIntentMatchExpression = podIntentIdentityMatchExpression + " || " + resultCredentialReferences("object") + " || " + resultCredentialReferences("oldObject")

// PodIntentMatchExpression is the installed routing contract, shared with
// lifecycle acceptance so a valid credential reference cannot escape its check.
func PodIntentMatchExpression() string { return podIntentMatchExpression }

func controllerWriteMatchExpression(expected RuntimeInvariants) string {
	return ControllerWriteMatchExpression("system:serviceaccount:" + expected.ReleaseNamespace + ":" + expected.ControllerServiceAccountName)
}

// ControllerWriteMatchExpression includes reserved credentials for every writer.
func ControllerWriteMatchExpression(controllerUser string) string {
	return fmt.Sprintf(`(request.resource.resource == 'ptahresultrecords') ||
 (request.userInfo.username ==
 '%s') ||
 (request.resource.resource == 'secrets' && (
 (object != null && object.metadata.name.startsWith('ptah-result-key-')) ||
 (oldObject != null && oldObject.metadata.name.startsWith('ptah-result-key-'))))`, controllerUser)
}

// MatchExpressionsEqual ignores formatting outside CEL string literals while
// preserving the exact identities and name prefixes those literals contain.
func MatchExpressionsEqual(actual, expected string) bool {
	return normalizeExpression(actual) == normalizeExpression(expected)
}

// The credential projection is the only additional volume durable delivery
// introduces. Its exact operation-derived name and frozen Pod template are
// checked by the controller-write webhook; this policy holds the isolated
// Secret projection and mount boundary even before that lookup.
const controllerJobResultCredentialExpression = `dyn(object).spec.template.spec.volumes.filter(v, v.name == "result-credentials").size() <= 1 &&
dyn(object).spec.template.spec.volumes.all(v, v.name != "result-credentials" || (
  has(v.secret) && v.secret.secretName.matches("^ptah-result-key-[0-9a-f]{32}$") &&
  has(v.secret.defaultMode) && v.secret.defaultMode == 288 &&
  (!has(v.secret.optional) || !v.secret.optional) &&
  has(v.secret.items) && v.secret.items.size() == 3 &&
  v.secret.items.map(item, item.key) == ["tls.crt", "tls.key", "ca.crt"] &&
  v.secret.items.all(item, item.path == item.key && !has(item.mode)) &&
  dyn(object).spec.template.spec.containers.all(c,
    has(c.volumeMounts) && c.volumeMounts.filter(m, m.name == "result-credentials").size() == 1 &&
    c.volumeMounts.all(m, m.name != "result-credentials" || (
      m.mountPath == "/credentials/result" && has(m.readOnly) && m.readOnly &&
      (!has(m.subPath) || m.subPath == "") && (!has(m.subPathExpr) || m.subPathExpr == "") &&
      (!has(m.mountPropagation) || m.mountPropagation == "None")))) &&
  dyn(object).spec.template.spec.initContainers.all(c,
    !has(c.volumeMounts) || c.volumeMounts.all(m, m.name != "result-credentials"))
))`
