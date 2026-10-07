package resultauthority

import (
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultretention"
)

func TestAuthorityUsesCurrentClaimAfterWorkloadReads(t *testing.T) {
	for _, family := range []string{"schema-observe", "migration-history"} {
		t.Run(family, func(t *testing.T) {
			f := resulttest.New(t, family)
			var reads []string
			reader := readerHook{Reader: f.Client(t), beforeGet: func(obj client.Object) error {
				switch obj.(type) {
				case *batchv1.Job:
					reads = append(reads, "job")
				case *api.PtahSchema, *api.PtahMigration:
					reads = append(reads, "claim")
				case *api.PtahResultRecord:
					reads = append(reads, "retirement")
				default:
					t.Fatalf("unexpected authority read %T", obj)
				}
				return nil
			}, list: func(*corev1.PodList) error {
				reads = append(reads, "pods")
				return nil
			}}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(reads, ","); got != "job,pods,claim,retirement" {
				t.Fatalf("authority must validate the workload against one later claim and finish at the live retirement fence; reads=%s", got)
			}
		})
	}
}

func TestAuthorityRefusesRetirementAfterCurrentClaim(t *testing.T) {
	for _, family := range []string{"schema-observe", "migration-history"} {
		t.Run(family, func(t *testing.T) {
			f := resulttest.New(t, family)
			c := f.Client(t)
			podsRead, retired := false, false
			reader := readerHook{Reader: c, list: func(*corev1.PodList) error {
				podsRead = true
				return nil
			}, get: func(obj client.Object) error {
				switch obj.(type) {
				case *api.PtahSchema, *api.PtahMigration:
					if podsRead && !retired {
						name, err := resultretention.Name(f.Identity.Binding)
						if err != nil {
							return err
						}
						retired = true
						// Even an unreadable marker fences a restored active claim.
						return c.Create(t.Context(), &api.PtahResultRecord{ObjectMeta: metav1.ObjectMeta{Namespace: f.Job.Namespace, Name: name}})
					}
				}
				return nil
			}}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("retirement after the claim read did not revoke authority: %v", err)
			}
			if !retired {
				t.Fatal("did not inject retirement after reading the workload and claim")
			}
		})
	}
}

func TestRetirementRemainsDefinitiveDuringAdoption(t *testing.T) {
	for _, family := range []string{"schema-observe", "migration-history"} {
		t.Run(family, func(t *testing.T) {
			f := resulttest.New(t, family)
			switch subject := f.Subject.(type) {
			case *api.PtahSchema:
				subject.Status.ActiveOperation.JobUID = ""
			case *api.PtahMigration:
				subject.Status.ActiveOperation.JobUID = ""
			}
			name, err := resultretention.Name(f.Identity.Binding)
			if err != nil {
				t.Fatal(err)
			}
			marker := &api.PtahResultRecord{ObjectMeta: metav1.ObjectMeta{Namespace: f.Job.Namespace, Name: name}}
			if err := (Authorizer{Reader: f.Client(t, marker)}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("retired claim became a retryable adoption wait: %v", err)
			}
		})
	}
}
