package resultcleanup

import (
	"context"
	"errors"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type policyReads struct {
	client.Reader
	gets map[client.ObjectKey]int
	fail client.ObjectKey
}

func (r *policyReads) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets[key]++
	if key == r.fail {
		return errWrite
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func TestDeletionReadsSharedSourceAndOwnerOnce(t *testing.T) {
	for _, role := range []string{"complete", "chunk"} {
		t.Run(role, func(t *testing.T) {
			f := fixture(t)
			f.retire(t)
			var target api.PtahResultRecord
			for _, record := range remaining(t, f.c) {
				if role == "chunk" && record.Spec.Type == "complete" {
					if err := f.c.Client.Delete(t.Context(), &record); err != nil {
						t.Fatal(err)
					}
				}
				if record.Spec.Type == role {
					target = record
				}
			}
			if target.UID == "" {
				t.Fatal("missing published member")
			}
			owner := client.ObjectKey{Namespace: target.Namespace, Name: target.OwnerReferences[0].Name}
			reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
			f.p.Reader = reader
			if err := f.p.AuthorizeDelete(t.Context(), &target); err != nil {
				t.Fatal(err)
			}
			if got := reader.gets[owner]; got != 1 {
				t.Fatalf("one deletion read the same source/owner %d times; want one current API read", got)
			}
			reader.fail = owner
			if err := f.p.AuthorizeDelete(t.Context(), &target); !errors.Is(err, errWrite) {
				t.Fatalf("unavailable current owner did not retain evidence: %v", err)
			}
		})
	}
}

func TestDeletionStillReadsOwnerWhenRetirementHasAnotherSource(t *testing.T) {
	f := fixture(t)
	b := f.f.Identity.Binding
	credentialName := jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)
	var err error
	f.marker, err = resultretention.Record(b,
		resultretention.Source{Name: credentialName, UID: "original-credential", Type: "credential"}, f.p.Window)
	if err != nil {
		t.Fatal(err)
	}
	f.retire(t)
	var target api.PtahResultRecord
	for _, record := range remaining(t, f.c) {
		if record.Spec.Type == "complete" {
			target = record
		}
	}
	if target.UID == "" {
		t.Fatal("missing completion")
	}
	owner := client.ObjectKey{Namespace: target.Namespace, Name: target.OwnerReferences[0].Name}
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}, fail: owner}
	f.p.Reader = reader
	if err := f.p.AuthorizeDelete(t.Context(), &target); !errors.Is(err, errWrite) {
		t.Fatalf("missing separate source bypassed the live owner read: %v", err)
	}
	if reader.gets[owner] != 1 || reader.gets[client.ObjectKey{Namespace: target.Namespace, Name: credentialName}] != 1 {
		t.Fatalf("expected current source and owner reads: %v", reader.gets)
	}
}
