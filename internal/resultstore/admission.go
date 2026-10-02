package resultstore

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ValidateRecordCreate checks publication structure through direct API reads.
// It returns the intent's binding and, for a completion, the complete payload.
// The caller must authenticate the writer, authorize this binding against live
// operation authority, and validate the completed protocol before admitting it.
// Nothing is written and no receipt is issued here.
func (s Store) ValidateRecordCreate(ctx context.Context, candidate *api.PtahResultRecord) (Binding, []byte, error) {
	if s.Reader == nil || candidate == nil || !candidate.DeletionTimestamp.IsZero() || len(candidate.Spec.Data) == 0 || len(candidate.Spec.Data) > ChunkBytes {
		return Binding{}, nil, ErrInvalid
	}
	var intent *api.PtahResultRecord
	if candidate.Spec.Type == "intent" {
		intent = candidate
	} else {
		if (candidate.Spec.Type != "chunk" && candidate.Spec.Type != "complete") || len(candidate.OwnerReferences) != 1 {
			return Binding{}, nil, ErrInvalid
		}
		ref := candidate.OwnerReferences[0]
		if ref.APIVersion != apiVersion || ref.Kind != "PtahResultRecord" || ref.Name == "" || ref.UID == "" {
			return Binding{}, nil, ErrInvalid
		}
		intent = &api.PtahResultRecord{}
		if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: candidate.Namespace, Name: ref.Name}, intent); err != nil {
			return Binding{}, nil, err
		}
		if intent.UID != ref.UID || !intent.DeletionTimestamp.IsZero() {
			return Binding{}, nil, ErrConflict
		}
	}
	m, name, err := admissionManifest(intent)
	if err != nil {
		return Binding{}, nil, err
	}
	childOwner := owner(apiVersion, "PtahResultRecord", name, intent.UID)
	var payload []byte
	switch candidate.Spec.Type {
	case "intent":
		// admissionManifest already checked its exact metadata and bytes.
	case "chunk":
		index := -1
		for n := range m.Chunks {
			if candidate.Name == chunkName(name, n) {
				index = n
				break
			}
		}
		if index < 0 || !recordShape(candidate, record(m.Binding.Namespace, chunkName(name, index), "chunk", childOwner, nil)) {
			return Binding{}, nil, ErrInvalid
		}
		ref := m.Chunks[index]
		if len(candidate.Spec.Data) != ref.Size || digest(candidate.Spec.Data) != ref.Digest {
			return Binding{}, nil, ErrConflict
		}
	case "complete":
		if !recordShape(candidate, record(m.Binding.Namespace, name+"-complete", "complete", childOwner, nil)) {
			return Binding{}, nil, ErrInvalid
		}
		var c completion
		if !canonicalRecord(candidate.Spec.Data, &c) || c.ManifestUID != intent.UID || c.ManifestDigest != digest(intent.Spec.Data) || len(c.ChunkUIDs) != len(m.Chunks) {
			return Binding{}, nil, ErrConflict
		}
		payload = make([]byte, 0, int(m.Size))
		for n, ref := range m.Chunks {
			want := record(m.Binding.Namespace, chunkName(name, n), "chunk", childOwner, nil)
			part, err := s.read(ctx, want)
			if err != nil {
				return Binding{}, nil, err
			}
			if !recordShape(part, want) || c.ChunkUIDs[n] == "" || part.UID != c.ChunkUIDs[n] || len(part.Spec.Data) != ref.Size || digest(part.Spec.Data) != ref.Digest {
				return Binding{}, nil, ErrConflict
			}
			payload = append(payload, part.Spec.Data...)
		}
		if int64(len(payload)) != m.Size || digest(payload) != m.Digest {
			return Binding{}, nil, ErrConflict
		}
	default:
		return Binding{}, nil, ErrInvalid
	}
	// Identical CREATE retries are admitted so the API can return AlreadyExists.
	// A completed publication may never be healed by recreating a missing member.
	existing := &api.PtahResultRecord{}
	err = s.Reader.Get(ctx, client.ObjectKeyFromObject(candidate), existing)
	if err == nil {
		if existing.UID == "" || !existing.DeletionTimestamp.IsZero() || !recordShape(existing, candidate) || !bytes.Equal(existing.Spec.Data, candidate.Spec.Data) {
			return Binding{}, nil, ErrConflict
		}
	} else if apierrors.IsNotFound(err) {
		// A completion racing this same CREATE is an identical-write race,
		// not recreation of a missing member. Let the API resolve that race.
		if candidate.Spec.Type != "complete" {
			ready := &api.PtahResultRecord{}
			err = s.Reader.Get(ctx, client.ObjectKey{Namespace: m.Binding.Namespace, Name: name + "-complete"}, ready)
			if err == nil {
				return Binding{}, nil, ErrConflict
			}
			if !apierrors.IsNotFound(err) {
				return Binding{}, nil, err
			}
		}
	} else {
		return Binding{}, nil, err
	}
	return m.Binding, payload, nil
}

func admissionManifest(intent *api.PtahResultRecord) (manifest, string, error) {
	var m manifest
	if intent == nil || len(intent.Spec.Data) == 0 || len(intent.Spec.Data) > ChunkBytes || !intent.DeletionTimestamp.IsZero() || !canonicalRecord(intent.Spec.Data, &m) || m.Version != 1 || m.Size <= 0 || m.Size > MaxPayloadBytes || !digestPattern.MatchString(m.Digest) || len(m.Chunks) != int((m.Size+ChunkBytes-1)/ChunkBytes) || len(m.Chunks) > maxChunks {
		return m, "", ErrInvalid
	}
	name, err := Name(m.Binding)
	if err != nil || !recordShape(intent, record(m.Binding.Namespace, name, "intent", owner(apiVersion, m.Binding.Kind, m.Binding.Name, m.Binding.UID), nil)) {
		return m, "", ErrInvalid
	}
	for n, ref := range m.Chunks {
		if ref.Size != min(int(m.Size)-n*ChunkBytes, ChunkBytes) || !digestPattern.MatchString(ref.Digest) {
			return m, "", ErrInvalid
		}
	}
	return m, name, nil
}

func canonicalRecord(data []byte, value any) bool {
	if json.Unmarshal(data, value) != nil {
		return false
	}
	encoded, err := json.Marshal(value)
	return err == nil && bytes.Equal(encoded, data)
}

// Server bookkeeping is not caller-controlled publication metadata.
func recordShape(got, want *api.PtahResultRecord) bool {
	return got.Namespace == want.Namespace && got.Name == want.Name && got.GenerateName == "" && len(got.Annotations) == 0 && len(got.Finalizers) == 0 &&
		reflect.DeepEqual(got.Labels, want.Labels) && reflect.DeepEqual(got.OwnerReferences, want.OwnerReferences) && got.Spec.Type == want.Spec.Type
}

// ValidateRecordUpdate preserves both immutable bytes and their ownership.
// API bookkeeping may change without changing publication authority.
func ValidateRecordUpdate(old, next *api.PtahResultRecord) error {
	if old == nil || next == nil || old.UID == "" || old.UID != next.UID {
		return ErrInvalid
	}
	a, b := old.DeepCopy(), next.DeepCopy()
	a.ResourceVersion, b.ResourceVersion = "", ""
	a.ManagedFields, b.ManagedFields = nil, nil
	a.DeletionTimestamp, b.DeletionTimestamp = nil, nil
	a.DeletionGracePeriodSeconds, b.DeletionGracePeriodSeconds = nil, nil
	if !reflect.DeepEqual(a, b) {
		return ErrConflict
	}
	return nil
}
