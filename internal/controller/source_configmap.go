package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcepolicy"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// errSourceRefused marks a ConfigMap source that, as configured, gives no
// snapshot: a missing ConfigMap or key, a path the rules refuse, or a
// selection too large for one ConfigMap. Reading it again changes nothing
// until somebody edits the source or the resource, and either edit wakes the
// resource.
var errSourceRefused = errors.New("ConfigMap source refused")

// configMapSource is what reading a ConfigMap source gave: the ConfigMap it
// read, and the snapshot that now holds the selected files.
type configMapSource struct {
	revision operatorv1alpha1.ConfigMapSourceRevision
	snapshot operatorv1alpha1.SourceSnapshotBinding
}

// readConfigMapSource reads a ConfigMap source and makes sure its snapshot
// exists. It reads the source from the API server, never from the cache,
// whose ConfigMaps carry no data. A snapshot with the same content digest is
// found rather than written again.
//
// schemaFile is set for a schema, which reads the one file it selected.
func readConfigMapSource(
	ctx context.Context,
	reader client.Reader,
	writer client.Writer,
	namespace string,
	owner sourcesnapshot.Owner,
	spec operatorv1alpha1.ConfigMapSourceSpec,
	schemaFile bool,
	policy *sourcepolicy.Policy,
) (configMapSource, error) {
	if policy != nil {
		if err := policy.CheckConfigMap(spec.Name); err != nil {
			return configMapSource{}, fmt.Errorf("%w: %w", errSourceRefused, err)
		}
	}
	source := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: spec.Name}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return configMapSource{}, fmt.Errorf("%w: ConfigMap %q does not exist", errSourceRefused, spec.Name)
		}
		return configMapSource{}, fmt.Errorf("read ConfigMap %q: %w", spec.Name, err)
	}
	if schemaFile && len(spec.Items) != 1 {
		return configMapSource{}, fmt.Errorf("%w: a schema selects exactly one file", errSourceRefused)
	}
	files, err := sourcesnapshot.Select(source, spec.Items)
	if err != nil {
		return configMapSource{}, fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	if policy != nil {
		size := int64(0)
		for _, file := range files {
			size += int64(len(file.Data))
		}
		// Before the snapshot is written: a refused source leaves nothing.
		if err := policy.CheckContentBytes(size); err != nil {
			return configMapSource{}, fmt.Errorf("%w: %w", errSourceRefused, err)
		}
	}
	built, digest, err := sourcesnapshot.Build(namespace, owner, files)
	if err != nil {
		return configMapSource{}, fmt.Errorf("%w: %w", errSourceRefused, err)
	}
	stored, err := ensureSnapshot(ctx, reader, writer, built)
	if err != nil {
		return configMapSource{}, err
	}
	snapshot := operatorv1alpha1.SourceSnapshotBinding{Name: stored.Name, UID: stored.UID, ContentDigest: digest}
	if schemaFile {
		snapshot.SchemaFile = spec.Items[0].Path
	}
	return configMapSource{
		revision: operatorv1alpha1.ConfigMapSourceRevision{
			Name: source.Name, UID: source.UID, ResourceVersion: source.ResourceVersion,
		},
		snapshot: snapshot,
	}, nil
}

// ensureSnapshot returns the stored snapshot built describes, creating it when
// it does not exist. A ConfigMap under that name that is this resource's but
// does not hold these files is replaced: only a person can have made it so,
// and nothing reads it while the resource is resolving. One that belongs to
// something else is left alone and refused.
func ensureSnapshot(ctx context.Context, reader client.Reader, writer client.Writer, built *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	for attempt := 0; attempt < 2; attempt++ {
		stored := &corev1.ConfigMap{}
		err := reader.Get(ctx, client.ObjectKeyFromObject(built), stored)
		if apierrors.IsNotFound(err) {
			created := built.DeepCopy()
			if err := writer.Create(ctx, created); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return nil, fmt.Errorf("create source snapshot %q: %w", built.Name, err)
			}
			return created, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read source snapshot %q: %w", built.Name, err)
		}
		if sourcesnapshot.Same(stored, built) {
			return stored, nil
		}
		if !ownsSnapshot(stored, built.OwnerReferences[0].UID) {
			return nil, fmt.Errorf("%w: ConfigMap %q exists and is not this resource's source snapshot", errSourceRefused, built.Name)
		}
		if err := deleteSnapshot(ctx, writer, stored); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("source snapshot %q changed while it was written", built.Name)
}

// retireSnapshots deletes this resource's snapshots that nothing names any
// more. keep is every snapshot something still reads: the one the source
// resolved to, and any an operation, a plan or a post-Apply proof names.
func retireSnapshots(
	ctx context.Context,
	reader client.Reader,
	writer client.Writer,
	namespace string,
	owner sourcesnapshot.Owner,
	ownerLabel string,
	keep ...string,
) error {
	configMaps := &corev1.ConfigMapList{}
	if err := reader.List(ctx, configMaps, client.InNamespace(namespace), client.MatchingLabels{ownerLabel: owner.Name}); err != nil {
		return fmt.Errorf("list source snapshots: %w", err)
	}
	for index := range configMaps.Items {
		configMap := &configMaps.Items[index]
		if !sourcesnapshot.IsName(configMap.Name) || !ownsSnapshot(configMap, owner.UID) || slices.Contains(keep, configMap.Name) {
			continue
		}
		if err := deleteSnapshot(ctx, writer, configMap); err != nil {
			return err
		}
	}
	return nil
}

// ownsSnapshot reports whether the resource with uid controls configMap.
func ownsSnapshot(configMap *corev1.ConfigMap, uid types.UID) bool {
	for _, reference := range configMap.OwnerReferences {
		if reference.Controller != nil && *reference.Controller && reference.UID == uid &&
			reference.APIVersion == operatorv1alpha1.GroupVersion.String() {
			return true
		}
	}
	return false
}

// deleteSnapshot deletes exactly the object that was read: a snapshot written
// again under the same name in between is another object, and stays.
func deleteSnapshot(ctx context.Context, writer client.Writer, configMap *corev1.ConfigMap) error {
	uid := configMap.UID
	err := writer.Delete(ctx, configMap, client.Preconditions{UID: &uid})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete source snapshot %q: %w", configMap.Name, err)
	}
	return nil
}

// snapshotNames returns the names of the snapshots the bindings name, for
// retireSnapshots.
func snapshotNames(bindings ...*operatorv1alpha1.SourceAccessBinding) []string {
	var names []string
	for _, binding := range bindings {
		if binding != nil && binding.Snapshot != nil {
			names = append(names, binding.Snapshot.Name)
		}
	}
	return names
}

// sourceConfigMapNames is every ConfigMap a source reads whose change has to
// wake the resource: the verification policy of an OCI source, the ConfigMap
// of a ConfigMap source.
func sourceConfigMapNames(spec operatorv1alpha1.SourceSpec) []string {
	var names []string
	if spec.VerificationPolicyFrom != nil && spec.VerificationPolicyFrom.Name != "" {
		names = append(names, spec.VerificationPolicyFrom.Name)
	}
	if spec.ConfigMap != nil && spec.ConfigMap.Name != "" {
		names = append(names, spec.ConfigMap.Name)
	}
	if spec.SourcePolicyFrom != nil && spec.SourcePolicyFrom.Name != "" {
		names = append(names, spec.SourcePolicyFrom.Name)
	}
	return names
}
