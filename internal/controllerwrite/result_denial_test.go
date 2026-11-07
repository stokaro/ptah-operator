package controllerwrite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
)

// failingReader is an API server that does not answer. Its error text stands in
// for what a response must never quote.
type failingReader struct{ client.Reader }

const unquotable = "parser diagnostic that must not reach a response"

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New(unquotable)
}

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New(unquotable)
}

// The denial is what a person reads in a namespace's conditions, so its wording
// is the contract here. A refusal about time says which time. Every other
// refusal says none, because no instant would lift it.
func TestResultRecordDenialNamesATimeOnlyWhenTimeIsTheReason(t *testing.T) {
	t.Parallel()

	const (
		name  = "ptah-result-record"
		plain = "result publication retention has not authorized deletion"
		timed = plain + ": record " + name + " cannot be deleted before 2026-11-06T12:40:00Z"
	)
	// The clock is not the record's creation time and not the window's end, so
	// a time computed from the clock cannot produce the one the rows expect.
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	young := now.Add(-20 * time.Minute)
	old := now.Add(-2 * time.Hour)

	record := func(kind string, created time.Time) *operatorv1alpha1.PtahResultRecord {
		return &operatorv1alpha1.PtahResultRecord{
			TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahResultRecord"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: name, UID: "record-uid", CreationTimestamp: metav1.NewTime(created)},
			Spec:       operatorv1alpha1.PtahResultRecordSpec{Type: kind},
		}
	}
	retiring := func(kind string, created time.Time) *operatorv1alpha1.PtahResultRecord {
		object := record(kind, created)
		object.DeletionTimestamp = &metav1.Time{Time: created.Add(time.Minute)}
		object.Finalizers = []string{metav1.FinalizerDeleteDependents}
		return object
	}
	owned := func(kind string, created time.Time) *operatorv1alpha1.PtahResultRecord {
		object := record(kind, created)
		object.OwnerReferences = []metav1.OwnerReference{{Kind: "PtahResultRecord", Name: "ptah-result-intent", UID: "intent-uid"}}
		return object
	}

	for _, row := range []struct {
		name string
		// object is the stored record; next is the candidate of an UPDATE.
		object, next *operatorv1alpha1.PtahResultRecord
		operation    admissionv1.Operation
		// collection sends the request without a name, as a namespace's
		// deletion does.
		collection bool
		options    string
		noPolicy   bool
		want       string
	}{
		{name: "a young record", object: record("intent", young), operation: admissionv1.Delete, want: timed},
		{name: "a young record in a collection deletion", object: record("intent", young), operation: admissionv1.Delete, collection: true, want: timed},
		{name: "a young credential", object: record("credential", young), operation: admissionv1.Delete, want: timed},
		// The retirement a foreground request asks for is refused too, since this
		// record cannot be bound. The time the ordinary deletion named still holds.
		{name: "a young record whose foreground retirement is refused", object: record("intent", young), operation: admissionv1.Delete, options: `{"propagationPolicy":"Foreground"}`, want: timed},
		{
			name: "the garbage collector finishing a young foreground deletion", operation: admissionv1.Update,
			object: retiring("intent", young),
			next: func() *operatorv1alpha1.PtahResultRecord {
				object := retiring("intent", young)
				object.Finalizers = nil
				return object
			}(),
			want: timed,
		},
		{name: "an old record that cannot be bound", object: record("intent", old), operation: admissionv1.Delete, want: plain},
		{name: "an old record the policy cannot read for", object: owned("complete", old), operation: admissionv1.Delete, want: plain},
		{name: "an old record whose foreground retirement is refused", object: record("intent", old), operation: admissionv1.Delete, options: `{"propagationPolicy":"Foreground"}`, want: plain},
		{name: "an installation with no retention policy", object: record("intent", young), operation: admissionv1.Delete, noPolicy: true, want: plain},
		{name: "an old credential that cannot be bound", object: record("credential", old), operation: admissionv1.Delete, want: plain},
		{name: "an old credential in a collection deletion", object: record("credential", old), operation: admissionv1.Delete, collection: true, want: plain},
		{name: "an old credential whose foreground retirement is refused", object: record("credential", old), operation: admissionv1.Delete, options: `{"propagationPolicy":"Foreground"}`, want: plain},
		{name: "a credential in an installation with no retention policy", object: record("credential", young), operation: admissionv1.Delete, noPolicy: true, want: plain},
		{
			name: "the garbage collector finishing an old foreground deletion that cannot be bound", operation: admissionv1.Update,
			object: retiring("intent", old),
			next: func() *operatorv1alpha1.PtahResultRecord {
				object := retiring("intent", old)
				object.Finalizers = nil
				return object
			}(),
			want: plain,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			validator := &controllerwrite.Validator{Reader: failingReader{}, Jobs: staticJobBuilder{}, ManagerUsername: managerUsername}
			if !row.noPolicy {
				validator.ResultCleanup = &resultcleanup.Policy{Reader: failingReader{}, Window: resultretention.MinimumWindow, Now: func() time.Time { return now }}
			}
			request := cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				UID:       "request-uid",
				Name:      row.object.Name,
				Namespace: row.object.Namespace,
				Operation: row.operation,
				Kind:      metav1.GroupVersionKind{Group: "operator.ptah.run", Version: "v1alpha1", Kind: "PtahResultRecord"},
				Resource:  metav1.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahresultrecords"},
				OldObject: rawObject(t, row.object),
			}}
			if row.collection {
				request.Name = ""
			}
			if row.options != "" {
				request.Options = runtime.RawExtension{Raw: []byte(row.options)}
			}
			if row.next != nil {
				request.Object = rawObject(t, row.next)
			}

			response := (&controllerwrite.ValidationHandler{Validator: validator}).Handle(t.Context(), request)
			if response.Allowed {
				t.Fatal("the record was admitted for deletion")
			}
			if response.Result == nil || response.Result.Message != row.want {
				t.Fatalf("denial = %#v, want %q", response.Result, row.want)
			}
		})
	}
}

