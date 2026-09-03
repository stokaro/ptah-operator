package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func TestDirectAuthorizationReviewClientCreatesSubjectAndSelfReviews(t *testing.T) {
	calls := make(map[string]int)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls[request.URL.Path]++
		body := ""
		switch request.URL.Path {
		case "/apis/authorization.k8s.io/v1/subjectaccessreviews":
			body = `{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{"denied":true}}`
		case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			body = `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"denied":true}}`
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: request}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})

	client, err := newDirectAuthorizationReviewClient(&rest.Config{
		Host:      "https://api.example.test",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("newDirectAuthorizationReviewClient() error = %v", err)
	}
	if _, err := client.CreateSubjectAccessReview(
		context.Background(),
		&authorizationv1.SubjectAccessReview{},
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("CreateSubjectAccessReview() error = %v", err)
	}
	if _, err := client.CreateSelfSubjectAccessReview(
		context.Background(),
		&authorizationv1.SelfSubjectAccessReview{},
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("CreateSelfSubjectAccessReview() error = %v", err)
	}
	if want := map[string]int{
		"/apis/authorization.k8s.io/v1/subjectaccessreviews":     1,
		"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews": 1,
	}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("authorization API calls = %#v, want %#v", calls, want)
	}
}

func TestNewTeardownRBACConvergenceBarrierDiscoversEveryDirectEndpoint(t *testing.T) {
	ready := true
	terminating := true
	first := validAPIEndpointSlice("kubernetes-a", discoveryv1.AddressTypeIPv4, 6443,
		discoveryv1.Endpoint{Addresses: []string{"10.0.0.12"}},
		discoveryv1.Endpoint{Conditions: discoveryv1.EndpointConditions{Ready: &ready, Terminating: &terminating}},
	)
	second := validAPIEndpointSlice("kubernetes-b", discoveryv1.AddressTypeIPv6, 7443,
		discoveryv1.Endpoint{Addresses: []string{"2001:db8:0:0::20"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
	)
	lister := &scriptedEndpointSliceLister{pages: map[string]*discoveryv1.EndpointSliceList{
		"": {
			ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "second-page"},
			Items:    []discoveryv1.EndpointSlice{first},
		},
		"second-page": {
			ListMeta: metav1.ListMeta{ResourceVersion: "42"},
			Items:    []discoveryv1.EndpointSlice{second},
		},
	}}
	base := &rest.Config{
		Host:        "https://kubernetes.default.svc",
		APIPath:     "/api",
		BearerToken: "projected-token",
		TLSClientConfig: rest.TLSClientConfig{
			CAData:     []byte("cluster-ca"),
			ServerName: "old-server-name",
		},
	}
	var configs []*rest.Config
	factory := func(config *rest.Config) (crdupgrade.AuthorizationReviewClient, error) {
		configs = append(configs, config)
		return &deniedSubjectAccessReviewClient{}, nil
	}

	barrier, err := newTeardownRBACConvergenceBarrierWith(
		context.Background(),
		base,
		lister,
		validRBACRolloutGuard(),
		validRBACAdmissionContract(),
		factory,
	)
	if err != nil {
		t.Fatalf("newTeardownRBACConvergenceBarrierWith() error = %v", err)
	}
	if err := barrier.Validate(); err != nil {
		t.Fatalf("barrier.Validate() error = %v", err)
	}
	if got, want := endpointNames(barrier), []string{"10.0.0.12:6443", "[2001:db8::20]:7443"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoint names = %#v, want %#v", got, want)
	}
	if len(configs) != 2 {
		t.Fatalf("client factory calls = %d, want 2", len(configs))
	}
	if got := authorizationSweepSize(barrier); got != 50 {
		t.Fatalf("authorization sweep size = %d, want 50 exact retired-subject plus current-credential probes", got)
	}
	for index, config := range configs {
		if config == base {
			t.Fatalf("client config %d aliases source config", index)
		}
		if config.Host != "https://"+barrier.Endpoints[index].Name {
			t.Errorf("client config %d Host = %q", index, config.Host)
		}
		if config.ServerName != kubernetesServiceTLSServerName {
			t.Errorf("client config %d TLS ServerName = %q", index, config.ServerName)
		}
		if config.BearerToken != base.BearerToken || string(config.CAData) != string(base.CAData) || config.APIPath != base.APIPath {
			t.Errorf("client config %d did not preserve credentials, CA, and API path", index)
		}
		if config.RateLimiter != nil || config.QPS != teardownRBACQueriesPerSecond || config.Burst != authorizationSweepSize(barrier) {
			t.Errorf(
				"client config %d limiter = (%v, %v, %d), want (nil, %v, %d)",
				index,
				config.RateLimiter,
				config.QPS,
				config.Burst,
				teardownRBACQueriesPerSecond,
				authorizationSweepSize(barrier),
			)
		}
		if config.Proxy == nil {
			t.Fatalf("client config %d has no explicit direct proxy policy", index)
		}
		proxy, proxyErr := config.Proxy(&http.Request{})
		if proxyErr != nil || proxy != nil {
			t.Errorf("client config %d proxy = %v, %v; want nil, nil", index, proxy, proxyErr)
		}
	}
	if base.Host != "https://kubernetes.default.svc" || base.ServerName != "old-server-name" || base.Proxy != nil || base.QPS != 0 || base.Burst != 0 {
		t.Fatalf("source REST config was mutated: %#v", base)
	}
	wantOptions := []metav1.ListOptions{
		{LabelSelector: discoveryv1.LabelServiceName + "=kubernetes", Limit: teardownEndpointSlicePageSize},
		{LabelSelector: discoveryv1.LabelServiceName + "=kubernetes", Limit: teardownEndpointSlicePageSize, Continue: "second-page"},
	}
	if !reflect.DeepEqual(lister.options, wantOptions) {
		t.Fatalf("EndpointSlice list options = %#v, want %#v", lister.options, wantOptions)
	}
}

func TestTeardownAuthorizationEndpointProviderRefreshesAndCachesByAddress(t *testing.T) {
	lister := &scriptedEndpointSliceLister{pages: oneEndpointSlicePage(
		validAPIEndpointSlice(
			"kubernetes-a",
			discoveryv1.AddressTypeIPv4,
			6443,
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.12"}},
		),
	)}
	created := make(map[string]int)
	provider := newTeardownAuthorizationEndpointProvider(
		validRBACRESTConfig(),
		lister,
		48,
		func(config *rest.Config) (crdupgrade.AuthorizationReviewClient, error) {
			created[config.Host]++
			return &deniedSubjectAccessReviewClient{}, nil
		},
	)

	first, err := provider(context.Background())
	if err != nil {
		t.Fatalf("initial endpoint refresh error = %v", err)
	}
	if got, want := namedAuthorizationEndpointNames(first), []string{"10.0.0.12:6443"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial endpoints = %#v, want %#v", got, want)
	}
	lister.pages = oneEndpointSlicePage(validAPIEndpointSlice(
		"kubernetes-expanded",
		discoveryv1.AddressTypeIPv4,
		6443,
		discoveryv1.Endpoint{Addresses: []string{"10.0.0.13"}},
		discoveryv1.Endpoint{Addresses: []string{"10.0.0.12"}},
	))
	second, err := provider(context.Background())
	if err != nil {
		t.Fatalf("expanded endpoint refresh error = %v", err)
	}
	if got, want := namedAuthorizationEndpointNames(second), []string{"10.0.0.12:6443", "10.0.0.13:6443"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded endpoints = %#v, want %#v", got, want)
	}
	lister.pages = oneEndpointSlicePage(validAPIEndpointSlice(
		"kubernetes-b",
		discoveryv1.AddressTypeIPv4,
		6443,
		discoveryv1.Endpoint{Addresses: []string{"10.0.0.13"}},
	))
	third, err := provider(context.Background())
	if err != nil {
		t.Fatalf("contracted endpoint refresh error = %v", err)
	}
	if got, want := namedAuthorizationEndpointNames(third), []string{"10.0.0.13:6443"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("contracted endpoints = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(created, map[string]int{
		"https://10.0.0.12:6443": 1,
		"https://10.0.0.13:6443": 1,
	}) {
		t.Fatalf("client creations = %#v, want one per canonical address", created)
	}
}

func TestDiscoverKubernetesAPIServerAddressesRejectsIncompleteOrMalformedTopology(t *testing.T) {
	notReady := false
	notServing := false
	udp := corev1.ProtocolUDP
	https := kubernetesServiceTLSPortName
	port := int32(6443)

	tests := []struct {
		name  string
		pages map[string]*discoveryv1.EndpointSliceList
		want  string
	}{
		{name: "no slices", pages: map[string]*discoveryv1.EndpointSliceList{"": {ListMeta: metav1.ListMeta{ResourceVersion: "42"}}}, want: "no EndpointSlices"},
		{
			name: "no ready endpoint",
			pages: oneEndpointSlicePage(validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443,
				discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}},
			)),
			want: "non-terminating but not ready",
		},
		{
			name: "non-serving endpoint",
			pages: oneEndpointSlicePage(validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443,
				discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Serving: &notServing}},
			)),
			want: "non-terminating but not serving",
		},
		{
			name:  "wrong namespace",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Namespace = "other" }),
			want:  "has namespace",
		},
		{
			name:  "padded name",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Name = " kubernetes" }),
			want:  "empty or padded name",
		},
		{
			name:  "wrong Service label",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Labels[discoveryv1.LabelServiceName] = "other" }),
			want:  "not owned",
		},
		{
			name:  "FQDN address type",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.AddressType = discoveryv1.AddressTypeFQDN }),
			want:  "unsupported address type",
		},
		{
			name:  "invalid IP",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Endpoints[0].Addresses[0] = "api.example.test" }),
			want:  "invalid IP address",
		},
		{
			name:  "unspecified IP",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Endpoints[0].Addresses[0] = "0.0.0.0" }),
			want:  "invalid IP address",
		},
		{
			name:  "address family mismatch",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.AddressType = discoveryv1.AddressTypeIPv6 }),
			want:  "does not match address type",
		},
		{
			name:  "empty addresses",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Endpoints[0].Addresses = nil }),
			want:  "has no addresses",
		},
		{
			name: "duplicate address",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) {
				slice.Endpoints[0].Addresses = []string{"10.0.0.2", "10.0.0.2"}
			}),
			want: "repeats address",
		},
		{
			name:  "missing HTTPS port",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Ports[0].Name = nil }),
			want:  "no named and numbered",
		},
		{
			name:  "missing HTTPS port number",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Ports[0].Port = nil }),
			want:  "invalid \"https\" port number",
		},
		{
			name:  "non-TCP HTTPS port",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.Ports[0].Protocol = &udp }),
			want:  "non-TCP",
		},
		{
			name: "duplicate HTTPS port",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) {
				slice.Ports = append(slice.Ports, discoveryv1.EndpointPort{Name: &https, Port: &port})
			}),
			want: "more than one",
		},
		{
			name: "repeated continue token",
			pages: map[string]*discoveryv1.EndpointSliceList{
				"": {
					ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "again"},
					Items:    []discoveryv1.EndpointSlice{validAPIEndpointSlice("kubernetes-a", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}})},
				},
				"again": {
					ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "again"},
					Items:    []discoveryv1.EndpointSlice{validAPIEndpointSlice("kubernetes-b", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}})},
				},
			},
			want: "repeated continue token",
		},
		{
			name: "negative remaining count",
			pages: func() map[string]*discoveryv1.EndpointSliceList {
				remaining := int64(-1)
				page := oneEndpointSlicePage(validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}}))
				page[""].RemainingItemCount = &remaining
				return page
			}(),
			want: "negative remaining item count",
		},
		{
			name: "changed paginated snapshot",
			pages: map[string]*discoveryv1.EndpointSliceList{
				"": {
					ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "again"},
					Items:    []discoveryv1.EndpointSlice{validAPIEndpointSlice("kubernetes-a", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}})},
				},
				"again": {
					ListMeta: metav1.ListMeta{ResourceVersion: "43"},
					Items:    []discoveryv1.EndpointSlice{validAPIEndpointSlice("kubernetes-b", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}})},
				},
			},
			want: "changed resourceVersion",
		},
		{
			name: "duplicate slice across pages",
			pages: map[string]*discoveryv1.EndpointSliceList{
				"": {
					ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "again"},
					Items:    []discoveryv1.EndpointSlice{validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}})},
				},
				"again": {
					ListMeta: metav1.ListMeta{ResourceVersion: "42"},
					Items: func() []discoveryv1.EndpointSlice {
						slice := validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}})
						slice.UID = "different-uid"
						return []discoveryv1.EndpointSlice{slice}
					}(),
				},
			},
			want: "appeared more than once",
		},
		{
			name: "duplicate UID across pages",
			pages: func() map[string]*discoveryv1.EndpointSliceList {
				first := validAPIEndpointSlice("kubernetes-a", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}})
				second := validAPIEndpointSlice("kubernetes-b", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}})
				second.UID = first.UID
				return map[string]*discoveryv1.EndpointSliceList{
					"":      {ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "again"}, Items: []discoveryv1.EndpointSlice{first}},
					"again": {ListMeta: metav1.ListMeta{ResourceVersion: "42"}, Items: []discoveryv1.EndpointSlice{second}},
				}
			}(),
			want: "share UID",
		},
		{
			name: "empty resourceVersion",
			pages: func() map[string]*discoveryv1.EndpointSliceList {
				page := oneEndpointSlicePage(validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}}))
				page[""].ResourceVersion = ""
				return page
			}(),
			want: "empty resourceVersion",
		},
		{
			name:  "empty UID",
			pages: mutatedEndpointSlicePage(func(slice *discoveryv1.EndpointSlice) { slice.UID = "" }),
			want:  "empty UID",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := discoverKubernetesAPIServerAddresses(context.Background(), &scriptedEndpointSliceLister{pages: test.pages})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("discoverKubernetesAPIServerAddresses() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestDiscoverKubernetesAPIServerAddressesAcceptsListHintsAndDeduplicatesTopology(t *testing.T) {
	remainingEstimate := int64(17)
	oversizedPage := make([]discoveryv1.EndpointSlice, teardownEndpointSlicePageSize+1)
	for index := range oversizedPage {
		oversizedPage[index] = validAPIEndpointSlice(
			fmt.Sprintf("kubernetes-%03d", index),
			discoveryv1.AddressTypeIPv4,
			6443,
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}},
		)
	}
	pages := map[string]*discoveryv1.EndpointSliceList{
		"": {
			ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "oversized"},
		},
		"oversized": {
			ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "canonical-duplicates"},
			Items:    oversizedPage,
		},
		"canonical-duplicates": {
			ListMeta: metav1.ListMeta{ResourceVersion: "42", RemainingItemCount: &remainingEstimate},
			Items: []discoveryv1.EndpointSlice{
				validAPIEndpointSlice(
					"kubernetes-ipv6-a",
					discoveryv1.AddressTypeIPv6,
					7443,
					discoveryv1.Endpoint{Addresses: []string{"2001:db8:0:0::20"}},
					discoveryv1.Endpoint{Addresses: []string{"2001:db8::20"}},
				),
				validAPIEndpointSlice(
					"kubernetes-ipv6-b",
					discoveryv1.AddressTypeIPv6,
					7443,
					discoveryv1.Endpoint{Addresses: []string{"2001:db8::20"}},
				),
			},
		},
	}
	lister := &scriptedEndpointSliceLister{pages: pages}

	addresses, err := discoverKubernetesAPIServerAddresses(context.Background(), lister)
	if err != nil {
		t.Fatalf("discoverKubernetesAPIServerAddresses() rejected valid List behavior: %v", err)
	}
	if want := []string{"10.0.0.2:6443", "[2001:db8::20]:7443"}; !reflect.DeepEqual(addresses, want) {
		t.Fatalf("addresses = %#v, want canonical set %#v", addresses, want)
	}
	wantContinues := []string{"", "oversized", "canonical-duplicates"}
	if len(lister.options) != len(wantContinues) {
		t.Fatalf("EndpointSlice list calls = %d, want %d", len(lister.options), len(wantContinues))
	}
	for index, wantContinue := range wantContinues {
		if lister.options[index].Continue != wantContinue || lister.options[index].Limit != teardownEndpointSlicePageSize {
			t.Fatalf("EndpointSlice list call %d = %#v, want Continue %q and Limit %d", index, lister.options[index], wantContinue, teardownEndpointSlicePageSize)
		}
	}
}

