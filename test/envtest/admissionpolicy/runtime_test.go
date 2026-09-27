package admissionpolicy_test

import (
	"context"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The pod-template-hash the Deployment controller would give the rendered
// templates. Its value is opaque to the policies; its shape is not.
const runtimeTemplateHash = "7d9c5b8f4"

// runtimeRows holds the four policies that keep the manager and the
// certificate rotator what the release rendered: the rollout guard over the
// runtime Deployments and the admission configuration, the runtime guard over
// the Deployments' exact shape, the parent guard over the ReplicaSets that
// carry their identities, and the Pod identity guard over the Pods that run
// as them. envtest runs no Deployment or ReplicaSet controller, so the rows
// send what those controllers would, as those controllers.
func runtimeRows(t *testing.T, c *catalog) {
	rollout := policy(t, "ptah-operator-rollout-guard-v1")
	shape := policy(t, "ptah-operator-runtime-guard-v1")
	parent := policy(t, "ptah-operator-runtime-parent-guard-")
	pods := policy(t, "ptah-operator-runtime-pod-identity-v1")
	names := env.Chart.Names
	hook := env.Hook()
	user := policyenv.User()
	deploymentController := policyenv.ServiceAccount("kube-system", "deployment-controller")
	replicaSetController := policyenv.ServiceAccount("kube-system", "replicaset-controller")
	manager := types.NamespacedName{Namespace: names.Namespace, Name: names.ManagerDeployment}
	rotator := types.NamespacedName{Namespace: names.Namespace, Name: names.CertificateDeployment}

	const (
		scaleRow           = "ordinary user scales the manager through its scale subresource"
		unrenderedRow      = "release hook creates a Deployment the chart does not render"
		restampRow         = "administrator stamps the admission configuration with an inactive release"
		reapplyWebhooksRow = "administrator reapplies the admission configuration unchanged"
		unrelatedRow       = "ordinary user creates a Deployment of their own in the release namespace"
		imageRow           = "ordinary user points the manager Deployment at another image"
		reapplyManagerRow  = "release hook reapplies the manager Deployment unchanged"
		reapplyRotatorRow  = "release hook reapplies the certificate rotator Deployment unchanged"
		forgedParentRow    = "ordinary user creates a ReplicaSet that runs as the manager"
		managerParentRow   = "Deployment controller creates the manager's ReplicaSet"
		tenantParentRow    = "ordinary user creates a ReplicaSet in a tenant namespace"
		forgedPodRow       = "ordinary user creates a Pod that runs as the manager"
		staleReleasePodRow = "ReplicaSet controller creates a manager Pod stamped with an inactive release"
		managerPodRow      = "ReplicaSet controller creates the manager's Pod"
		rotatorPodRow      = "ReplicaSet controller creates the certificate rotator's Pod"
		unrelatedPodRow    = "ordinary user creates a Pod of their own in the release namespace"
	)

	// The rollout guard: no scale subresource, no Deployment the release does
	// not render, and nothing stamped with a release that is not the active one.
	c.row(policyenv.Row{
		Name: scaleRow, Deny: []string{rollout}, Message: "rejected an unsafe release transition",
		Do: as(user, func(ctx context.Context, api client.Client) error {
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: manager.Namespace, Name: manager.Name}}
			scale := &autoscalingv1.Scale{
				ObjectMeta: metav1.ObjectMeta{Namespace: manager.Namespace, Name: manager.Name},
				Spec:       autoscalingv1.ScaleSpec{Replicas: 5},
			}
			return api.SubResource("scale").Update(ctx, deployment, client.WithSubResourceBody(scale), client.DryRunAll)
		}),
	})
	c.row(policyenv.Row{
		Name: unrenderedRow, Deny: []string{rollout}, Message: "rejected an arbitrary hook Deployment name",
		Do: as(hook, runtimeDryRunCreate(func(context.Context) (client.Object, error) {
			return runtimeUnrelatedDeployment(names.Namespace, "ptah-envtest-extra"), nil
		})),
	})
	c.row(policyenv.Row{
		Name: restampRow, Deny: []string{rollout}, Message: "rejected an unsafe release transition",
		Do: func(ctx context.Context, env *policyenv.Env) error {
			current, err := stored(ctx, runtimeWebhooks(names))
			if err != nil {
				return err
			}
			current.Annotations["operator.ptah.run/release-sequence"] = "2"
			return env.Admin.Update(ctx, current, client.DryRunAll)
		},
	})
	c.row(policyenv.Row{Name: reapplyWebhooksRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		current, err := stored(ctx, runtimeWebhooks(names))
		if err != nil {
			return err
		}
		return env.Admin.Update(ctx, current, client.DryRunAll)
	}})
	c.row(policyenv.Row{Name: unrelatedRow, Do: as(user, runtimeDryRunCreate(func(context.Context) (client.Object, error) {
		return runtimeUnrelatedDeployment(names.Namespace, "team-tools"), nil
	}))})

	// The runtime guard: the Deployments stay exactly what the release rendered.
	c.row(policyenv.Row{
		Name: imageRow, Deny: []string{shape}, Message: "rejected a non-candidate controller image",
		Do: as(user, runtimeUpdateDeployment(manager, func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.Containers[0].Image = "ghcr.io/stokaro/ptah-operator@" + digest("9")
		})),
	})
	c.row(policyenv.Row{Name: reapplyManagerRow, Do: as(hook, runtimeUpdateDeployment(manager, func(*appsv1.Deployment) {}))})
	c.row(policyenv.Row{Name: reapplyRotatorRow, Do: as(hook, runtimeUpdateDeployment(rotator, func(*appsv1.Deployment) {}))})

	// The parent guard: only the Deployment controller makes the ReplicaSets
	// that carry the runtime identities, and only the one each Deployment owns.
	c.row(policyenv.Row{
		Name: forgedParentRow, Deny: []string{parent}, Message: "rejected an unsafe ReplicaSet",
		Do: as(user, runtimeDryRunCreate(func(ctx context.Context) (client.Object, error) {
			return runtimeReplicaSet(ctx, manager)
		})),
	})
	c.row(policyenv.Row{Name: managerParentRow, Do: as(deploymentController, runtimeDryRunCreate(func(ctx context.Context) (client.Object, error) {
		return runtimeReplicaSet(ctx, manager)
	}))})
	c.row(policyenv.Row{Name: tenantParentRow, Do: as(user, runtimeDryRunCreate(func(context.Context) (client.Object, error) {
		deployment := runtimeUnrelatedDeployment(tenantNamespace, "orders-tools")
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "orders-tools-" + runtimeTemplateHash},
			Spec: appsv1.ReplicaSetSpec{
				Selector: deployment.Spec.Selector,
				Template: deployment.Spec.Template,
			},
		}, nil
	}))})

	// The Pod identity guard: only the ReplicaSet controller makes the Pods
	// that run as the runtime identities, exactly as their templates say, and
	// stamped with the release the activation parameter names.
	c.row(policyenv.Row{
		Name: forgedPodRow, Deny: []string{pods}, Message: "rejected an unsafe workload",
		Do: as(user, runtimeDryRunCreate(func(ctx context.Context) (client.Object, error) {
			return runtimePod(ctx, manager)
		})),
	})
	c.row(policyenv.Row{
		Name: staleReleasePodRow, Deny: []string{pods}, Message: "rejected an unsafe workload",
		Do: as(replicaSetController, runtimeDryRunCreate(func(ctx context.Context) (client.Object, error) {
			pod, err := runtimePod(ctx, manager)
			if err != nil {
				return nil, err
			}
			pod.Annotations["operator.ptah.run/release-sequence"] = "2"
			return pod, nil
		})),
	})
	c.row(policyenv.Row{Name: managerPodRow, Do: as(replicaSetController, runtimeDryRunCreate(func(ctx context.Context) (client.Object, error) {
		return runtimePod(ctx, manager)
	}))})
	c.row(policyenv.Row{Name: rotatorPodRow, Do: as(replicaSetController, runtimeDryRunCreate(func(ctx context.Context) (client.Object, error) {
		return runtimePod(ctx, rotator)
	}))})
	c.row(policyenv.Row{Name: unrelatedPodRow, Do: as(user, runtimeDryRunCreate(func(context.Context) (client.Object, error) {
		template := runtimeUnrelatedDeployment(names.Namespace, "team-tools").Spec.Template
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: names.Namespace, Name: "team-tools", Labels: template.Labels},
			Spec:       template.Spec,
		}, nil
	}))})

	c.mutation(policyenv.Mutation{
		Name: "rollout guard binding dropped", Policies: []string{rollout},
		Apply:  policyenv.DropBinding(rollout),
		Breaks: []string{scaleRow, unrenderedRow, restampRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "rollout guard parameter reference fails open", Policies: []string{rollout},
		Apply: policyenv.RedirectParameters(rollout), Breaks: []string{restampRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "rollout guard matches every Deployment", Policies: []string{rollout},
		Apply: policyenv.WidenMatch(rollout), Breaks: []string{unrelatedRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "rollout guard refuses what it matches", Policies: []string{rollout},
		Apply: policyenv.RefuseEverything(rollout), Breaks: []string{reapplyWebhooksRow, reapplyManagerRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime guard binding dropped", Policies: []string{shape},
		Apply: policyenv.DropBinding(shape), Breaks: []string{imageRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime guard parameter reference fails open", Policies: []string{shape},
		Apply: policyenv.RedirectParameters(shape), Breaks: []string{imageRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime guard refuses what it matches", Policies: []string{shape},
		Apply: policyenv.RefuseEverything(shape), Breaks: []string{reapplyManagerRow, reapplyRotatorRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime parent guard binding dropped", Policies: []string{parent},
		Apply: policyenv.DropBinding(parent), Breaks: []string{forgedParentRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime parent guard matches every ReplicaSet", Policies: []string{parent},
		Apply: policyenv.WidenMatch(parent), Breaks: []string{tenantParentRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime parent guard refuses what it matches", Policies: []string{parent},
		Apply: policyenv.RefuseEverything(parent), Breaks: []string{managerParentRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime Pod identity guard binding dropped", Policies: []string{pods},
		Apply:  policyenv.DropBinding(pods),
		Breaks: []string{forgedPodRow, staleReleasePodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime Pod identity guard parameter reference fails open", Policies: []string{pods},
		Apply: policyenv.RedirectParameters(pods), Breaks: []string{staleReleasePodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime Pod identity guard matches every Pod in the namespace", Policies: []string{pods},
		Apply: policyenv.WidenMatch(pods), Breaks: []string{unrelatedPodRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "runtime Pod identity guard refuses what it matches", Policies: []string{pods},
		Apply: policyenv.RefuseEverything(pods), Breaks: []string{managerPodRow, rotatorPodRow},
	})
}

// runtimeDryRunCreate is dryRunCreate for builders that read what the fixture
// stored.
func runtimeDryRunCreate(build func(context.Context) (client.Object, error)) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		object, err := build(ctx)
		if err != nil {
			return err
		}
		return api.Create(ctx, object, client.DryRunAll)
	}
}

// runtimeUpdateDeployment updates the stored Deployment as edit changes it, as
// a dry run. An unchanged update still passes through admission, which is how
// a reapply is judged.
func runtimeUpdateDeployment(key types.NamespacedName, edit func(*appsv1.Deployment)) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		current, err := stored(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}})
		if err != nil {
			return err
		}
		edit(current)
		return api.Update(ctx, current, client.DryRunAll)
	}
}

func runtimeWebhooks(names policyenv.Names) *admissionregistrationv1.MutatingWebhookConfiguration {
	return &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: names.WebhookConfiguration}}
}

// runtimeReplicaSet is the ReplicaSet the Deployment controller makes from
// the stored Deployment: its template with the hash label added, owned by the
// Deployment.
func runtimeReplicaSet(ctx context.Context, key types.NamespacedName) (*appsv1.ReplicaSet, error) {
	deployment, err := stored(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}})
	if err != nil {
		return nil, err
	}
	template := *deployment.Spec.Template.DeepCopy()
	template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = runtimeTemplateHash
	selector := deployment.Spec.Selector.DeepCopy()
	selector.MatchLabels[appsv1.DefaultDeploymentUniqueLabelKey] = runtimeTemplateHash
	controller, block := true, true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   deployment.Namespace,
			Name:        deployment.Name + "-" + runtimeTemplateHash,
			Labels:      template.Labels,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID,
				Controller: &controller, BlockOwnerDeletion: &block,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Replicas: deployment.Spec.Replicas, Selector: selector, Template: template},
	}, nil
}

// runtimePod is the Pod the ReplicaSet controller makes from that ReplicaSet.
// The name is left to the API server, as the controller leaves it.
func runtimePod(ctx context.Context, key types.NamespacedName) (*corev1.Pod, error) {
	replicaSet, err := runtimeReplicaSet(ctx, key)
	if err != nil {
		return nil, err
	}
	template := replicaSet.Spec.Template
	controller, block := true, true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    replicaSet.Namespace,
			GenerateName: replicaSet.Name + "-",
			Labels:       template.Labels,
			Annotations:  template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name,
				UID:        "5b0e9c7a-0000-4000-8000-00000000000a",
				Controller: &controller, BlockOwnerDeletion: &block,
			}},
		},
		Spec: template.Spec,
	}, nil
}

// runtimeUnrelatedDeployment is a workload with no Ptah identity at all.
func runtimeUnrelatedDeployment(namespace, name string) *appsv1.Deployment {
	labels := map[string]string{"app.kubernetes.io/name": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "tools", Image: "registry.example/tools@" + digest("7"),
				}}},
			},
		},
	}
}
