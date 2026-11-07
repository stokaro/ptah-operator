package v1alpha1

// SettledApplyAnnotation is how a person settles the proof a PtahSchema owes an
// Apply when that proof cannot be read: the URL Secret names another login or
// address than the plan the Apply ran from, which status reports with reason
// TargetIdentityChanged. Its value is status.pendingObservation.applyOperationID:
//
//	kubectl annotate ptahschema storefront \
//	  operator.ptah.run/settled-apply=sha256:... --overwrite
//
// It says the person accounts for what that Apply did to the target it ran
// against. The operator gives up the proof, records no Apply and reads the
// database again under whatever the Secret names now. An operation ID is never
// reused, so the annotation cannot settle a later Apply, and it does nothing
// while the proof is failing for any other reason.
const SettledApplyAnnotation = "operator.ptah.run/settled-apply"