func TestDiscoverKubernetesAPIServerAddressesPropagatesListFailures(t *testing.T) {
	wantErr := errors.New("discovery unavailable")
	_, err := discoverKubernetesAPIServerAddresses(context.Background(), &scriptedEndpointSliceLister{errors: map[string]error{"": wantErr}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("discoverKubernetesAPIServerAddresses() error = %v, want wrapping %v", err, wantErr)
	}

	_, err = discoverKubernetesAPIServerAddresses(context.Background(), &scriptedEndpointSliceLister{pages: map[string]*discoveryv1.EndpointSliceList{"": nil}})
	if err == nil || !strings.Contains(err.Error(), "nil page") {
		t.Fatalf("nil page error = %v", err)
	}
}

func TestTeardownAuthorizationSubjectsAndChecksCoverRetiredPrivileges(t *testing.T) {
	rollout := validRBACRolloutGuard()
	contract := validRBACAdmissionContract()
	subjects, err := teardownAuthorizationSubjects(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationSubjects() error = %v", err)
	}
	wantUsers := map[string]string{
		"controller":   "system:serviceaccount:ptah-system:ptah-controller",
		"certificate":  "system:serviceaccount:ptah-system:ptah-certificate",
		"hook-quiesce": "system:serviceaccount:ptah-system:ptah-operator-crd-v1-0123456789ab",
	}
	if len(subjects) != len(wantUsers) {
		t.Fatalf("subject count = %d, want %d", len(subjects), len(wantUsers))
	}
	for _, subject := range subjects {
		if subject.User != wantUsers[subject.Name] {
			t.Errorf("subject %q user = %q, want %q", subject.Name, subject.User, wantUsers[subject.Name])
		}
		wantGroups := []string{"system:serviceaccounts", "system:serviceaccounts:ptah-system", "system:authenticated"}
		if !reflect.DeepEqual(subject.Groups, wantGroups) {
			t.Errorf("subject %q groups = %#v, want %#v", subject.Name, subject.Groups, wantGroups)
		}
	}
	probes, selfChecks, err := teardownAuthorizationProbes(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationProbes() error = %v", err)
	}
	probeCounts := make(map[string]int, len(probes))
	probeChecks := make(map[string]map[string]struct{}, len(probes))
	for _, probe := range probes {
		probeCounts[probe.Subject.Name] = len(probe.Checks)
		probeChecks[probe.Subject.Name] = authorizationCheckNames(probe.Checks)
	}
	if want := map[string]int{"controller": 18, "certificate": 5, "hook-quiesce": 11}; !reflect.DeepEqual(probeCounts, want) {
		t.Fatalf("retired subject probe counts = %#v, want %#v", probeCounts, want)
	}
	for _, test := range []struct {
		subject string
		check   string
	}{
		{subject: "controller", check: "patch PtahSchema"},
		{subject: "certificate", check: "update mutating admission singleton"},
		{subject: "certificate", check: "update webhook Secret"},
		{subject: "hook-quiesce", check: "update CRD ptahschemas.operator.ptah.dev"},
		{subject: "hook-quiesce", check: "create guarded Deployment"},
	} {
		if _, found := probeChecks[test.subject][test.check]; !found {
			t.Errorf("retired subject %q is missing actual rendered grant check %q", test.subject, test.check)
		}
	}
	if _, found := probeChecks["controller"]["update CRD ptahschemas.operator.ptah.dev"]; found {
		t.Error("controller probe includes a hook-only CRD update grant")
	}
	if _, found := probeChecks["hook-quiesce"]["update PtahSchema"]; found {
		t.Error("hook probe includes a controller-only custom resource update grant")
	}
	if len(selfChecks) != 16 {
		t.Fatalf("current cleanup credential check count = %d, want 16", len(selfChecks))
	}
	selfCheckNames := authorizationCheckNames(selfChecks)
	if _, found := selfCheckNames["delete RoleBinding ptah-system/"+mustTeardownPrivilegeRoleName(t, rollout.HookServiceAccountName)]; !found {
		t.Error("current cleanup credential checks omit release cleanup RoleBinding self-revocation")
	}
	for _, check := range selfChecks {
		if check.ResourceAttributes == nil || check.ResourceAttributes.Verb != "delete" {
			t.Errorf("current cleanup credential check is not a revoked exact delete: %#v", check)
		}
		if check.ResourceAttributes.Resource == "clusterrolebindings" && check.ResourceAttributes.Name == mustTeardownPrivilegeRoleName(t, rollout.HookServiceAccountName) {
			t.Errorf("current cleanup credential checks include residual ClusterRoleBinding deletion: %#v", check)
		}
	}

	checks, err := teardownAuthorizationChecks(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationChecks() error = %v", err)
	}
	cleanupPrivilegeName, err := crdupgrade.TeardownPrivilegeRoleName(rollout.HookServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	wantChecks := []string{
		"update CRD ptahschemas.operator.ptah.dev",
		"update controller Deployment",
		"create guarded Deployment",
		"delete ClusterRoleBinding ptah-operator",
		"delete ClusterRoleBinding ptah-operator-quiesce-v1-0123456789ab",
		"mint hook ServiceAccount token",
		"update webhook Secret",
		"create webhook Secret",
		"update mutating admission singleton",
		"update validating admission singleton",
		"patch PtahSchema",
		"update PtahSchema status",
		"patch PtahSchema status",
		"update PtahSchema finalizer",
		"update PtahSchemaPlan finalizer",
		"create PtahSchemaPlan",
		"create operation Job",
		"patch operation Job",
		"create controller ConfigMap",
		"create controller Event",
		"patch controller Event",
		"update controller Event",
		"create leader-election Lease",
		"update leader-election Lease",
		"update certificate rotation Lease",
		"delete RoleBinding ptah-system/" + cleanupPrivilegeName,
	}
	byName := make(map[string]*authorizationv1.ResourceAttributes, len(checks))
	cleanupGuardName, err := crdupgrade.TeardownGuardRoleName(rollout.HookServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		attributes := check.ResourceAttributes
		if attributes == nil || attributes.Name == "" {
			t.Fatalf("check %q is not an exact named resource check: %#v", check.Name, check)
		}
		byName[check.Name] = attributes
		if attributes.Verb == "delete" && attributes.Group == "admissionregistration.k8s.io" {
			t.Errorf("check %q includes intentional residual admission deletion", check.Name)
		}
		if attributes.Verb == "delete" &&
			(attributes.Resource == "clusterrolebindings" || attributes.Resource == "clusterroles") &&
			(attributes.Name == cleanupPrivilegeName || attributes.Name == cleanupGuardName) {
			t.Errorf("check %q includes intentional residual RBAC deletion for %q", check.Name, attributes.Name)
		}
	}
	if len(checks) != 48 || len(byName) != 48 {
		t.Fatalf("authorization checks = %d total/%d unique, want 48/48", len(checks), len(byName))
	}
	for _, name := range wantChecks {
		if byName[name] == nil {
			t.Errorf("missing authorization convergence check %q", name)
		}
	}
	if got := byName["update controller Deployment"]; got.Namespace != rollout.ReleaseNamespace || got.Group != "apps" || got.Resource != "deployments" || got.Verb != "update" || got.Name != rollout.ControllerDeploymentName {
		t.Errorf("controller Deployment check = %#v", got)
	}
	if got := byName["update PtahSchema status"]; got.Group != "operator.ptah.dev" || got.Resource != "ptahschemas" || got.Subresource != "status" {
		t.Errorf("PtahSchema status check = %#v", got)
	}
	for _, name := range []string{
		"update PtahSchemaPlan status",
		"patch PtahSchemaPlan status",
		"update PtahSchemaApproval status",
		"patch PtahSchemaApproval status",
	} {
		if byName[name] == nil {
			t.Errorf("missing distinct subresource/verb authorization check %q", name)
		}
	}
}

func TestTeardownAuthorizationChecksUseSeparateCoordinationNamespace(t *testing.T) {
	rollout := validRBACRolloutGuard()
	rollout.CoordinationNamespace = "ptah-coordination"

	contract := validRBACAdmissionContract()
	checks, err := teardownAuthorizationChecks(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationChecks() error = %v", err)
	}
	byName := make(map[string]*authorizationv1.ResourceAttributes, len(checks))
	for _, check := range checks {
		byName[check.Name] = check.ResourceAttributes
	}
	for _, name := range []string{"delete RoleBinding " + rollout.ControllerDeploymentName} {
		attributes := byName[name]
		if attributes == nil {
			t.Fatalf("missing authorization convergence check %q", name)
		}
		if attributes.Namespace != rollout.CoordinationNamespace {
			t.Errorf("check %q namespace = %q, want %q", name, attributes.Namespace, rollout.CoordinationNamespace)
		}
	}
	if len(checks) != 49 {
		t.Fatalf("split-namespace authorization check count = %d, want 49", len(checks))
	}
	_, selfChecks, err := teardownAuthorizationProbes(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationProbes() error = %v", err)
	}
	if len(selfChecks) != 17 {
		t.Fatalf("split-namespace current cleanup credential check count = %d, want 17", len(selfChecks))
	}
	privilegeName := mustTeardownPrivilegeRoleName(t, rollout.HookServiceAccountName)
	selfNames := authorizationCheckNames(selfChecks)
	for _, namespace := range []string{rollout.ReleaseNamespace, rollout.CoordinationNamespace} {
		name := "delete RoleBinding " + namespace + "/" + privilegeName
		if _, found := selfNames[name]; !found {
			t.Errorf("current cleanup credential checks omit self-revocation %q", name)
		}
	}
}

func TestTeardownAuthorizationProbesCoverConditionalRBACBranches(t *testing.T) {
	for _, certificateEnabled := range []bool{false, true} {
		for _, controllerServiceAccountCreated := range []bool{false, true} {
			for _, splitCoordinationNamespace := range []bool{false, true} {
				name := fmt.Sprintf(
					"certificate=%t/controller-service-account=%t/split-coordination=%t",
					certificateEnabled,
					controllerServiceAccountCreated,
					splitCoordinationNamespace,
				)
				t.Run(name, func(t *testing.T) {
					rollout := validRBACRolloutGuard()
					contract := validRBACAdmissionContract()
					contract.CertificateRuntimeEnabled = certificateEnabled
					contract.ControllerServiceAccountCreate = controllerServiceAccountCreated
					if !certificateEnabled {
						rollout.CertificateArgs = nil
					}
					if splitCoordinationNamespace {
						rollout.CoordinationNamespace = "ptah-coordination"
					}

					probes, selfChecks, err := teardownAuthorizationProbes(rollout, contract)
					if err != nil {
						t.Fatalf("teardownAuthorizationProbes() error = %v", err)
					}
					counts := make(map[string]int, len(probes))
					for _, probe := range probes {
						counts[probe.Subject.Name] = len(probe.Checks)
					}
					wantCounts := map[string]int{"controller": 18, "hook-quiesce": 11}
					if certificateEnabled {
						wantCounts["certificate"] = 5
					}
					if !reflect.DeepEqual(counts, wantCounts) {
						t.Fatalf("retired subject probe counts = %#v, want %#v", counts, wantCounts)
					}

					wantSelfChecks := 12
					if splitCoordinationNamespace {
						wantSelfChecks++
					}
					if controllerServiceAccountCreated {
						wantSelfChecks++
					}
					if certificateEnabled {
						wantSelfChecks += 3
					}
					if len(selfChecks) != wantSelfChecks {
						t.Fatalf("current cleanup credential checks = %d, want %d", len(selfChecks), wantSelfChecks)
					}
					checks, err := teardownAuthorizationChecks(rollout, contract)
					if err != nil {
						t.Fatalf("teardownAuthorizationChecks() error = %v", err)
					}
					wantUnion := 29 + wantSelfChecks
					if certificateEnabled {
						wantUnion += 3
					}
					if len(checks) != wantUnion {
						t.Fatalf("authorization check union = %d, want %d", len(checks), wantUnion)
					}
				})
			}
		}
	}
}

func TestTeardownAuthorizationProbeCompletenessRejectsMissingAndForeignChecks(t *testing.T) {
	rollout := validRBACRolloutGuard()
	contract := validRBACAdmissionContract()
	probes, selfChecks, err := teardownAuthorizationProbes(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationProbes() error = %v", err)
	}
	grants, err := crdupgrade.RevokedPrivilegeMutationGrants(rollout, contract)
	if err != nil {
		t.Fatalf("RevokedPrivilegeMutationGrants() error = %v", err)
	}
	cloneProbes := func() []crdupgrade.AuthorizationProbe {
		cloned := append([]crdupgrade.AuthorizationProbe(nil), probes...)
		for index := range cloned {
			cloned[index].Checks = append([]crdupgrade.AuthorizationCheck(nil), cloned[index].Checks...)
		}
		return cloned
	}

	missing := cloneProbes()
	missing[0].Checks = missing[0].Checks[1:]
	if err := validateTeardownAuthorizationProbeCompleteness(missing, selfChecks, grants); err == nil || !strings.Contains(err.Error(), "omit") {
		t.Fatalf("missing probe validation error = %v, want omitted grant", err)
	}

	missingSelf := append([]crdupgrade.AuthorizationCheck(nil), selfChecks[1:]...)
	if err := validateTeardownAuthorizationProbeCompleteness(probes, missingSelf, grants); err == nil || !strings.Contains(err.Error(), "omit") {
		t.Fatalf("missing self probe validation error = %v, want omitted cleanup grant", err)
	}

	foreign := cloneProbes()
	foreign[0].Checks = append(foreign[0].Checks, crdupgrade.AuthorizationCheck{
		Name: "foreign secret deletion",
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: rollout.ReleaseNamespace,
			Verb:      "delete",
			Resource:  "secrets",
			Name:      "foreign",
		},
	})
	if err := validateTeardownAuthorizationProbeCompleteness(foreign, selfChecks, grants); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("foreign probe validation error = %v, want exact contract rejection", err)
	}
}

