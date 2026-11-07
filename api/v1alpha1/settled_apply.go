package v1alpha1

// SettledApplyAnnotation is how a person settles the proof a PtahSchema owes an
// Apply when that proof cannot be read: the Secret it reads holds a URL of
// another target than the plan the Apply ran from, which status reports with
// reason TargetIdentityChanged. Its value is the one that status prints, the
// Apply's operation ID and the digest of the target the read named:
//
//	kubectl annotate ptahschema storefront \
//	  operator.ptah.run/settled-apply=sha256:.../sha256:... --overwrite
//
// It says the person accounts for what that Apply did to the target it ran
// against. The operator gives up the proof at the next read that names that
// target, records no Apply and reads the database again under whatever the
// Secret holds. Under apply: Always the next plan then applies with no approval
// unless it is destructive or changes privileges. The value cannot be known
// before the resource stops, it settles no later Apply and no other target, and
// it does nothing while the proof is failing for any other reason.
const SettledApplyAnnotation = "operator.ptah.run/settled-apply"
