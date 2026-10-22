// Package sourcesnapshot is the immutable ConfigMap a ConfigMap source is
// copied into. The manager writes one per resource and content digest, owned
// by the resource; every operation that reads the source mounts it, and the
// operation Pod checks it against the bound content digest before it starts.
//
// The layout is the one internal/sourcecontent reads: the manifest under
// sourcecontent.ManifestKey and file i under sourcecontent.SnapshotFileKey(i),
// all of it in binaryData so that no byte is reinterpreted on the way.
package sourcesnapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

const (
	// NamePrefix starts every snapshot name.
	NamePrefix = "ptah-src-"

	// LabelSchema and LabelMigration name the owning resource, the same
	// labels the resource's Jobs and plans carry.
	LabelSchema    = "operator.ptah.run/schema"
	LabelMigration = "operator.ptah.run/migration"
)

var namePattern = regexp.MustCompile(`^ptah-src-[0-9a-f]{24}$`)

// ErrMissingKey is the refusal of a source that selects a key the ConfigMap
// does not hold.
var ErrMissingKey = errors.New("source ConfigMap does not hold a selected key")

// ErrTooLarge is the refusal of a selection that does not fit in one
// ConfigMap.
var ErrTooLarge = errors.New("selected files do not fit in a snapshot")

// Owner is the resource a snapshot belongs to.
type Owner struct {
	// Kind is PtahSchema or PtahMigration.
	Kind string
	Name string
	UID  types.UID
}

func (o Owner) label() (string, error) {
	switch o.Kind {
	case "PtahSchema":
		return LabelSchema, nil
	case "PtahMigration":
		return LabelMigration, nil
	default:
		return "", fmt.Errorf("a snapshot cannot belong to a %s", o.Kind)
	}
}

// Name is the snapshot one resource keeps for one content digest. Two
// resources with the same files keep two snapshots, so neither one's
// deletion takes the other's away.
func Name(owner types.UID, contentDigest string) string {
	sum := sha256.Sum256([]byte(string(owner) + "\x00" + contentDigest))
	return NamePrefix + hex.EncodeToString(sum[:12])
}

// IsName reports whether name has the shape of a snapshot name.
func IsName(name string) bool { return namePattern.MatchString(name) }

// Select reads the selected items out of a source ConfigMap, from data or
// binaryData, exactly as stored.
func Select(source *corev1.ConfigMap, items []operatorv1alpha1.ConfigMapSourceItem) ([]sourcecontent.File, error) {
	files := make([]sourcecontent.File, 0, len(items))
	for _, item := range items {
		if value, ok := source.BinaryData[item.Key]; ok {
			files = append(files, sourcecontent.File{Path: item.Path, Data: bytes.Clone(value)})
			continue
		}
		if value, ok := source.Data[item.Key]; ok {
			files = append(files, sourcecontent.File{Path: item.Path, Data: []byte(value)})
			continue
		}
		return nil, fmt.Errorf("%w: %q", ErrMissingKey, item.Key)
	}
	return files, nil
}

// Build returns the snapshot of files for owner, and its content digest.
func Build(namespace string, owner Owner, files []sourcecontent.File) (*corev1.ConfigMap, string, error) {
	label, err := owner.label()
	if err != nil {
		return nil, "", err
	}
	if owner.Name == "" || owner.UID == "" || namespace == "" {
		return nil, "", errors.New("a snapshot needs its owner's namespace, name and UID")
	}
	sorted, manifest, digest, err := sourcecontent.Canonical(files)
	if err != nil {
		return nil, "", err
	}
	total := len(manifest)
	data := make(map[string][]byte, len(sorted)+1)
	data[sourcecontent.ManifestKey] = manifest
	for index, file := range sorted {
		total += len(file.Data)
		data[sourcecontent.SnapshotFileKey(index)] = bytes.Clone(file.Data)
	}
	if total > sourcecontent.MaxSnapshotBytes {
		return nil, "", fmt.Errorf("%w: the files and their manifest take %d bytes, at most %d",
			ErrTooLarge, total, sourcecontent.MaxSnapshotBytes)
	}
	immutable := true
	controller := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      Name(owner.UID, digest),
			Labels:    map[string]string{label: owner.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               owner.Kind,
				Name:               owner.Name,
				UID:                owner.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &controller,
			}},
		},
		Immutable:  &immutable,
		BinaryData: data,
	}, digest, nil
}

// Verify checks that a stored ConfigMap is a snapshot exactly as Build writes
// one: its owner, labels and name, and files that give the content digest its
// name was derived from. It returns the owner and that digest. It is the check
// the manager applies before it binds an existing snapshot, and the one the
// write webhook applies to the manager's own writes.
func Verify(configMap *corev1.ConfigMap) (Owner, string, error) {
	if !IsName(configMap.Name) {
		return Owner{}, "", fmt.Errorf("%q is not a snapshot name", configMap.Name)
	}
	if len(configMap.OwnerReferences) != 1 {
		return Owner{}, "", errors.New("a snapshot has exactly one owner")
	}
	reference := configMap.OwnerReferences[0]
	owner := Owner{Kind: reference.Kind, Name: reference.Name, UID: reference.UID}
	label, err := owner.label()
	if err != nil {
		return Owner{}, "", err
	}
	if reference.APIVersion != operatorv1alpha1.GroupVersion.String() || owner.Name == "" || owner.UID == "" ||
		reference.Controller == nil || !*reference.Controller ||
		reference.BlockOwnerDeletion == nil || !*reference.BlockOwnerDeletion {
		return Owner{}, "", errors.New("a snapshot is controlled by its resource")
	}
	if len(configMap.Labels) != 1 || configMap.Labels[label] != owner.Name {
		return Owner{}, "", fmt.Errorf("a snapshot carries exactly the %s label naming its owner", label)
	}
	if len(configMap.Annotations) != 0 || len(configMap.Finalizers) != 0 || configMap.GenerateName != "" {
		return Owner{}, "", errors.New("a snapshot carries no annotation, finalizer or generated name")
	}
	if configMap.Immutable == nil || !*configMap.Immutable || len(configMap.Data) != 0 {
		return Owner{}, "", errors.New("a snapshot is immutable and stores its files in binaryData")
	}
	manifest := configMap.BinaryData[sourcecontent.ManifestKey]
	files, err := sourcecontent.Files(manifest, func(index int, _ int64) ([]byte, error) {
		value, ok := configMap.BinaryData[sourcecontent.SnapshotFileKey(index)]
		if !ok {
			return nil, errors.New("the key is missing")
		}
		return value, nil
	})
	if err != nil {
		return Owner{}, "", err
	}
	if len(configMap.BinaryData) != len(files)+1 {
		return Owner{}, "", errors.New("a snapshot holds a key its manifest does not name")
	}
	digest := sourcecontent.DigestOf(manifest)
	if configMap.Name != Name(owner.UID, digest) {
		return Owner{}, "", errors.New("a snapshot's name does not match its owner and content")
	}
	return owner, digest, nil
}

// Same reports whether two snapshots hold the same thing: Verify on the
// stored one, then the same name, owner and bytes as the one Build returned.
func Same(stored, built *corev1.ConfigMap) bool {
	if _, _, err := Verify(stored); err != nil {
		return false
	}
	return stored.Name == built.Name && stored.OwnerReferences[0].UID == built.OwnerReferences[0].UID &&
		maps.EqualFunc(stored.BinaryData, built.BinaryData, bytes.Equal)
}