func TestTeardownAuthorizationContractOmitsDisabledCertificateIdentityAndMutations(t *testing.T) {
	rollout := validRBACRolloutGuard()
	rollout.CertificateArgs = nil
	contract := validRBACAdmissionContract()
	contract.CertificateRuntimeEnabled = false

	subjects, err := teardownAuthorizationSubjects(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationSubjects() error = %v", err)
	}
	for _, subject := range subjects {
		if subject.Name == "certificate" {
			t.Fatalf("disabled certificate identity was included: %#v", subject)
		}
	}
	checks, err := teardownAuthorizationChecks(rollout, contract)
	if err != nil {
		t.Fatalf("teardownAuthorizationChecks() error = %v", err)
	}
	for _, check := range checks {
		if strings.Contains(check.Name, "webhook Secret") || strings.Contains(check.Name, "certificate rotation Lease") {
			t.Errorf("disabled certificate mutation was included: %q", check.Name)
		}
		if check.ResourceAttributes != nil && check.ResourceAttributes.Verb == "delete" && check.ResourceAttributes.Name == rollout.CertificateDeploymentName {
			t.Errorf("disabled certificate cleanup mutation was included: %q", check.Name)
		}
	}
}

func TestTeardownAuthorizationChecksRejectsAmbiguousCertificateArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing lease", args: []string{"--recreate-missing-secret=true"}, want: "--lease-name= is required"},
		{name: "duplicate lease", args: []string{"--lease-name=one", "--lease-name=two"}, want: "--lease-name= is duplicated"},
		{name: "empty lease", args: []string{"--lease-name="}, want: "empty or padded value"},
		{name: "invalid recovery flag", args: []string{"--lease-name=lease", "--recreate-missing-secret=yes"}, want: "exactly true or false"},
		{name: "duplicate recovery flag", args: []string{"--lease-name=lease", "--recreate-missing-secret=true", "--recreate-missing-secret=false"}, want: "is duplicated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rollout := validRBACRolloutGuard()
			rollout.CertificateArgs = test.args
			_, err := teardownAuthorizationChecks(rollout, validRBACAdmissionContract())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("teardownAuthorizationChecks() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestNewTeardownRBACConvergenceBarrierRejectsInvalidInputsAndFactories(t *testing.T) {
	validLister := &scriptedEndpointSliceLister{pages: oneEndpointSlicePage(
		validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}}),
	)}
	validFactory := func(*rest.Config) (crdupgrade.AuthorizationReviewClient, error) {
		return &deniedSubjectAccessReviewClient{}, nil
	}
	rollout := validRBACRolloutGuard()
	contract := validRBACAdmissionContract()

	tests := []struct {
		name     string
		ctx      context.Context
		config   *rest.Config
		lister   endpointSliceLister
		rollout  *crdupgrade.RolloutGuard
		contract crdupgrade.RuntimeAdmissionContract
		factory  authorizationReviewClientFactory
		want     string
	}{
		{name: "nil context", config: validRBACRESTConfig(), lister: validLister, rollout: rollout, contract: contract, factory: validFactory, want: "context is nil"},
		{name: "nil config", ctx: context.Background(), lister: validLister, rollout: rollout, contract: contract, factory: validFactory, want: "REST configuration"},
		{name: "insecure TLS", ctx: context.Background(), config: &rest.Config{TLSClientConfig: rest.TLSClientConfig{Insecure: true}}, lister: validLister, rollout: rollout, contract: contract, factory: validFactory, want: "verify API server TLS"},
		{name: "missing CA", ctx: context.Background(), config: &rest.Config{}, lister: validLister, rollout: rollout, contract: contract, factory: validFactory, want: "no API server CA"},
		{name: "nil lister", ctx: context.Background(), config: validRBACRESTConfig(), rollout: rollout, contract: contract, factory: validFactory, want: "EndpointSlice client"},
		{name: "nil rollout", ctx: context.Background(), config: validRBACRESTConfig(), lister: validLister, contract: contract, factory: validFactory, want: "rollout guard"},
		{name: "nil factory", ctx: context.Background(), config: validRBACRESTConfig(), lister: validLister, rollout: rollout, contract: contract, want: "client factory"},
		{
			name:     "contract namespace mismatch",
			ctx:      context.Background(),
			config:   validRBACRESTConfig(),
			lister:   validLister,
			rollout:  rollout,
			contract: mutateRBACContract(contract, func(value *crdupgrade.RuntimeAdmissionContract) { value.Namespace = "other" }),
			factory:  validFactory,
			want:     "differs from rollout namespace",
		},
		{
			name:     "controller identity mismatch",
			ctx:      context.Background(),
			config:   validRBACRESTConfig(),
			lister:   validLister,
			rollout:  rollout,
			contract: mutateRBACContract(contract, func(value *crdupgrade.RuntimeAdmissionContract) { value.ControllerServiceAccountName = "other" }),
			factory:  validFactory,
			want:     "differs from rollout identity",
		},
		{
			name:   "shared identities",
			ctx:    context.Background(),
			config: validRBACRESTConfig(),
			lister: validLister,
			rollout: mutateRBACRollout(rollout, func(value *crdupgrade.RolloutGuard) {
				value.CertificateDeploymentName = value.ControllerServiceAccountName
			}),
			contract: mutateRBACContract(contract, func(value *crdupgrade.RuntimeAdmissionContract) {
				value.CertificateServiceAccountName = value.ControllerServiceAccountName
			}),
			factory: validFactory,
			want:    "share ServiceAccount",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newTeardownRBACConvergenceBarrierWith(test.ctx, test.config, test.lister, test.rollout, test.contract, test.factory)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newTeardownRBACConvergenceBarrierWith() error = %v, want containing %q", err, test.want)
			}
		})
	}

	_, err := newTeardownRBACConvergenceBarrierWith(
		context.Background(),
		validRBACRESTConfig(),
		validLister,
		rollout,
		contract,
		func(*rest.Config) (crdupgrade.AuthorizationReviewClient, error) {
			return nil, errors.New("factory failed")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "factory failed") || !strings.Contains(err.Error(), "10.0.0.2:6443") {
		t.Fatalf("factory failure error = %v", err)
	}

	_, err = newTeardownRBACConvergenceBarrierWith(
		context.Background(),
		validRBACRESTConfig(),
		validLister,
		rollout,
		contract,
		func(*rest.Config) (crdupgrade.AuthorizationReviewClient, error) { return nil, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "returned nil") {
		t.Fatalf("nil factory result error = %v", err)
	}
}

func validAPIEndpointSlice(name string, addressType discoveryv1.AddressType, port int32, endpoints ...discoveryv1.Endpoint) discoveryv1.EndpointSlice {
	portName := kubernetesServiceTLSPortName
	protocol := corev1.ProtocolTCP
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kubernetesServiceNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: kubernetesServiceName},
			UID:       types.UID("uid-" + name),
		},
		AddressType: addressType,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &port}},
		Endpoints:   endpoints,
	}
}

