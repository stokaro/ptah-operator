package admissionpolicy_test

import (
	"testing"

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
}
