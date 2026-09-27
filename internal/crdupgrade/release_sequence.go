package crdupgrade

const (
	// ReleaseSequenceAnnotation must increase for every published operator
	// release, even when its stored-state and admission contracts stay stable.
	ReleaseSequenceAnnotation = "operator.ptah.run/release-sequence"
	// CurrentReleaseSequence is mirrored by the Helm helper and release gates.
	CurrentReleaseSequence int32 = 1
)