func oneEndpointSlicePage(slice discoveryv1.EndpointSlice) map[string]*discoveryv1.EndpointSliceList {
	return map[string]*discoveryv1.EndpointSliceList{
		"": {ListMeta: metav1.ListMeta{ResourceVersion: "42"}, Items: []discoveryv1.EndpointSlice{slice}},
	}
}

func mutatedEndpointSlicePage(mutate func(*discoveryv1.EndpointSlice)) map[string]*discoveryv1.EndpointSliceList {
	slice := validAPIEndpointSlice("kubernetes", discoveryv1.AddressTypeIPv4, 6443, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}})
	mutate(&slice)
	return oneEndpointSlicePage(slice)
}

func validRBACRolloutGuard() *crdupgrade.RolloutGuard {
	return &crdupgrade.RolloutGuard{
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		CoordinationNamespace:        "ptah-system",
		LeaderElectionID:             "ptah-operator-leader-election",
		WebhookSecretName:            "ptah-operator-webhook",
		HookServiceAccountName:       "ptah-operator-crd-v1-0123456789ab",
		ControllerServiceAccountName: "ptah-controller",
		ControllerDeploymentName:     "ptah-operator",
		CertificateDeploymentName:    "ptah-certificate",
		ReleaseSequence:              1,
		ManagerImage:                 "registry.example.test/ptah-operator@sha256:1234",
		CertificateArgs: []string{
			"--lease-name=ptah-cert-rotation",
			"--recreate-missing-secret=true",
		},
	}
}

