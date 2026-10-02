// Package resultstore persists an already authenticated and validated runner
// payload. It provides storage integrity, not sender authentication or protocol
// validation. The receiver must perform those checks before calling Publish.
package resultstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/plancontract"
)

const (
	ChunkBytes      = plancontract.ChunkBytes
	MaxPayloadBytes = plancontract.MaxResultPayloadBytes
	maxChunks       = int((MaxPayloadBytes + ChunkBytes - 1) / ChunkBytes)
	apiVersion      = "operator.ptah.run/v1alpha1"
	labelRecord     = "operator.ptah.run/result-record"
)

var (
	ErrIncomplete = errors.New("runner result publication is incomplete")
	ErrConflict   = errors.New("runner result publication conflicts with stored evidence")
	ErrInvalid    = errors.New("invalid runner result publication")
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	epochPattern  = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
)

// Binding is the authority the receiver verified, including the actual Pod UID.
// None of these fields may be taken on trust from an upload's body.
type Binding struct {
	Namespace          string    `json:"namespace"`
	Kind               string    `json:"kind"`
	Name               string    `json:"name"`
	UID                types.UID `json:"uid"`
	Generation         int64     `json:"generation"`
	ExecutionBindingID string    `json:"executionBindingID"`
	InputFingerprint   string    `json:"inputFingerprint"`
	Operation          string    `json:"operation"`
	OperationID        string    `json:"operationID"`
	JobName            string    `json:"jobName"`
	JobUID             types.UID `json:"jobUID"`
	PodName            string    `json:"podName"`
	PodUID             types.UID `json:"podUID"`
}

func (b Binding) validate() error {
	if len(validation.IsDNS1123Label(b.Namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(b.Name)) != 0 ||
		len(validation.IsDNS1123Subdomain(b.JobName)) != 0 ||
		len(validation.IsDNS1123Subdomain(b.PodName)) != 0 || b.Generation < 1 ||
		!epochPattern.MatchString(b.ExecutionBindingID) || !digestPattern.MatchString(b.InputFingerprint) {
		return ErrInvalid
	}
	for _, value := range []string{string(b.UID), string(b.JobUID), string(b.PodUID), b.OperationID} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, " \t\r\n\x00") {
			return ErrInvalid
		}
	}
	switch b.Kind {
	case "PtahSchema":
		switch b.Operation {
		case "resolve", "verify", "observe", "plan", "apply":
			return nil
		}
	case "PtahMigration":
		switch b.Operation {
		case "resolve", "verify", "migration-history", "migration-apply":
			return nil
		}
	}
	return ErrInvalid
}

// Name reserves one publication per attempt. A replacement Pod, Job, generation,
// or epoch cannot obtain a second receipt under the same reserved attempt name.
func Name(b Binding) (string, error) {
	if err := b.validate(); err != nil {
		return "", err
	}
	key, _ := json.Marshal([]string{b.Namespace, string(b.UID), b.OperationID, b.JobName})
	return "ptah-result-" + digest(key)[len("sha256:"):], nil
}

type manifest struct {
	Version int     `json:"version"`
	Binding Binding `json:"binding"`
	Digest  string  `json:"digest"`
	Size    int64   `json:"size"`
	Chunks  []chunk `json:"chunks"`
}

type chunk struct {
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}

type completion struct {
	ManifestUID    types.UID   `json:"manifestUID"`
	ManifestDigest string      `json:"manifestDigest"`
	ChunkUIDs      []types.UID `json:"chunkUIDs"`
}

// Receipt identifies the immutable completion record. It contains no SQL,
// payload bytes, database credentials, or delivery credentials.
type Receipt struct {
	Name   string
	UID    types.UID
	Digest string
	Size   int64
}

// Store requires a direct API reader. A cached read cannot certify a completed
// publication. The caller supplies request deadlines and limits concurrency.
// Result Secrets are not projected into operation Pods.
type Store struct {
	Client client.Client
	Reader client.Reader
}

