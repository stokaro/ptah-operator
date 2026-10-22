package controllerwrite

import (
	"bytes"
	"context"
	"maps"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// validateSnapshotCreate admits the snapshot of a ConfigMap source and
// nothing else under a snapshot name. It rebuilds the snapshot from what the
// source ConfigMap holds now, the way plan publication is rebuilt from the
// schema's own status: the manager may store only the selected files of the
// ConfigMap its resource names, owned by that resource. A source edited
// between the manager's read and this one refuses the write, and the
// manager reads the source again on its next pass.
func (v *Validator) validateSnapshotCreate(ctx context.Context, req admissionv1.AdmissionRequest, configMap *corev1.ConfigMap) error {
	owner, _, err := sourcesnapshot.Verify(configMap)
	if err != nil {
		return denyf("ConfigMap is not a well-formed source snapshot: %v", err)
	}
	var spec *operatorv1alpha1.ConfigMapSourceSpec
	switch owner.Kind {
	case "PtahSchema":
		schema := &operatorv1alpha1.PtahSchema{}
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: owner.Name}, schema); err != nil {
			return internalf("directly read the snapshot's schema %s/%s: %v", req.Namespace, owner.Name, err)
		}
		if schema.UID != owner.UID || schema.DeletionTimestamp != nil {
			return denyf("source snapshot owner is not the live schema it names")
		}
		spec = schema.Spec.Desired.ConfigMap
	case "PtahMigration":
		migration := &operatorv1alpha1.PtahMigration{}
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: owner.Name}, migration); err != nil {
			return internalf("directly read the snapshot's migration %s/%s: %v", req.Namespace, owner.Name, err)
		}
		if migration.UID != owner.UID || migration.DeletionTimestamp != nil {
			return denyf("source snapshot owner is not the live migration it names")
		}
		spec = migration.Spec.Artifact.ConfigMap
	}
	if spec == nil {
		return denyf("source snapshot owner does not read a ConfigMap source")
	}
	source := &corev1.ConfigMap{}
	if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: spec.Name}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return denyf("source snapshot names a ConfigMap source that does not exist")
		}
		return internalf("directly read source ConfigMap %s/%s: %v", req.Namespace, spec.Name, err)
	}
	files, err := sourcesnapshot.Select(source, spec.Items)
	if err != nil {
		return denyf("source snapshot does not match its source: %v", err)
	}
	expected, _, err := sourcesnapshot.Build(req.Namespace, owner, files)
	if err != nil {
		return denyf("source snapshot does not match its source: %v", err)
	}
	if expected.Name != configMap.Name || !maps.EqualFunc(expected.BinaryData, configMap.BinaryData, bytes.Equal) {
		return denyf("source snapshot does not hold the selected files of its source")
	}
	return nil
}
