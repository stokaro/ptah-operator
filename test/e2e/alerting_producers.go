package e2e

import (
	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	alSchemaProducer    = "e2e-alert-producer-schema"
	alMigrationProducer = "e2e-alert-producer-migration"
)

// An alert fixture must inherit inputs the real operator resolved and planned.
// Publishing an artifact alone does not establish the execution binding.
func alProducerReady(object client.Object, digest, executor string) bool {
	if !sha256Pattern.MatchString(digest) || !digestPinnedImage.MatchString(executor) || !alNegativeReading(object).gated() {
		return false
	}
	var binding *ptahv1.ExecutionBindingStatus
	var resolved string
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		binding, resolved = v.Status.ExecutionBinding, v.Status.Source.Digest
	case *ptahv1.PtahMigration:
		binding = v.Status.ExecutionBinding
		if v.Status.Artifact != nil {
			resolved = v.Status.Artifact.Digest
		}
	}
	return resolved == digest && binding != nil && binding.ExecutorImage == executor
}
