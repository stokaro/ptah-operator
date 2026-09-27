package crdupgrade

import (
	"fmt"
)

func controllerPrincipalGuardDigest(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return hookIdentityDigest(releaseNamespace, releaseName, releaseSequence, managerImage)[:12]
}

func controllerPrincipalUsername(releaseNamespace, serviceAccount string) string {
	return "system:serviceaccount:" + releaseNamespace + ":" + serviceAccount
}

func controllerPrincipalMatchExpression(releaseNamespace, serviceAccount string) string {
	return fmt.Sprintf(`request.userInfo.username == %q`, controllerPrincipalUsername(releaseNamespace, serviceAccount))
}

// controllerPrincipalAuthorityExpression admits the controller while this
// release or the one before it is active. Every release runs the controller
// under the same ServiceAccount, so the predecessor's runtime writes under
// that name until the cutover stops it, and this release's runtime writes
// under it once the cutover activates this release.
func controllerPrincipalAuthorityExpression(releaseNamespace, serviceAccount string, releaseSequence int32) string {
	username := controllerPrincipalUsername(releaseNamespace, serviceAccount)
	if releaseSequence <= 1 {
		return fmt.Sprintf(`request.userInfo.username == %q && variables.activeRelease == %d`, username, releaseSequence)
	}
	return fmt.Sprintf(
		`request.userInfo.username == %q && (variables.activeRelease == %d || variables.activeRelease == %d)`,
		username,
		releaseSequence,
		releaseSequence-1,
	)
}

func controllerPrincipalGuardDenialMessage() string {
	return "Ptah controller principal guard rejected an inactive release identity"
}
