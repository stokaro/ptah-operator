package admissionpolicy_test

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The Job controller names a Pod after its Job plus five generated characters,
// and labels it with the Job's name and UID. The hook policies hold a hook Pod
// to exactly that origin.
const (
	hookPodSuffix = "h4k9z"
	hookJobUID    = types.UID("5b0c7a1e-0000-4000-8000-0000000000a1")
)

// hookRows holds the policies around the CRD manager hook. Its Jobs run with
// the release's most privileged ServiceAccount, so the chart pins who may
// create them (an administrator of admission policy), their exact shape, who
// may create their Pods (the Job controller) and those Pods' exact shape, and
// it keeps one ConfigMap update that must always be refused so the hook can
// prove to itself that the guards are enforced before it trusts them.
func hookRows(t *testing.T, c *catalog) {
	identity := policy(t, "ptah-operator-hook-identity-")
	probeGuard := policy(t, "ptah-operator-hook-probe-guard-")
	parentOrigin := policy(t, "ptah-operator-hook-parent-origin-guard-")
	podOrigin := policy(t, "ptah-operator-hook-pod-origin-guard-")
	contract := policy(t, "ptah-operator-hook-parent-contract-")
	names := env.Chart.Names
	rendered := hookReconcileJob(t)

	const (
		adminJobRow        = "administrator creates the chart's exact reconcile hook Job"
		userJobRow         = "ordinary user creates the chart's exact reconcile hook Job"
		foreignImageJobRow = "administrator creates the reconcile hook Job with another image"
		tenantJobRow       = "ordinary user creates an unrelated Job in a tenant namespace"
		controllerPodRow   = "job controller creates the exact Pod of the reconcile hook Job"
		foreignImagePodRow = "job controller creates the reconcile hook Pod with another image"
		otherSequencePod   = "ordinary user creates a Pod as another release sequence's hook ServiceAccount"
		tenantPodRow       = "ordinary user creates an unrelated Pod in a tenant namespace"
		probeRow           = "hook updates its enforcement probe ConfigMap"
		adminProbeRow      = "administrator updates the hook's enforcement probe ConfigMap"
	)

	// The Jobs, as Helm would create them for an administrator.
	c.row(policyenv.Row{Name: adminJobRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		return env.Admin.Create(ctx, rendered.DeepCopy(), client.DryRunAll)
	}})
	c.row(policyenv.Row{
		Name: userJobRow, Deny: []string{parentOrigin}, Message: "rejected an unauthorized Job",
		Do: as(policyenv.User(), func(ctx context.Context, api client.Client) error {
			return api.Create(ctx, rendered.DeepCopy(), client.DryRunAll)
		}),
	})
	c.row(policyenv.Row{
		Name: foreignImageJobRow, Deny: []string{contract}, Message: "rejected an unsafe Job",
		Do: func(ctx context.Context, env *policyenv.Env) error {
			job := rendered.DeepCopy()
			job.Spec.Template.Spec.Containers[0].Image = "ghcr.io/stokaro/ptah-operator@" + digest("9")
			return env.Admin.Create(ctx, job, client.DryRunAll)
		},
	})
	c.row(policyenv.Row{Name: tenantJobRow, Do: as(policyenv.User(), func(ctx context.Context, api client.Client) error {
		job := rendered.DeepCopy()
		job.ObjectMeta = metav1.ObjectMeta{Namespace: tenantNamespace, Name: "orders-report"}
		job.Spec.Template.ObjectMeta = metav1.ObjectMeta{}
		job.Spec.Template.Spec.ServiceAccountName = "orders-report"
		return api.Create(ctx, job, client.DryRunAll)
	})})

	// Their Pods, as the Job controller would create them.
	c.row(policyenv.Row{Name: controllerPodRow, Do: as(policyenv.JobController(), func(ctx context.Context, api client.Client) error {
		return api.Create(ctx, hookPod(rendered), client.DryRunAll)
	})})
	c.row(policyenv.Row{
		Name: foreignImagePodRow, Deny: []string{identity}, Message: "rejected an unsafe privileged hook Pod",
		Do: as(policyenv.JobController(), func(ctx context.Context, api client.Client) error {
			pod := hookPod(rendered)
			pod.Spec.Containers[0].Image = "ghcr.io/stokaro/ptah-operator@" + digest("9")
			return api.Create(ctx, pod, client.DryRunAll)
		}),
	})
	c.row(policyenv.Row{
		// The identity guard names this release's hook accounts exactly; the
		// origin guard matches any sequence's by pattern, so a Pod running as
		// the next release's hook account is the origin guard's alone to refuse.
		Name: otherSequencePod, Deny: []string{podOrigin}, Message: "rejected an unauthorized Pod",
		Do: as(policyenv.User(), func(ctx context.Context, api client.Client) error {
			pod := hookPod(rendered)
			pod.Spec.ServiceAccountName = "ptah-ptah-operator-crd-v2-aaaaaaaaaaaa"
			return api.Create(ctx, pod, client.DryRunAll)
		}),
	})
	c.row(policyenv.Row{Name: tenantPodRow, Do: as(policyenv.User(), func(ctx context.Context, api client.Client) error {
		pod := hookPod(rendered)
		pod.ObjectMeta = metav1.ObjectMeta{Namespace: tenantNamespace, Name: "orders-report"}
		pod.Spec.ServiceAccountName = "orders-report"
		return api.Create(ctx, pod, client.DryRunAll)
	})})

	// The enforcement probe: a write the hook makes knowing it must fail.
	probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: names.Namespace, Name: hookProbeName(t)}}
	c.row(policyenv.Row{
		Name: probeRow, Deny: []string{probeGuard}, Message: "rejected the enforcement probe",
		Do: as(env.Hook(), func(ctx context.Context, api client.Client) error {
			current, err := stored(ctx, probe)
			if err != nil {
				return err
			}
			return api.Update(ctx, current, client.DryRunAll)
		}),
	})
	c.row(policyenv.Row{Name: adminProbeRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		current, err := stored(ctx, probe)
		if err != nil {
			return err
		}
		return env.Admin.Update(ctx, current, client.DryRunAll)
	}})

	c.mutation(policyenv.Mutation{
		Name: "hook parent origin guard binding dropped", Policies: []string{parentOrigin},
		Apply: policyenv.DropBinding(parentOrigin), Breaks: []string{userJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook parent origin guard refuses what it matches", Policies: []string{parentOrigin},
		Apply: policyenv.RefuseEverything(parentOrigin), Breaks: []string{adminJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook parent origin guard matches every Job", Policies: []string{parentOrigin},
		Apply: policyenv.WidenMatch(parentOrigin), Breaks: []string{tenantJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook parent contract binding dropped", Policies: []string{contract},
		Apply: policyenv.DropBinding(contract), Breaks: []string{foreignImageJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook parent contract refuses what it matches", Policies: []string{contract},
		Apply: policyenv.RefuseEverything(contract), Breaks: []string{adminJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook parent contract matches every Job", Policies: []string{contract},
		Apply: policyenv.WidenMatch(contract), Breaks: []string{tenantJobRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook identity guard binding dropped", Policies: []string{identity},
		Apply: policyenv.DropBinding(identity), Breaks: []string{foreignImagePodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook identity guard refuses what it matches", Policies: []string{identity},
		Apply: policyenv.RefuseEverything(identity), Breaks: []string{controllerPodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook identity guard matches every Pod", Policies: []string{identity},
		Apply: policyenv.WidenMatch(identity), Breaks: []string{tenantPodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook Pod origin guard binding dropped", Policies: []string{podOrigin},
		Apply: policyenv.DropBinding(podOrigin), Breaks: []string{otherSequencePod},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook Pod origin guard refuses what it matches", Policies: []string{podOrigin},
		Apply: policyenv.RefuseEverything(podOrigin), Breaks: []string{controllerPodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook Pod origin guard matches every Pod", Policies: []string{podOrigin},
		Apply: policyenv.WidenMatch(podOrigin), Breaks: []string{tenantPodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook probe guard binding dropped", Policies: []string{probeGuard},
		Apply: policyenv.DropBinding(probeGuard), Breaks: []string{probeRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "hook probe guard matches every identity", Policies: []string{probeGuard},
		Apply: policyenv.WidenMatch(probeGuard), Breaks: []string{adminProbeRow},
	})
}

// hookReconcileJob is the rendered reconcile hook Job. It carries the hook
// ServiceAccount's name, which is how the chart names it.
func hookReconcileJob(t *testing.T) *batchv1.Job {
	t.Helper()
	object, err := env.Chart.Object("Job", env.Chart.Names.Hook)
	if err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, job); err != nil {
		t.Fatalf("decode the rendered Job %s: %v", object.GetName(), err)
	}
	if job.Spec.Template.Spec.ServiceAccountName != env.Chart.Names.Hook {
		t.Fatalf("the rendered Job %s runs as %q, want the hook ServiceAccount %s",
			job.Name, job.Spec.Template.Spec.ServiceAccountName, env.Chart.Names.Hook)
	}
	return job
}

// hookPod is the Pod the Job controller creates for job: the template, owned
// by the Job, named and labeled as the controller names and labels it.
func hookPod(job *batchv1.Job) *corev1.Pod {
	controller, block := true, true
	labels := map[string]string{}
	for key, value := range job.Spec.Template.Labels {
		labels[key] = value
	}
	for _, key := range []string{"batch.kubernetes.io/job-name", "job-name"} {
		labels[key] = job.Name
	}
	for _, key := range []string{"batch.kubernetes.io/controller-uid", "controller-uid"} {
		labels[key] = string(hookJobUID)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    job.Namespace,
			GenerateName: job.Name + "-",
			Name:         job.Name + "-" + hookPodSuffix,
			Labels:       labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: hookJobUID,
				Controller: &controller, BlockOwnerDeletion: &block,
			}},
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
}

// hookProbeName is the ConfigMap the probe guard refuses the hook's update of.
func hookProbeName(t *testing.T) string {
	t.Helper()
	for _, object := range env.Chart.Installed {
		if object.GetKind() == "ConfigMap" && object.GetAnnotations()["operator.ptah.run/hook-identity-policy"] != "" {
			return object.GetName()
		}
	}
	t.Fatal("the chart installs no hook enforcement probe ConfigMap")
	return ""
}
