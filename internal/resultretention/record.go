package resultretention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MinimumWindow exceeds the qualification profile's five-minute backup lag
// plus thirty-minute combined recovery objective. Installations can retain
// evidence longer; changing configuration cannot shorten an existing marker.
const MinimumWindow = time.Hour

var (
	ErrRetired = errors.New("result delivery attempt has been retired")
	ErrWindow  = errors.New("result evidence retention window has not elapsed")
	ErrRecord  = errors.New("invalid result retirement record")
)

// Source identifies the persisted root whose authenticated binding was read
// before retirement. It carries no credential material or result payload.
type Source struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	Type string    `json:"type"`
}

type Retirement struct {
	Version          int                 `json:"version"`
	Binding          resultstore.Binding `json:"binding"`
	Source           Source              `json:"source"`
	RetentionSeconds int64               `json:"retentionSeconds"`
}

func Name(b resultstore.Binding) (string, error) {
	name, err := resultstore.Name(b)
	return name + "-retired", err
}

// Record fixes the original source and minimum duration. The API server's
// creationTimestamp, not a client clock or the Job's age, starts the window.
func Record(b resultstore.Binding, source Source, window time.Duration) (*api.PtahResultRecord, error) {
	name, err := Name(b)
	if err != nil || window < MinimumWindow || window%time.Second != 0 {
		return nil, ErrRecord
	}
	r := Retirement{Version: 1, Binding: b, Source: source, RetentionSeconds: int64(window / time.Second)}
	if !validSource(r) {
		return nil, ErrRecord
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, ErrRecord
	}
	return &api.PtahResultRecord{ObjectMeta: metav1.ObjectMeta{
		Namespace: b.Namespace, Name: name,
		Labels:          map[string]string{"operator.ptah.run/result-record": "retired"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: b.Kind, Name: b.Name, UID: b.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false)}},
	}, Spec: api.PtahResultRecordSpec{Type: "retired", Data: data}}, nil
}

func validSource(r Retirement) bool {
	if r.Source.UID == "" {
		return false
	}
	switch r.Source.Type {
	case "credential":
		return r.Source.Name == jobconfig.CredentialName(r.Binding.UID, r.Binding.OperationID, r.Binding.JobName)
	case "intent":
		name, err := resultstore.Name(r.Binding)
		return err == nil && r.Source.Name == name
	default:
		return false
	}
}

// Decode validates immutable retirement bytes and metadata. It is not a live
// retirement check: the source and resource must be read before creation.
func Decode(record *api.PtahResultRecord) (Retirement, error) {
	var r Retirement
	if record == nil || record.Spec.Type != "retired" || len(record.Spec.Data) == 0 || len(record.Spec.Data) > resultstore.ChunkBytes || json.Unmarshal(record.Spec.Data, &r) != nil || r.Version != 1 ||
		r.RetentionSeconds < int64(MinimumWindow/time.Second) || r.RetentionSeconds > int64((time.Duration(1<<63-1))/time.Second) {
		return r, ErrRecord
	}
	want, err := Record(r.Binding, r.Source, time.Duration(r.RetentionSeconds)*time.Second)
	if err != nil || !bytes.Equal(want.Spec.Data, record.Spec.Data) || record.Name != want.Name || record.Namespace != want.Namespace || record.GenerateName != "" ||
		len(record.Annotations) != 0 || !reflect.DeepEqual(record.Labels, want.Labels) || !reflect.DeepEqual(record.OwnerReferences, want.OwnerReferences) {
		return r, ErrRecord
	}
	if len(record.Finalizers) != 0 && (record.DeletionTimestamp.IsZero() || len(record.Finalizers) != 1 || record.Finalizers[0] != metav1.FinalizerDeleteDependents) {
		return r, ErrRecord
	}
	return r, nil
}

// CheckOpen fences issuance and delivery, including a restored active claim.
// Even an unreadable marker refuses authority; it must never be treated as an
// absent retirement record. Consumers may still read already-persisted bytes.
func CheckOpen(ctx context.Context, reader client.Reader, b resultstore.Binding) error {
	name, err := Name(b)
	if err != nil || reader == nil {
		return ErrRecord
	}
	record := &api.PtahResultRecord{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, record); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return ErrRetired
}

// Eligible rechecks recovery pins on every deletion. A marker's age alone is
// insufficient, and a longer current policy also applies to older markers.
func Eligible(ctx context.Context, reader client.Reader, record *api.PtahResultRecord, now time.Time, window time.Duration) error {
	r, err := Decode(record)
	if err != nil || record.UID == "" || record.CreationTimestamp.IsZero() || window < MinimumWindow {
		return ErrRecord
	}
	if err := CheckUnpinned(ctx, reader, r.Binding); err != nil {
		return err
	}
	deadline := record.CreationTimestamp.Add(max(window, time.Duration(r.RetentionSeconds)*time.Second))
	if now.Before(deadline) {
		return ErrWindow
	}
	return nil
}
