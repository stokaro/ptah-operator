package main

import (
	"strconv"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

// values are what the chart decides when it renders a release, and so what
// the generator leaves as Helm expressions in every template.
type values struct {
	ReleaseNamespace         string
	ReleaseName              string
	ControllerServiceAccount string
	ManagerImage             string
	ControllerStateVersion   int32
	RotatorServiceAccount    string
	WebhookSecret            string
}

// sentinels are the values the generator builds the policies with. Each is
// a token no real value contains, so the emitter finds it wherever a
// definition put it. The controller-state version is an integer, so its
// token is a number no release will reach.
func sentinels() values {
	return values{
		ReleaseNamespace:         "__PTAH_RELEASE_NAMESPACE__",
		ReleaseName:              "__PTAH_RELEASE_NAME__",
		ControllerServiceAccount: "__PTAH_CONTROLLER_SERVICE_ACCOUNT__",
		ManagerImage:             "__PTAH_MANAGER_IMAGE__",
		ControllerStateVersion:   1999999999,
		RotatorServiceAccount:    "__PTAH_CERT_ROTATOR_SERVICE_ACCOUNT__",
		WebhookSecret:            "__PTAH_WEBHOOK_SECRET__",
	}
}

// templates are the chart templates the generator writes, built from v.
//
// The generator calls this with sentinels; the test that renders the chart
// calls it with the release's real values and compares the policies helm
// produced with the ones Go built, which is what proves the Helm expressions
// below compute what the Go definitions do.
//
// templates/apply-policy-guard.yaml is not here. It has no Go definition to
// generate from: it is written once, in the chart, where its exempt-groups
// loop and its refusal of system:authenticated are Helm logic over a value.
func templates(v values) []template {
	object := &crdupgrade.ControllerObjectGuard{
		ReleaseName:                  v.ReleaseName,
		ReleaseNamespace:             v.ReleaseNamespace,
		ControllerServiceAccountName: v.ControllerServiceAccount,
		ManagerImage:                 v.ManagerImage,
		ControllerStateVersion:       v.ControllerStateVersion,
	}
	write := &crdupgrade.ControllerWriteGuard{
		ReleaseName:                  v.ReleaseName,
		ReleaseNamespace:             v.ReleaseNamespace,
		ControllerServiceAccountName: v.ControllerServiceAccount,
	}
	state := &crdupgrade.ManagerStateGuard{
		ReleaseName:                  v.ReleaseName,
		ReleaseNamespace:             v.ReleaseNamespace,
		ControllerServiceAccountName: v.ControllerServiceAccount,
	}
	secret := certrotation.SecretCreateGuard{
		Namespace:          v.ReleaseNamespace,
		ReleaseName:        v.ReleaseName,
		SecretName:         v.WebhookSecret,
		ServiceAccountName: v.RotatorServiceAccount,
	}
	const controllerServiceAccount = `$controllerServiceAccount := include "ptah-operator.serviceAccountName" .`

	var objectPairs []policyPair
	for _, guard := range object.Policies() {
		objectPairs = append(objectPairs, policyPair{policy: guard.Policy, binding: guard.Binding})
	}
	var statePairs []policyPair
	for _, guard := range state.Policies() {
		statePairs = append(statePairs, policyPair{policy: guard.Policy, binding: guard.Binding})
	}
	return []template{
		{
			path:   "templates/controller-object-guard.yaml",
			source: "internal/crdupgrade/controller_object_guard.go",
			preamble: []string{
				controllerServiceAccount,
				`$releaseControllerState := include "ptah-operator.controllerStateVersion" .`,
				`$releaseControllerImage := include "ptah-operator.controllerImage" .`,
			},
			parameters: []parameter{
				{crdupgrade.ControllerJobWriteGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.controllerJobWriteGuardPolicyName" .`},
				{crdupgrade.ControllerChunkWriteGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.controllerChunkWriteGuardPolicyName" .`},
				{crdupgrade.ControllerProjectionWriteGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.controllerProjectionWriteGuardPolicyName" .`},
				{crdupgrade.ControllerPlanWriteGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.controllerPlanWriteGuardPolicyName" .`},
				{crdupgrade.ControllerMigrationPlanWriteGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.controllerMigrationPlanWriteGuardPolicyName" .`},
				{v.ReleaseNamespace, ".Release.Namespace"},
				{v.ControllerServiceAccount, "$controllerServiceAccount"},
				{v.ManagerImage, "$releaseControllerImage"},
				{strconv.FormatInt(int64(v.ControllerStateVersion), 10), "$releaseControllerState"},
			},
			pairs: objectPairs,
		},
		{
			path:     "templates/controller-write-guard.yaml",
			source:   "internal/crdupgrade/controller_write_guard.go",
			preamble: []string{controllerServiceAccount},
			parameters: []parameter{
				{crdupgrade.ControllerWriteGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.controllerWriteGuardPolicyName" .`},
				{v.ReleaseNamespace, ".Release.Namespace"},
				{v.ControllerServiceAccount, "$controllerServiceAccount"},
			},
			pairs: []policyPair{{policy: write.Policy(), binding: write.Binding()}},
		},
		{
			path:     "templates/manager-state-guard.yaml",
			source:   "internal/crdupgrade/manager_state_guard.go",
			preamble: []string{controllerServiceAccount},
			parameters: []parameter{
				{crdupgrade.StatusWriteGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.statusWriteGuardPolicyName" .`},
				{crdupgrade.UnresolvedRunGuardPolicyName(v.ReleaseNamespace, v.ReleaseName), `include "ptah-operator.unresolvedRunGuardPolicyName" .`},
				{v.ReleaseNamespace, ".Release.Namespace"},
				{v.ControllerServiceAccount, "$controllerServiceAccount"},
			},
			pairs: statePairs,
		},
		{
			path:   "templates/certificate-secret-guard.yaml",
			source: "internal/certrotation/secret_create_guard.go",
			// The guard exists for the create grant the chart adds when a
			// deleted generated Secret may be recreated; without that grant
			// there is nothing to narrow, and with an external Secret there
			// is no rotator.
			condition: "and .Values.certificateRotation.enabled (not .Values.webhook.existingSecret) .Values.certificateRotation.recreateMissingSecret",
			preamble: []string{
				`$rotatorName := include "ptah-operator.certRotatorServiceAccountName" .`,
				`$secretName := include "ptah-operator.webhookSecretName" .`,
			},
			parameters: []parameter{
				{v.RotatorServiceAccount, "$rotatorName"},
				{v.WebhookSecret, "$secretName"},
				{v.ReleaseNamespace, ".Release.Namespace"},
				{v.ReleaseName, ".Release.Name"},
			},
			pairs: []policyPair{{policy: secret.Policy(), binding: secret.Binding()}},
		},
	}
}
