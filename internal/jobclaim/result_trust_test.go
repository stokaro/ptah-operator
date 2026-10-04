package jobclaim_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	batchv1 "k8s.io/api/batch/v1"
)

func claimTrust(t *testing.T) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestJobClaimPinsPublicTrustAcrossReceiverRotation(t *testing.T) {
	oldTrust, nextTrust := claimTrust(t), claimTrust(t)
	setTrust := func(job *batchv1.Job, trust []byte) {
		for i := range job.Spec.Template.Spec.Containers[0].Env {
			if job.Spec.Template.Spec.Containers[0].Env[i].Name == jobconfig.ServerTrust {
				job.Spec.Template.Spec.Containers[0].Env[i].Value = string(trust)
				return
			}
		}
		t.Fatal("fixture has no public trust")
	}
	for _, name := range []string{"schema-observe", "migration-history"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.NewPodToken(t, name, oldTrust)
			var claim jobclaim.Claim
			switch subject := f.Subject.(type) {
			case *api.PtahSchema:
				claim = jobclaim.SchemaOperation(subject, subject.Status.ActiveOperation)
			case *api.PtahMigration:
				claim = jobclaim.MigrationOperation(subject, subject.Status.ActiveOperation)
			default:
				t.Fatal("unknown resource family")
			}
			claim.Built = f.Job.DeepCopy()
			setTrust(claim.Built, nextTrust)
			if err := jobclaim.Match(f.Job, claim); err != nil {
				t.Fatalf("receiver CA rotation invalidated the captured Job: %v", err)
			}
			changed := f.Job.DeepCopy()
			setTrust(changed, nextTrust)
			if jobclaim.Match(changed, claim) == nil {
				t.Fatal("changed public trust escaped the stored template digest")
			}
			changed = f.Job.DeepCopy()
			changed.Spec.Template.Spec.Volumes[len(changed.Spec.Template.Spec.Volumes)-1].Projected.Sources[0].ServiceAccountToken.Audience = "kubernetes"
			if jobclaim.Match(changed, claim) == nil {
				t.Fatal("CA overlap granted a broader token projection")
			}
		})
	}
}
