package apipace_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/stokaro/ptah-operator/internal/apipace"
)

// controller-runtime turns client-go's default limit off by setting a negative
// QPS. The bucket has to win over that, and reach every client built from the
// config rather than each client building its own.
func TestEveryClientSharesTheOneBucket(t *testing.T) {
	config := apipace.Config(&rest.Config{Host: "https://api.invalid", QPS: -1})
	limiter, ok := config.RateLimiter.(flowcontrol.RateLimiter)
	if !ok || limiter.QPS() != apipace.RequestsPerSecond {
		t.Fatalf("the config carries no %d-per-second bucket: %#v", apipace.RequestsPerSecond, config.RateLimiter)
	}
	if rest.CopyConfig(config).RateLimiter != limiter {
		t.Fatal("a copied config built its own bucket")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	typed := clientset.CoreV1().RESTClient().GetRateLimiter()
	other := clientset.BatchV1().RESTClient().GetRateLimiter()

	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	gvk := clientset.CoreV1().RESTClient().APIVersion().WithKind("Pod")
	perType, err := apiutil.RESTClientForGVK(gvk, false, false, config, serializer.NewCodecFactory(scheme.Scheme), httpClient)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]flowcontrol.RateLimiter{"core": typed, "batch": other, "controller-runtime": perType.GetRateLimiter()} {
		if got != limiter {
			t.Errorf("the %s client does not use the shared bucket", name)
		}
	}
}

func TestTheBucketBoundsAMinuteToTheLeadersShare(t *testing.T) {
	// The leader's worst minute: the sustained rate plus one full burst. The
	// follower's share, measured at about a third of the leader's, has to fit
	// beside it inside the profile's 7,000.
	leader := apipace.RequestsPerSecond*60 + apipace.Burst
	if leader+leader/3 > 7000 {
		t.Fatalf("a %d-request leader minute leaves the follower no room under 7,000", leader)
	}
	unpaced := 5499 // the leader in the audited 0.2.3 soak's burst minute
	if leader >= unpaced {
		t.Fatalf("a %d-request leader minute would not have bounded the measured burst of %d", leader, unpaced)
	}
}
