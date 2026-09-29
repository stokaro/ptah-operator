//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planview"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// pendingSQLReadAuthorization checks the pending plan through the example
// Roles and returns the same check for that plan after Apply. The lifecycle
// calls it before moving the source tag: resolving another artifact clears
// status.applied and retires the previous plan's projection.
func (d *dataPlane) pendingSQLReadAuthorization(schema string) func() {
	diagnostic := d.installReaderExample("diagnostic-reader-role.yaml", "diagnostic", "<diagnostic-reader-group>")
	reviewer := d.installReaderExample("approver-plan-reader-role.yaml", "reviewer", "<plan-reviewer-group>")
	verify := func(selection planview.Selection) types.UID {
		expected, err := planview.Load(d.ctx, d.cluster.Client, d.in.TestNamespace, schema, selection)
		d.check(err, "read the %s plan of %s as the administrator", selection, schema)
		if expected.PlanUID == "" || len(expected.Document) == 0 || expected.StatementCount == 0 {
			d.fatalf("%s has no nonempty %s plan to test SQL access", schema, selection)
		}
		plan := d.schemaPlan(expected.PlanName)
		metadata := &ptahv1alpha1.PtahSchemaPlan{}
		d.check(diagnostic.Get(d.ctx, client.ObjectKeyFromObject(plan), metadata), "the diagnostic reader must read the plan manifest")
		if metadata.UID != expected.PlanUID {
			d.fatalf("the diagnostic reader read another plan manifest")
		}
		refused, err := planview.Load(d.ctx, diagnostic, d.in.TestNamespace, schema, selection)
		if !apierrors.IsForbidden(err) || len(refused.Document) != 0 {
			d.fatalf("the diagnostic reader did not receive Forbidden with no SQL for %s's %s plan: %v", schema, selection, err)
		}
		read, err := planview.Load(d.ctx, reviewer, d.in.TestNamespace, schema, selection)
		d.check(err, "the plan reviewer must reconstruct %s's %s plan", schema, selection)
		if read.PlanUID != expected.PlanUID || read.ContentDigest != expected.ContentDigest || !bytes.Equal(read.Document, expected.Document) {
			d.fatalf("the plan reviewer did not reconstruct the exact %s plan of %s", selection, schema)
		}
		for _, ref := range plan.Spec.Chunks {
			err := diagnostic.Get(d.ctx, types.NamespacedName{Namespace: plan.Namespace, Name: ref.Name}, &ptahv1alpha1.PtahSchemaPlanChunk{})
			d.requireForbiddenRead(err, "a plan chunk as the diagnostic reader")
		}
		if selection == planview.Current {
			d.assertPlanNotProjected(plan.Name)
		} else {
			d.assertPlanProjected(plan)
			for _, ref := range plan.Spec.Chunks {
				for _, reader := range []client.Client{diagnostic, reviewer} {
					err := reader.Get(d.ctx, types.NamespacedName{Namespace: plan.Namespace, Name: ref.Name}, &corev1.ConfigMap{})
					d.requireForbiddenRead(err, "an applied plan's ConfigMap through either example Role")
				}
			}
		}
		d.logf("PASS %s %s plan %s: diagnostic metadata allowed, SQL refused; reviewer read %d statements from %d chunks", schema, selection, plan.UID, read.StatementCount, len(plan.Spec.Chunks))
		return expected.PlanUID
	}
	pendingUID := verify(planview.Current)
	d.requireForbiddenRead(diagnostic.List(d.ctx, &ptahv1alpha1.PtahSchemaPlanChunkList{}, client.InNamespace(d.in.TestNamespace)), "listing SQL chunks as the diagnostic reader")
	for _, reader := range []client.Client{diagnostic, reviewer} {
		for _, name := range []string{pgSecret, mysqlSecret} {
			d.requireForbiddenRead(reader.Get(d.ctx, types.NamespacedName{Namespace: d.in.TestNamespace, Name: name}, &corev1.Secret{}), "a database credential through either example Role")
		}
	}

	return func() {
		if verify(planview.Applied) != pendingUID {
			d.fatalf("the applied SQL authorization check read a different plan from the pending check")
		}
	}
}

func (d *dataPlane) requireForbiddenRead(err error, what string) {
	d.t.Helper()
	if !apierrors.IsForbidden(err) {
		d.fatalf("%s did not return Forbidden: %v", what, err)
	}
}

// Only the namespace and placeholder group change. In particular, the rules
// and roleRef remain the example's, so a documentation regression changes the
// authority these requests actually receive.
func (d *dataPlane) installReaderExample(file, identity, placeholder string) client.Client {
	d.t.Helper()
	content, err := os.ReadFile(filepath.Join(repositoryRoot, "examples", file))
	d.check(err, "read %s", file)
	documents, err := decodeManifests(content)
	d.check(err, "decode %s", file)
	if len(documents) != 2 {
		d.fatalf("%s must contain one Role and one RoleBinding", file)
	}
	var role *rbacv1.Role
	var binding *rbacv1.RoleBinding
	for _, document := range documents {
		switch document["kind"] {
		case "Role":
			if role != nil {
				d.fatalf("%s contains two Roles", file)
			}
			role = &rbacv1.Role{}
			d.check(runtime.DefaultUnstructuredConverter.FromUnstructured(document, role), "decode the example Role")
		case "RoleBinding":
			if binding != nil {
				d.fatalf("%s contains two RoleBindings", file)
			}
			binding = &rbacv1.RoleBinding{}
			d.check(runtime.DefaultUnstructuredConverter.FromUnstructured(document, binding), "decode the example RoleBinding")
		default:
			d.fatalf("%s contains an unexpected kind", file)
		}
	}
	if role == nil || binding == nil || len(role.Rules) == 0 || binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name ||
		len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "Group" || binding.Subjects[0].Name != placeholder {
		d.fatalf("%s is not a nonempty Role bound to its documented placeholder group", file)
	}
	role.Namespace, binding.Namespace = d.in.TestNamespace, d.in.TestNamespace
	group := "e2e:sql-" + identity
	binding.Subjects[0].Name = group
	for _, object := range []client.Object{role, binding} {
		d.check(d.cluster.Client.Create(d.ctx, object), "install %s", file)
		t := d.t
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := d.cluster.Client.Delete(ctx, object, client.Preconditions{UID: ptr.To(object.GetUID())}); client.IgnoreNotFound(err) != nil {
				t.Errorf("remove the SQL reader example: %v", err)
			}
		})
	}
	reader, err := d.cluster.As(rest.ImpersonationConfig{UserName: "e2e-sql-" + identity, Groups: []string{"system:authenticated", group}})
	d.check(err, "construct the SQL reader identity")
	d.check(harness.Wait(d.ctx, "the example RoleBinding to grant schema reads", time.Minute, time.Second,
		func(ctx context.Context) (bool, string, error) {
			err := reader.Get(ctx, types.NamespacedName{Namespace: d.in.TestNamespace, Name: "e2e-postgresql"}, &ptahv1alpha1.PtahSchema{})
			if apierrors.IsForbidden(err) {
				return false, "waiting for the RBAC authorizer to observe the RoleBinding", nil
			}
			return err == nil, "reading the existing schema through the example Role", err
		}), "wait for %s to take effect", file)
	return reader
}