func validRBACAdmissionContract() crdupgrade.RuntimeAdmissionContract {
	return crdupgrade.RuntimeAdmissionContract{
		Namespace:                      "ptah-system",
		ControllerServiceAccountName:   "ptah-controller",
		CertificateServiceAccountName:  "ptah-certificate",
		ControllerServiceAccountCreate: true,
		CertificateRuntimeEnabled:      true,
	}
}

func validRBACRESTConfig() *rest.Config {
	return &rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: []byte("cluster-ca")}}
}

func mutateRBACRollout(source *crdupgrade.RolloutGuard, mutate func(*crdupgrade.RolloutGuard)) *crdupgrade.RolloutGuard {
	copy := *source
	mutate(&copy)
	return &copy
}

func mutateRBACContract(source crdupgrade.RuntimeAdmissionContract, mutate func(*crdupgrade.RuntimeAdmissionContract)) crdupgrade.RuntimeAdmissionContract {
	mutate(&source)
	return source
}

func authorizationCheckNames(checks []crdupgrade.AuthorizationCheck) map[string]struct{} {
	names := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		names[check.Name] = struct{}{}
	}
	return names
}

func mustTeardownPrivilegeRoleName(t *testing.T, hookServiceAccountName string) string {
	t.Helper()
	name, err := crdupgrade.TeardownPrivilegeRoleName(hookServiceAccountName)
	if err != nil {
		t.Fatalf("derive teardown privilege role name: %v", err)
	}
	return name
}