// refusingCredentials refuses everything with a reason no response may quote.
type refusingCredentials struct{}

func (refusingCredentials) ValidateCreate(context.Context, *corev1.Secret) error {
	return errors.New(unquotable)
}

func (refusingCredentials) ValidateRecordCreate(context.Context, *operatorv1alpha1.PtahResultRecord) error {
	return errors.New(unquotable)
}

func (refusingCredentials) AuthorizePublication(context.Context, resultstore.Binding) (resultdelivery.Identity, error) {
	return resultdelivery.Identity{}, errors.New(unquotable)
}

// A refusal says what was refused. Only an update changes something a record
// binds, so only an update is told about that binding; a deletion is told about
// retention, and a creation about its operation.
func TestResultWriteDenialSaysWhatWasRefused(t *testing.T) {
	t.Parallel()

	const (
		recordName = "ptah-result-record"
		secretName = jobconfig.SecretPrefix + "record"
	)
	recordResource := metav1.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahresultrecords"}
	recordKind := metav1.GroupVersionKind{Group: "operator.ptah.run", Version: "v1alpha1", Kind: "PtahResultRecord"}
	secretResource := metav1.GroupVersionResource{Version: "v1", Resource: "secrets"}
	secretKind := metav1.GroupVersionKind{Version: "v1", Kind: "Secret"}

	record := func(name, kind string) *operatorv1alpha1.PtahResultRecord {
		return &operatorv1alpha1.PtahResultRecord{
			TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahResultRecord"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: name, UID: "record-uid"},
			Spec:       operatorv1alpha1.PtahResultRecordSpec{Type: kind, Data: []byte(`{}`)},
		}
	}
	changed := func(object *operatorv1alpha1.PtahResultRecord) *operatorv1alpha1.PtahResultRecord {
		next := object.DeepCopy()
		next.Labels = map[string]string{"moved": "true"}
		return next
	}
	secret := func() *corev1.Secret {
		return &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: secretName, UID: "secret-uid"},
		}
	}
	changedSecret := secret()
	changedSecret.Labels = map[string]string{"moved": "true"}

	for _, row := range []struct {
		name      string
		resource  metav1.GroupVersionResource
		kind      metav1.GroupVersionKind
		object    string
		operation admissionv1.Operation
		// next and old are the request's candidate and stored objects.
		next, old any
		want      string
	}{
		{
			name: "a retirement the operation refuses", operation: admissionv1.Create,
			resource: recordResource, kind: recordKind, object: recordName + "-retired",
			next: record(recordName+"-retired", "retired"),
			want: "result record creation is not authorized for its operation",
		},
		{
			name: "a credential record the operation refuses", operation: admissionv1.Create,
			resource: recordResource, kind: recordKind, object: secretName,
			next: record(secretName, "credential"),
			want: "result record creation is not authorized for its operation",
		},
		{
			name: "a publication the operation refuses", operation: admissionv1.Create,
			resource: recordResource, kind: recordKind, object: recordName,
			next: record(recordName, "intent"),
			want: "result record creation is not authorized for its operation",
		},
		{
			name: "an intent that changes", operation: admissionv1.Update,
			resource: recordResource, kind: recordKind, object: recordName,
			old: record(recordName, "intent"), next: changed(record(recordName, "intent")),
			want: "result record update violates its immutable operation binding",
		},
		{
			name: "a credential record that changes", operation: admissionv1.Update,
			resource: recordResource, kind: recordKind, object: secretName,
			old: record(secretName, "credential"), next: changed(record(secretName, "credential")),
			want: "result record update violates its immutable operation binding",
		},
		{
			name: "a credential Secret the operation refuses", operation: admissionv1.Create,
			resource: secretResource, kind: secretKind, object: secretName,
			next: secret(),
			want: "result credential creation is not authorized for its operation",
		},
		{
			name: "a credential Secret that changes", operation: admissionv1.Update,
			resource: secretResource, kind: secretKind, object: secretName,
			old: secret(), next: changedSecret,
			want: "result credential update violates its immutable operation binding",
		},
		{
			name: "a credential Secret retention keeps", operation: admissionv1.Delete,
			resource: secretResource, kind: secretKind, object: secretName,
			old:  secret(),
			want: "result credential retention has not authorized deletion",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			validator := &controllerwrite.Validator{
				Reader: failingReader{}, Jobs: staticJobBuilder{}, ManagerUsername: managerUsername,
				ResultCredentials: refusingCredentials{},
				ResultCleanup:     &resultcleanup.Policy{Reader: failingReader{}, Window: resultretention.MinimumWindow},
			}
			request := cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				UID:       "request-uid",
				Name:      row.object,
				Namespace: "application",
				Operation: row.operation,
				Kind:      row.kind,
				Resource:  row.resource,
				UserInfo:  authenticationv1.UserInfo{Username: managerUsername},
			}}
			if row.next != nil {
				request.Object = rawObject(t, row.next)
			}
			if row.old != nil {
				request.OldObject = rawObject(t, row.old)
			}

			response := (&controllerwrite.ValidationHandler{Validator: validator}).Handle(t.Context(), request)
			if response.Allowed {
				t.Fatal("the write was admitted")
			}
			if response.Result == nil || response.Result.Message != row.want {
				t.Fatalf("denial = %#v, want %q", response.Result, row.want)
			}
		})
	}
}
