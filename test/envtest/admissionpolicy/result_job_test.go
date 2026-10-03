package admissionpolicy_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// These requests use the actual Job builder and the installed policy. A
// receiver that accepts its own test fixtures cannot establish that Kubernetes
// admits the Jobs the controller must dispatch to reach it.
func resultJobRows(t *testing.T, c *catalog) {
	guard := policy(t, "ptah-operator-job-write-guard-")
	builder := managerBuilder()
	builder.ResultEndpoint = "https://receiver.operator.svc"
	admitted := []string{}
	for _, family := range []struct {
		name  string
		build func(workload.Builder) (*batchv1.Job, error)
	}{{"schema", resolveJobBuiltBy}, {"migration", migrationResolveJobBuiltBy}} {
		name := "manager dispatches a durable " + family.name + " Job"
		admitted = append(admitted, name)
		c.row(policyenv.Row{Name: name, Do: as(env.Manager(), dryRunCreate(func() (client.Object, error) {
			return family.build(builder)
		}))})
	}
	refused := []string{}
	for _, test := range []struct {
		name string
		edit func(*batchv1.Job)
	}{
		{"unreserved Secret", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes[len(j.Spec.Template.Spec.Volumes)-1].Secret.SecretName = "database"
		}},
		{"world-readable key", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes[len(j.Spec.Template.Spec.Volumes)-1].Secret.DefaultMode = ptr.To(int32(0644))
		}},
		{"writable mount", func(j *batchv1.Job) {
			mounts := j.Spec.Template.Spec.Containers[0].VolumeMounts
			mounts[len(mounts)-1].ReadOnly = false
		}},
		{"init container access", func(j *batchv1.Job) {
			j.Spec.Template.Spec.InitContainers[0].VolumeMounts = append(j.Spec.Template.Spec.InitContainers[0].VolumeMounts, corev1.VolumeMount{Name: "result-credentials", MountPath: "/credentials/result", ReadOnly: true})
		}},
	} {
		name := "manager dispatches durable credentials with " + test.name
		refused = append(refused, name)
		c.row(policyenv.Row{Name: name, Deny: []string{guard}, Message: "rejected an unsafe workload shape", Do: as(env.Manager(), dryRunCreate(func() (client.Object, error) {
			job, err := resolveJobBuiltBy(builder)
			if err != nil {
				return nil, err
			}
			test.edit(job)
			return job, nil
		}))})
	}
	c.mutation(policyenv.Mutation{Name: "durable credential Job guard binding dropped", Policies: []string{guard}, Apply: policyenv.DropBinding(guard), Breaks: refused})
	c.mutation(policyenv.Mutation{Name: "durable credential Job guard refuses every shape", Policies: []string{guard}, Apply: policyenv.RefuseEverything(guard), Breaks: admitted})
	resultTokenJobRows(t, c, guard)
}

func resultTokenJobRows(t *testing.T, c *catalog, guard string) {
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
	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	builder := managerBuilder()
	builder.ResultEndpoint = "https://receiver.operator.svc"
	builder.ResultServerTrust = func() []byte { return trust }
	var admitted, refused []string
	for _, family := range []struct {
		name  string
		build func(workload.Builder) (*batchv1.Job, error)
	}{{"schema", resolveJobBuiltBy}, {"migration", migrationResolveJobBuiltBy}} {
		name := "manager dispatches a Pod-token " + family.name + " Job"
		admitted = append(admitted, name)
		c.row(policyenv.Row{Name: name, Do: as(env.Manager(), dryRunCreate(func() (client.Object, error) { return family.build(builder) }))})
	}
	for _, row := range []struct {
		name string
		edit func(*corev1.PodSpec)
	}{
		{"API audience", func(p *corev1.PodSpec) {
			p.Volumes[len(p.Volumes)-1].Projected.Sources[0].ServiceAccountToken.Audience = "https://kubernetes.default.svc"
		}},
		{"different expiration", func(p *corev1.PodSpec) {
			p.Volumes[len(p.Volumes)-1].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = ptr.To(int64(86400))
		}},
		{"world-readable token", func(p *corev1.PodSpec) { p.Volumes[len(p.Volumes)-1].Projected.DefaultMode = ptr.To(int32(0644)) }},
		{"another projection", func(p *corev1.PodSpec) {
			p.Volumes[len(p.Volumes)-1].Projected.Sources = append(p.Volumes[len(p.Volumes)-1].Projected.Sources, corev1.VolumeProjection{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}}})
		}},
		{"init access", func(p *corev1.PodSpec) {
			p.InitContainers[0].VolumeMounts = append(p.InitContainers[0].VolumeMounts, corev1.VolumeMount{Name: "result-credentials", MountPath: "/credentials/result", ReadOnly: true})
		}},
	} {
		name := "manager dispatches a Pod token with " + row.name
		refused = append(refused, name)
		c.row(policyenv.Row{Name: name, Deny: []string{guard}, Message: "rejected an unsafe workload shape", Do: as(env.Manager(), dryRunCreate(func() (client.Object, error) {
			job, err := resolveJobBuiltBy(builder)
			if err != nil {
				return nil, err
			}
			row.edit(&job.Spec.Template.Spec)
			return job, nil
		}))})
	}
	c.mutation(policyenv.Mutation{Name: "Pod-token Job guard binding dropped", Policies: []string{guard}, Apply: policyenv.DropBinding(guard), Breaks: refused})
	c.mutation(policyenv.Mutation{Name: "Pod-token Job guard refuses every shape", Policies: []string{guard}, Apply: policyenv.RefuseEverything(guard), Breaks: admitted})
}
