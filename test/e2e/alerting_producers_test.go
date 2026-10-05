package e2e

import (
	"strings"
	"testing"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestAlProducerRequiresNativePlanAndExactInputs(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	executor := "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			object := negativeFixtureState(t, family, ptahv1.ApplyPolicyOnApproval)
			if alProducerReady(object, digest, executor) {
				t.Fatal("a plan without resolved inputs counted as a native producer")
			}
			binding := &ptahv1.ExecutionBindingStatus{ExecutorImage: executor}
			switch v := object.(type) {
			case *ptahv1.PtahSchema:
				v.Status.Source.Digest, v.Status.ExecutionBinding = digest, binding
			case *ptahv1.PtahMigration:
				v.Status.Artifact = &ptahv1.OCIArtifactAccessBinding{Digest: digest}
				v.Status.ExecutionBinding = binding
			}
			if !alProducerReady(object, digest, executor) {
				t.Fatal("the exact artifact and native executor at the approval gate were refused")
			}
			if alProducerReady(object, "sha256:"+strings.Repeat("c", 64), executor) {
				t.Fatal("another published artifact was accepted")
			}
			if alProducerReady(object, digest, "registry.example/ptah:latest") {
				t.Fatal("an unpinned executor was accepted")
			}
			binding.ExecutorImage = "registry.example/ptah@sha256:" + strings.Repeat("c", 64)
			if alProducerReady(object, digest, executor) {
				t.Fatal("a different native executor was accepted")
			}
			binding.ExecutorImage = executor
			switch v := object.(type) {
			case *ptahv1.PtahSchema:
				v.Status.Plan = nil
			case *ptahv1.PtahMigration:
				v.Status.Plan = nil
			}
			if alProducerReady(object, digest, executor) {
				t.Fatal("resolved inputs without a native plan were accepted")
			}
		})
	}
}
