package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnrollmentHintAvoidsRepeatedReadsAndExpires(t *testing.T) {
	for _, name := range []string{"schema-observe", "migration-history"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.NewPodToken(t, name, enrollmentTrust(t))
			c := enrollmentAPI{f.Client(t)}
			reader := &enrollmentReader{Reader: c}
			issuer := &enrollmentProbe{PodBindings: resultcredentials.PodBindings{Writer: c, Reader: reader}}
			now := time.Unix(100, 0)
			hints := &resultEnrollmentHints{now: func() time.Time { return now }}
			if ready, err := invokeIssuanceWithHints(t.Context(), f, reader, issuer, hints); err != nil || !ready {
				t.Fatal(ready, err)
			}
			reader.reads = 0
			for range 5 {
				if ready, err := invokeIssuanceWithHints(t.Context(), f, reader, issuer, hints); err != nil || !ready {
					t.Fatal(ready, err)
				}
			}
			if issuer.calls != 1 || reader.reads != 0 {
				t.Fatalf("repeated Job events used %d reads and %d enrollments", reader.reads, issuer.calls)
			}
			// Repeated hits cannot extend the lifetime of the observation.
			now = now.Add(resultEnrollmentHintTTL)
			if ready, err := invokeIssuanceWithHints(t.Context(), f, reader, issuer, hints); err != nil || !ready || reader.reads != 2 || issuer.calls != 1 {
				t.Fatalf("expiration did not read the original Pod and pin: ready=%v reads=%d calls=%d err=%v", ready, reader.reads, issuer.calls, err)
			}
		})
	}
}

func TestEnrollmentHintNeverGrantsPublicationAuthority(t *testing.T) {
	for _, family := range []string{"schema-observe", "migration-history"} {
		for _, failure := range []string{"revoked claim", "replacement Pod", "unavailable API"} {
			t.Run(family+"/"+failure, func(t *testing.T) {
				f := resulttest.NewPodToken(t, family, enrollmentTrust(t))
				c := enrollmentAPI{f.Client(t)}
				reader := &enrollmentReader{Reader: c}
				issuer := &enrollmentProbe{PodBindings: resultcredentials.PodBindings{Writer: c, Reader: reader}}
				hints := &resultEnrollmentHints{}
				if ready, err := invokeIssuanceWithHints(t.Context(), f, reader, issuer, hints); err != nil || !ready {
					t.Fatal(ready, err)
				}
				switch failure {
				case "revoked claim":
					revoked := f.Subject.DeepCopyObject().(client.Object)
					switch object := revoked.(type) {
					case *api.PtahSchema:
						object.Status.ActiveOperation = nil
					case *api.PtahMigration:
						object.Status.ActiveOperation = nil
					}
					if err := c.Update(t.Context(), revoked); err != nil {
						t.Fatal(err)
					}
				case "replacement Pod":
					if err := c.Delete(t.Context(), f.Pod); err != nil {
						t.Fatal(err)
					}
					replacement := f.Pod.DeepCopy()
					replacement.UID, replacement.ResourceVersion = "replacement", ""
					if err := c.Client.Create(t.Context(), replacement); err != nil {
						t.Fatal(err)
					}
				case "unavailable API":
					reader.fail = errors.New("API unavailable")
				}
				// Reconciliation may still hold the pre-change Job and subject.
				reader.reads = 0
				if ready, err := invokeIssuanceWithHints(t.Context(), f, reader, issuer, hints); err != nil || !ready || reader.reads != 0 {
					t.Fatalf("enrollment hint was not exercised: ready=%v reads=%d err=%v", ready, reader.reads, err)
				}
				_, err := issuer.AuthorizePublication(t.Context(), f.Identity.Binding)
				if err == nil || failure != "unavailable API" && !errors.Is(err, resultdelivery.ErrAuthority) {
					t.Fatalf("hint authorized publication after %s: %v", failure, err)
				}
			})
		}
	}
}

func TestEnrollmentHintDoesNotCoverReplacementJobOrCanceledPass(t *testing.T) {
	for _, failure := range []string{"replacement Job", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			f := resulttest.NewPodToken(t, "schema-observe", enrollmentTrust(t))
			c := enrollmentAPI{f.Client(t)}
			reader := &enrollmentReader{Reader: c}
			issuer := &enrollmentProbe{PodBindings: resultcredentials.PodBindings{Writer: c, Reader: reader}}
			hints := &resultEnrollmentHints{}
			if ready, err := invokeIssuanceWithHints(t.Context(), f, reader, issuer, hints); err != nil || !ready {
				t.Fatal(ready, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "canceled" {
				cancel()
			} else {
				f.Job.UID = "replacement"
			}
			if ready, err := invokeIssuanceWithHints(ctx, f, reader, issuer, hints); ready || issuer.calls != 1 || failure == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("hint covered %s: ready=%v calls=%d err=%v", failure, ready, issuer.calls, err)
			}
		})
	}
}

func TestEnrollmentHintsStayBounded(t *testing.T) {
	now := time.Unix(100, 0)
	hints := &resultEnrollmentHints{now: func() time.Time { return now }}
	for i := range 2 * resultEnrollmentHintLimit {
		hints.remember(resultconsumer.Request{OperationID: fmt.Sprint(i)})
	}
	if len(hints.entries) != resultEnrollmentHintLimit || hints.known(resultconsumer.Request{OperationID: fmt.Sprint(2*resultEnrollmentHintLimit - 1)}) {
		t.Fatal("enrollment hints exceeded their fixed memory bound")
	}
	now = now.Add(resultEnrollmentHintTTL)
	request := resultconsumer.Request{OperationID: "new"}
	hints.remember(request)
	if len(hints.entries) != 1 || !hints.known(request) {
		t.Fatal("expired hints did not leave room for new enrollment")
	}
}