// Publish fixes the byte digest before writing any chunks, then creates the
// completion record last. Identical retries resume; conflicting bytes never
// replace even a partial publication. A successful return includes a complete
// readback. It is the only storage state on which a receiver may acknowledge.
//
// payload must already be protocol-validated and independently readable after
// process key loss; storing an old process-sealed Plan is not sufficient.
func (s Store) Publish(ctx context.Context, b Binding, payload []byte, expectedDigest string) (Receipt, error) {
	return s.publish(ctx, b, payload, expectedDigest, nil)
}

// PublishAuthorized rechecks the receiver's live authority after chunk writes,
// immediately before creating the completion record. A duplicate publication
// must pass the same check before its existing receipt is returned. Admission
// and the consuming controller still enforce the current execution epoch.
func (s Store) PublishAuthorized(ctx context.Context, b Binding, payload []byte, expectedDigest string, check func(context.Context) error) (Receipt, error) {
	if check == nil {
		return Receipt{}, ErrInvalid
	}
	return s.publish(ctx, b, payload, expectedDigest, check)
}

func (s Store) publish(ctx context.Context, b Binding, payload []byte, expectedDigest string, check func(context.Context) error) (Receipt, error) {
	name, err := Name(b)
	if err != nil {
		return Receipt{}, err
	}
	if s.Client == nil || s.Reader == nil || len(payload) == 0 || int64(len(payload)) > MaxPayloadBytes ||
		!digestPattern.MatchString(expectedDigest) || digest(payload) != expectedDigest {
		return Receipt{}, ErrInvalid
	}
	m := manifest{Version: 1, Binding: b, Digest: expectedDigest, Size: int64(len(payload))}
	for offset := 0; offset < len(payload); offset += ChunkBytes {
		part := payload[offset:min(offset+ChunkBytes, len(payload))]
		m.Chunks = append(m.Chunks, chunk{Digest: digest(part), Size: len(part)})
	}
	encoded, _ := json.Marshal(m)
	intent := secret(b.Namespace, name, "intent", owner(apiVersion, b.Kind, b.Name, b.UID), encoded)
	if err := s.ensure(ctx, intent); err != nil {
		return Receipt{}, err
	}
	childOwner := owner("v1", "Secret", intent.Name, intent.UID)
	// Once committed, never recreate a missing chunk or rewrite a receipt.
	ready := &corev1.Secret{}
	err = s.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name + "-complete"}, ready)
	if err == nil {
		if check != nil {
			if err := check(ctx); err != nil {
				return Receipt{}, err
			}
		}
		_, receipt, err := s.Load(ctx, b)
		return receipt, err
	}
	if !apierrors.IsNotFound(err) {
		return Receipt{}, err
	}
	c := completion{ManifestUID: intent.UID, ManifestDigest: digest(encoded)}
	for index := range m.Chunks {
		offset := index * ChunkBytes
		part := secret(b.Namespace, chunkName(name, index), "chunk", childOwner,
			payload[offset:min(offset+ChunkBytes, len(payload))])
		if err := s.ensure(ctx, part); err != nil {
			return Receipt{}, err
		}
		c.ChunkUIDs = append(c.ChunkUIDs, part.UID)
	}
	encodedCompletion, _ := json.Marshal(c)
	if check != nil {
		if err := check(ctx); err != nil {
			return Receipt{}, err
		}
	}
	if err := s.ensure(ctx, secret(b.Namespace, name+"-complete", "complete", childOwner, encodedCompletion)); err != nil {
		return Receipt{}, err
	}
	_, receipt, err := s.Load(ctx, b)
	return receipt, err
}

