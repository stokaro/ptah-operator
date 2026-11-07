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
// against. The operator gives up the proof at a read that names that target,
// once the resource has stopped there and printed that value, records no Apply
// and reads the database again under whatever the Secret holds. Under apply:
// Always the next plan then applies with no approval unless it is destructive
// or changes privileges. The value is a statement rather than a secret, and it
// holds until the proof ends, so a store that leaves the target and comes back
// settles there with no new write. Removing the annotation takes it back. It
// settles no other Apply and no other target, and it does nothing while the
// proof is failing for any other reason.
const SettledApplyAnnotation = "operator.ptah.run/settled-apply"
