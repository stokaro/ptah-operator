package resultstore_test

import (
	"errors"
	"testing"

	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This proves TokenRequest/TokenReview against a real API server, including an
// API-audience positive control. There is no kubelet here: mounted-token refresh
// and installed result delivery still require their existing native workflows.
func TestPodBoundReceiverTokenCannotAuthenticateToAPI(t *testing.T) {
	b := newBinding(t)
	ctx := t.Context()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: "runner"}, AutomountServiceAccountToken: ptr.To(false)}
	if err := api.Create(ctx, sa); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: b.PodName},
		Spec: corev1.PodSpec{ServiceAccountName: sa.Name, AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{{Name: "runner", Image: "fixture.invalid/runner"}}}}
	if err := api.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	b.PodUID = pod.UID
	identity := resultdelivery.Identity{Binding: b, Engine: "postgresql"}
	admin, err := kubernetes.NewForConfig(plane.Config)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(audience []string, bound *authenticationv1.BoundObjectReference) string {
		t.Helper()
		result, err := admin.CoreV1().ServiceAccounts(b.Namespace).CreateToken(ctx, sa.Name, &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{Audiences: audience, ExpirationSeconds: ptr.To(int64(600)), BoundObjectRef: bound},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal("TokenRequest failed")
		}
		return result.Status.Token
	}
	bound := &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
	token := issue([]string{resultdelivery.TokenAudience}, bound)

	// Only the receiver may create TokenReviews. The test identity has no
	// Secret access; Pod GET is the verifier's one live-object dependency.
	username := "result-token-reviewer-" + b.Namespace
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: username}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
	}}
	grant := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: username}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: username}}}
	podRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: username}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
	}}
	podGrant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: username}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: podRole.Name}, Subjects: []rbacv1.Subject{
		{Kind: "User", APIGroup: rbacv1.GroupName, Name: username},
		// The API-audience control deliberately grants this disposable SA a
		// Pod read, so a 401 cannot be confused with missing RBAC permission.
		{Kind: "ServiceAccount", Namespace: b.Namespace, Name: sa.Name},
	}}
	for _, obj := range []client.Object{role, grant, podRole, podGrant} {
		if err := api.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	reviewer, err := kubernetes.NewForConfig(plane.Impersonate(username))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := client.New(plane.Impersonate(username), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	verifier := resultauthority.TokenVerifier{Reviews: reviewer.AuthenticationV1().TokenReviews(), Reader: reader}
	if err := verifier.Verify(ctx, token, identity); err != nil {
		t.Fatalf("receiver-scoped Pod token refused: %v", err)
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: "any-secret"}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Fatalf("receiver unexpectedly has Secret read permission: %v", err)
	}
	readPod := func(credential string) error {
		t.Helper()
		config := &rest.Config{Host: plane.Config.Host, TLSClientConfig: rest.TLSClientConfig{CAData: plane.Config.CAData, CAFile: plane.Config.CAFile}, BearerToken: credential}
		caller, err := kubernetes.NewForConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		_, err = caller.CoreV1().Pods(b.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		return err
	}
	apiToken := issue(nil, bound) // API server's default audience, not the receiver.
	if err := readPod(apiToken); err != nil {
		t.Fatal("API-audience positive control could not read its permitted Pod")
	}
	if err := readPod(token); !apierrors.IsUnauthorized(err) {
		t.Fatalf("receiver audience authenticated to the Kubernetes API: unauthorized=%v", apierrors.IsUnauthorized(err))
	}
	if err := verifier.Verify(ctx, apiToken, identity); err == nil {
		t.Fatal("API-audience token authenticated to the receiver")
	}
	unbound := issue([]string{resultdelivery.TokenAudience}, nil)
	if err := verifier.Verify(ctx, unbound, identity); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatalf("unbound ServiceAccount token authenticated an operation Pod: %v", err)
	}
	// Issuance and refresh do not create per-token Secret objects.
	for range 4 {
		if err := verifier.Verify(ctx, issue([]string{resultdelivery.TokenAudience}, bound), identity); err != nil {
			t.Fatal(err)
		}
	}
	secrets := &corev1.SecretList{}
	if err := api.List(ctx, secrets, client.InNamespace(b.Namespace)); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("TokenRequest created Secrets: count=%d err=%v", len(secrets.Items), err)
	}
	uid := pod.UID
	if err := api.Delete(ctx, pod, client.GracePeriodSeconds(0), client.Preconditions{UID: &uid}); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("original Pod still exists")
	}
	if err := verifier.Verify(ctx, token, identity); err == nil {
		t.Fatal("deleted Pod token remained valid")
	}
	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: pod.Name}, Spec: pod.Spec}
	if err := api.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.UID == uid {
		t.Fatal("test did not replace the Pod")
	}
	if err := verifier.Verify(ctx, token, identity); err == nil {
		t.Fatal("replacement Pod revived the predecessor's token")
	}
	bound.UID = replacement.UID
	if err := verifier.Verify(ctx, issue([]string{resultdelivery.TokenAudience}, bound), identity); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatalf("replacement Pod token claimed the predecessor's identity: %v", err)
	}
}