func endpointNames(barrier *crdupgrade.RBACConvergenceBarrier) []string {
	return namedAuthorizationEndpointNames(barrier.Endpoints)
}

func authorizationSweepSize(barrier *crdupgrade.RBACConvergenceBarrier) int {
	size := len(barrier.SelfChecks)
	for _, probe := range barrier.Probes {
		size += len(probe.Checks)
	}
	return size
}

func namedAuthorizationEndpointNames(endpoints []crdupgrade.NamedAuthorizationReviewClient) []string {
	names := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		names = append(names, endpoint.Name)
	}
	return names
}

type scriptedEndpointSliceLister struct {
	pages   map[string]*discoveryv1.EndpointSliceList
	errors  map[string]error
	options []metav1.ListOptions
}

func (l *scriptedEndpointSliceLister) List(_ context.Context, options metav1.ListOptions) (*discoveryv1.EndpointSliceList, error) {
	l.options = append(l.options, options)
	if err := l.errors[options.Continue]; err != nil {
		return nil, err
	}
	page, found := l.pages[options.Continue]
	if !found {
		return nil, fmt.Errorf("unexpected continuation token %q", options.Continue)
	}
	if page == nil {
		return nil, nil
	}
	return page.DeepCopy(), nil
}

type deniedSubjectAccessReviewClient struct{}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (*deniedSubjectAccessReviewClient) CreateSubjectAccessReview(
	context.Context,
	*authorizationv1.SubjectAccessReview,
	metav1.CreateOptions,
) (*authorizationv1.SubjectAccessReview, error) {
	return &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Denied: true}}, nil
}

func (*deniedSubjectAccessReviewClient) CreateSelfSubjectAccessReview(
	context.Context,
	*authorizationv1.SelfSubjectAccessReview,
	metav1.CreateOptions,
) (*authorizationv1.SelfSubjectAccessReview, error) {
	return &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Denied: true}}, nil
}