// Load returns nothing until the completion record, identities, lengths, and
// hashes of every chunk agree. It does not read a Job, Pod, or container log.
func (s Store) Load(ctx context.Context, b Binding) ([]byte, Receipt, error) {
	name, err := Name(b)
	if err != nil {
		return nil, Receipt{}, err
	}
	if s.Reader == nil {
		return nil, Receipt{}, ErrInvalid
	}
	intent, err := s.read(ctx, secret(b.Namespace, name, "intent", owner(apiVersion, b.Kind, b.Name, b.UID), nil))
	if err != nil {
		return nil, Receipt{}, err
	}
	var m manifest
	if err := json.Unmarshal(intent.Data["data"], &m); err != nil || m.Version != 1 || m.Binding != b ||
		m.Size <= 0 || m.Size > MaxPayloadBytes || !digestPattern.MatchString(m.Digest) ||
		len(m.Chunks) != int((m.Size+ChunkBytes-1)/ChunkBytes) || len(m.Chunks) > maxChunks {
		return nil, Receipt{}, ErrConflict
	}
	childOwner := owner("v1", "Secret", intent.Name, intent.UID)
	ready, err := s.read(ctx, secret(b.Namespace, name+"-complete", "complete", childOwner, nil))
	if err != nil {
		return nil, Receipt{}, err
	}
	var c completion
	if err := json.Unmarshal(ready.Data["data"], &c); err != nil || c.ManifestUID != intent.UID ||
		c.ManifestDigest != digest(intent.Data["data"]) || len(c.ChunkUIDs) != len(m.Chunks) {
		return nil, Receipt{}, ErrConflict
	}
	payload := make([]byte, 0, int(m.Size))
	for index, ref := range m.Chunks {
		wantSize := min(int(m.Size)-index*ChunkBytes, ChunkBytes)
		if ref.Size != wantSize || !digestPattern.MatchString(ref.Digest) || c.ChunkUIDs[index] == "" {
			return nil, Receipt{}, ErrConflict
		}
		part, err := s.read(ctx, secret(b.Namespace, chunkName(name, index), "chunk", childOwner, nil))
		if err != nil {
			return nil, Receipt{}, err
		}
		if part.UID != c.ChunkUIDs[index] || len(part.Data["data"]) != ref.Size || digest(part.Data["data"]) != ref.Digest {
			return nil, Receipt{}, ErrConflict
		}
		payload = append(payload, part.Data["data"]...)
	}
	if int64(len(payload)) != m.Size || digest(payload) != m.Digest {
		return nil, Receipt{}, ErrConflict
	}
	return payload, Receipt{Name: ready.Name, UID: ready.UID, Digest: m.Digest, Size: m.Size}, nil
}

func (s Store) ensure(ctx context.Context, want *corev1.Secret) error {
	// Preserve the request before Create fills in server-assigned fields.
	expected := want.DeepCopy()
	if err := s.Client.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	got, err := s.read(ctx, expected)
	if err != nil {
		return err
	}
	if !maps.EqualFunc(got.Data, expected.Data, bytes.Equal) {
		return ErrConflict
	}
	*want = *got
	return nil
}

func (s Store) read(ctx context.Context, want *corev1.Secret) (*corev1.Secret, error) {
	got := &corev1.Secret{}
	if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(want), got); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrIncomplete
		}
		return nil, err
	}
	if got.UID == "" || !got.DeletionTimestamp.IsZero() || got.Immutable == nil || !*got.Immutable ||
		got.Type != want.Type || !reflect.DeepEqual(got.OwnerReferences, want.OwnerReferences) ||
		got.Labels[labelRecord] != want.Labels[labelRecord] || len(got.Data) != 1 || len(got.Data["data"]) == 0 {
		return nil, ErrConflict
	}
	return got, nil
}

func secret(namespace, name, role string, ref metav1.OwnerReference, data []byte) *corev1.Secret {
	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name,
			OwnerReferences: []metav1.OwnerReference{ref}, Labels: map[string]string{labelRecord: role}},
		Type: corev1.SecretType("operator.ptah.run/result-" + role + "-v1"), Immutable: &immutable,
		Data: map[string][]byte{"data": bytes.Clone(data)},
	}
}

func owner(version, kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{APIVersion: version, Kind: kind, Name: name, UID: uid, Controller: &controller}
}

func chunkName(name string, index int) string { return fmt.Sprintf("%s-%03d", name, index) }

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
