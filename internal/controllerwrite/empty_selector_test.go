package controllerwrite_test

import (
	"context"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestValidationHandlerAllowsAnExplicitEmptyNodeSelector(t *testing.T) {
	t.Parallel()

	migration, job := migrationJobFixtureWith(t, operatorv1alpha1.MigrationOperationResolve,
		func(migration *operatorv1alpha1.PtahMigration) {
			migration.Spec.Execution.NodeSelector = map[string]string{}
		})
	// requestFor uses the same JSON encoding as a submitted Job. It omits an
	// empty selector, while the resource's accepted configuration retains it.
	reader := fake.NewClientBuilder().WithScheme(controllerWriteScheme(t)).
		WithObjects(migration, migrationServiceAccount()).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, out client.Object, options ...client.GetOption) error {
			if err := c.Get(ctx, key, out, options...); err != nil {
				return err
			}
			if resource, ok := out.(*operatorv1alpha1.PtahMigration); ok {
				// A native API GET preserves the explicit empty object. The
				// typed fake's JSON round trip would erase this regression input.
				resource.Spec.Execution.NodeSelector = map[string]string{}
			}
			return nil
		}}).Build()
	response := handlerWithReader(migrationJobBuilder(), reader).Handle(context.Background(), requestFor(t, admissionv1.Create, job))
	if !response.Allowed {
		t.Fatalf("an accepted empty selector prevented Resolve: %s", responseMessage(response))
	}
}
