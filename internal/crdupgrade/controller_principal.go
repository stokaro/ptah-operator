package crdupgrade

import (
	"fmt"
	"strconv"
	"strings"
)

func controllerPrincipalGuardDigest(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return hookIdentityDigest(releaseNamespace, releaseName, releaseSequence, managerImage)[:12]
}

func controllerPrincipalUsernames(releaseNamespace, candidate, previous string) []string {
	usernames := []string{"system:serviceaccount:" + releaseNamespace + ":" + candidate}
	if previous != "" && previous != candidate {
		usernames = append(usernames, "system:serviceaccount:"+releaseNamespace+":"+previous)
	}
	return usernames
}

func controllerPrincipalMatchExpression(releaseNamespace, candidate, previous string) string {
	usernames := controllerPrincipalUsernames(releaseNamespace, candidate, previous)
	quoted := make([]string, len(usernames))
	for index, username := range usernames {
		quoted[index] = strconv.Quote(username)
	}
	return `request.userInfo.username in [` + strings.Join(quoted, ", ") + `]`
}

func controllerPrincipalAuthorityExpression(
	releaseNamespace, candidate, previous string, candidateSequence, previousSequence int32,
) string {
	usernames := controllerPrincipalUsernames(releaseNamespace, candidate, previous)
	candidateAuthority := fmt.Sprintf(
		`request.userInfo.username == %q && variables.activeRelease == %d`,
		usernames[0],
		candidateSequence,
	)
	if len(usernames) == 1 {
		return candidateAuthority
	}
	return fmt.Sprintf(
		`(%s) || (request.userInfo.username == %q && variables.activeRelease == %d)`,
		candidateAuthority,
		usernames[1],
		previousSequence,
	)
}

func controllerPrincipalGuardDenialMessage() string {
	return "Ptah controller principal guard rejected an inactive release identity"
}
