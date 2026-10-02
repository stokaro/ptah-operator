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
 (request.resource.resource != 'secrets' && request.resource.resource != 'ptahresultrecords' && request.userInfo.username ==
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
