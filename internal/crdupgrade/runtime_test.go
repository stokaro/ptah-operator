package crdupgrade

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestRuntimeVerifierAcceptsExactSingleton(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := verifier.Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeVerifierRejectsMismatchedOwner(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	verifier.Mutating.(*mutatingAdmissionClient).object.Annotations[ReleaseNameAnnotation] = "other-release"
	err := verifier.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), ReleaseNameAnnotation) {
		t.Fatalf("Verify error = %v, want release annotation mismatch", err)
	}
}

func TestRuntimeVerifierRejectsEveryInvariantMismatch(t *testing.T) {
	tests := []string{
		ReleaseNamespaceAnnotation,
		CoordinationAnnotation,
		LeaderElectionAnnotation,
		LeaderElectionIDAnnotation,
		WebhookServiceAnnotation,
		HookServiceAccountAnnotation,
		ControllerServiceAccountAnnotation,
		ControllerDeploymentAnnotation,
		CertificateDeploymentAnnotation,
		ReleaseSequenceAnnotation,
	}
	for _, annotation := range tests {
		t.Run(annotation, func(t *testing.T) {
			verifier := readyRuntimeVerifier(t)
			verifier.Validating.(*validatingAdmissionClient).object.Annotations[annotation] = "mismatch"
			err := verifier.Verify(context.Background())
			if err == nil || !strings.Contains(err.Error(), annotation) {
				t.Fatalf("Verify error = %v, want %s mismatch", err, annotation)
			}
		})
	}
}

func TestRuntimeVerifierFailsClosedWhenSingletonIsIncomplete(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	verifier.Validating.(*validatingAdmissionClient).object = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := verifier.Verify(ctx)
	if err == nil || !strings.Contains(err.Error(), "is incomplete") || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("Verify error = %v, want incomplete singleton failure", err)
	}
}

func TestRuntimeVerifierFailsClosedWhenSingletonIsMissing(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	verifier.Mutating.(*mutatingAdmissionClient).object = nil
	verifier.Validating.(*validatingAdmissionClient).object = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := verifier.Verify(ctx)
	if err == nil || !strings.Contains(err.Error(), "is incomplete") || !strings.Contains(err.Error(), "MutatingWebhookConfiguration") {
		t.Fatalf("Verify error = %v, want missing singleton failure", err)
	}
}

func TestRuntimeVerifierRechecksCRDsAfterAdmissionVerification(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	crdClient := verifier.CRDs.Client.(*memoryClient)
	verifier.Mutating.(*mutatingAdmissionClient).onGet = func() {
		crdClient.objects[PtahSchemaCRDName].Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "concurrent drift"
	}
	err := verifier.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "re-verify candidate CRDs") || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Verify error = %v, want concurrent CRD drift refusal", err)
	}
}

func TestRuntimeVerifierRejectsAdmissionContractDrift(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*RuntimeVerifier)
	}{
		{
			name: "cardinality", want: "expected exactly 1",
			mutate: func(verifier *RuntimeVerifier) {
				client := verifier.Mutating.(*mutatingAdmissionClient)
				client.object.Webhooks = append(client.object.Webhooks, client.object.Webhooks[0])
			},
		},
		{
			name: "validating cardinality", want: "expected exactly 3",
			mutate: func(verifier *RuntimeVerifier) {
				client := verifier.Validating.(*validatingAdmissionClient)
				client.object.Webhooks = client.object.Webhooks[:1]
			},
		},
		{
			name: "validating order", want: "has name",
			mutate: func(verifier *RuntimeVerifier) {
				webhooks := verifier.Validating.(*validatingAdmissionClient).object.Webhooks
				webhooks[1], webhooks[2] = webhooks[2], webhooks[1]
			},
		},
		{
			name: "webhook name", want: "has name",
			mutate: func(verifier *RuntimeVerifier) {
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].Name = "foreign.operator.ptah.dev"
			},
		},
		{
			name: "failure policy", want: "failurePolicy must be Fail",
			mutate: func(verifier *RuntimeVerifier) {
				ignore := admissionregistrationv1.Ignore
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].FailurePolicy = &ignore
			},
		},
		{
			name: "side effects", want: "sideEffects must be None",
			mutate: func(verifier *RuntimeVerifier) {
				unknown := admissionregistrationv1.SideEffectClassUnknown
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].SideEffects = &unknown
			},
		},
		{
			name: "match policy", want: "matchPolicy must be Equivalent",
			mutate: func(verifier *RuntimeVerifier) {
				exact := admissionregistrationv1.Exact
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].MatchPolicy = &exact
			},
		},
		{
			name: "reinvocation", want: "reinvocationPolicy",
			mutate: func(verifier *RuntimeVerifier) {
				ifNeeded := admissionregistrationv1.IfNeededReinvocationPolicy
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].ReinvocationPolicy = &ifNeeded
			},
		},
		{
			name: "review version", want: "admissionReviewVersions",
			mutate: func(verifier *RuntimeVerifier) {
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].AdmissionReviewVersions = []string{"v1beta1"}
			},
		},
		{
			name: "CA bundle", want: "caBundle must be nonempty",
			mutate: func(verifier *RuntimeVerifier) {
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].ClientConfig.CABundle = nil
			},
		},
		{
			name: "service", want: "Service target does not match",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, validatingApprovalWebhookName).ClientConfig.Service.Name = "foreign-service"
			},
		},
		{
			name: "service path", want: "Service target does not match",
			mutate: func(verifier *RuntimeVerifier) {
				path := "/foreign"
				validatingWebhook(t, verifier, validatingApprovalWebhookName).ClientConfig.Service.Path = &path
			},
		},
		{
			name: "URL target", want: "not a URL",
			mutate: func(verifier *RuntimeVerifier) {
				url := "https://foreign.invalid"
				webhook := validatingWebhook(t, verifier, validatingApprovalWebhookName)
				webhook.ClientConfig.Service = nil
				webhook.ClientConfig.URL = &url
			},
		},
		{
			name: "rules", want: "rules do not match",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, validatingApprovalWebhookName).Rules[0].Resources = []string{"*"}
			},
		},
		{
			name: "mutating approval selector", want: "objectSelector",
			mutate: func(verifier *RuntimeVerifier) {
				verifier.Mutating.(*mutatingAdmissionClient).object.Webhooks[0].ObjectSelector = &metav1.LabelSelector{}
			},
		},
		{
			name: "validating approval selector", want: "objectSelector",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, validatingApprovalWebhookName).ObjectSelector = &metav1.LabelSelector{}
			},
		},
		{
			name: "namespace selector", want: "namespaceSelector",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, validatingApprovalWebhookName).NamespaceSelector = &metav1.LabelSelector{}
			},
		},
		{
			name: "approval match condition", want: "must not have matchConditions",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, validatingApprovalWebhookName).MatchConditions =
					[]admissionregistrationv1.MatchCondition{{Name: "foreign", Expression: "true"}}
			},
		},
		{
			name: "pod selector missing", want: "objectSelector",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, podIntentWebhookName).ObjectSelector = nil
			},
		},
		{
			name: "pod selector empty", want: "objectSelector",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, podIntentWebhookName).ObjectSelector = &metav1.LabelSelector{}
			},
		},
		{
			name: "pod selector wrong", want: "objectSelector",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, podIntentWebhookName).ObjectSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{
						managedByLabel:                "ptah-operator",
						"app.kubernetes.io/component": "foreign",
					},
				}
			},
		},
		{
			name: "pod match condition", want: "matchConditions",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, podIntentWebhookName).MatchConditions[0].Expression = "true"
			},
		},
		{
			name: "pod match condition literal", want: "matchConditions",
			mutate: func(verifier *RuntimeVerifier) {
				webhook := validatingWebhook(t, verifier, podIntentWebhookName)
				webhook.MatchConditions[0].Expression = strings.Replace(webhook.MatchConditions[0].Expression, "'batch/v1'", "'batch /v1'", 1)
			},
		},
		{
			name: "controller write match policy", want: "matchPolicy must be Exact",
			mutate: func(verifier *RuntimeVerifier) {
				equivalent := admissionregistrationv1.Equivalent
				validatingWebhook(t, verifier, controllerWriteWebhookName).MatchPolicy = &equivalent
			},
		},
		{
			name: "controller write rules", want: "rules do not match",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, controllerWriteWebhookName).Rules[0].Resources = []string{"*"}
			},
		},
		{
			name: "controller write identity", want: "matchConditions",
			mutate: func(verifier *RuntimeVerifier) {
				validatingWebhook(t, verifier, controllerWriteWebhookName).MatchConditions[0].Expression = "true"
			},
		},
		{
			name: "controller write timeout", want: "timeoutSeconds must be 30",
			mutate: func(verifier *RuntimeVerifier) {
				timeout := int32(5)
				validatingWebhook(t, verifier, controllerWriteWebhookName).TimeoutSeconds = &timeout
			},
		},
		{
			name: "timeout", want: "timeoutSeconds must be 5",
			mutate: func(verifier *RuntimeVerifier) {
				timeout := int32(6)
				validatingWebhook(t, verifier, podIntentWebhookName).TimeoutSeconds = &timeout
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verifier := readyRuntimeVerifier(t)
			test.mutate(verifier)
			err := verifier.Verify(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Verify error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeVerifierRechecksAdmissionContractBeforeSuccess(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	client := verifier.Validating.(*validatingAdmissionClient)
	client.onGet = func(call int) {
		if call == 2 {
			ignore := admissionregistrationv1.Ignore
			client.object.Webhooks[0].FailurePolicy = &ignore
		}
	}
	err := verifier.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "final admission singleton verification") ||
		!strings.Contains(err.Error(), "failurePolicy must be Fail") {
		t.Fatalf("Verify error = %v, want final admission contract refusal", err)
	}
}

func TestRuntimeVerifierRejectsStoredFutureControllerState(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	state := storedStateClientsWithSchemas(&schemaListClient{pages: []*unstructured.UnstructuredList{{
		Items: []unstructured.Unstructured{schemaWithControllerState("tenant-a", "schema-a", int64(2))},
	}}})
	verifier.StoredState = &state
	verifier.SupportedControllerStateVersion = 1
	err := verifier.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "controller downgrade refused") || !strings.Contains(err.Error(), "tenant-a/schema-a") {
		t.Fatalf("Verify error = %v, want stored future-state refusal", err)
	}
}

func TestVerifyStoredControllerStateRequiresEveryClient(t *testing.T) {
	var nilPlanClient *schemaListClient
	tests := []struct {
		name    string
		clients StoredControllerStateClients
		want    string
	}{
		{name: "schema", clients: StoredControllerStateClients{}, want: "PtahSchema client is required"},
		{
			name: "plan",
			clients: StoredControllerStateClients{
				Schemas: &schemaListClient{},
			},
			want: "PtahSchemaPlan client is required",
		},
		{
			name: "typed nil plan",
			clients: StoredControllerStateClients{
				Schemas: &schemaListClient{},
				Plans:   nilPlanClient,
			},
			want: "PtahSchemaPlan client is required",
		},
		{
			name: "approval",
			clients: StoredControllerStateClients{
				Schemas: &schemaListClient{},
				Plans:   &schemaListClient{},
			},
			want: "PtahSchemaApproval client is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := VerifyStoredControllerState(context.Background(), test.clients, 1)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyStoredControllerState error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeVerifierRejectsFutureControllerStateInEveryDurableLocation(t *testing.T) {
	tests := []struct {
		name string
		path []string
	}{
		{name: "plan", path: []string{"status", "plan", "controllerStateVersion"}},
		{name: "applied", path: []string{"status", "applied", "controllerStateVersion"}},
		{name: "pending observation plan", path: []string{"status", "pendingObservation", "plan", "controllerStateVersion"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verifier := readyRuntimeVerifier(t)
			object := schemaWithControllerStateAt("tenant-a", "schema-a", int64(2), test.path...)
			// A current top-level plan and a missing legacy execution binding must
			// not hide future state in another durable evidence location.
			if test.name == "pending observation plan" {
				setControllerStateAt(&object, int64(1), "status", "plan", "controllerStateVersion")
			}
			state := storedStateClientsWithSchemas(&schemaListClient{pages: []*unstructured.UnstructuredList{{
				Items: []unstructured.Unstructured{object},
			}}})
			verifier.StoredState = &state
			verifier.SupportedControllerStateVersion = 1
			err := verifier.Verify(context.Background())
			if err == nil || !strings.Contains(err.Error(), "controller downgrade refused") ||
				!strings.Contains(err.Error(), strings.Join(test.path[:len(test.path)-1], ".")) {
				t.Fatalf("Verify error = %v, want future-state refusal at %s", err, test.name)
			}
		})
	}
}

func TestRuntimeVerifierAcceptsLegacyAndCurrentStateAcrossPages(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	firstPage := &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{
			schemaWithoutControllerState("legacy", "missing"),
			schemaWithControllerState("legacy", "zero", int64(0)),
		},
	}
	firstPage.SetContinue("page-two")
	client := &schemaListClient{pages: []*unstructured.UnstructuredList{
		firstPage,
		{
			Items: []unstructured.Unstructured{schemaWithControllerState("current", "schema", int64(1))},
		},
	}}
	state := storedStateClientsWithSchemas(client)
	verifier.StoredState = &state
	verifier.SupportedControllerStateVersion = 1
	if err := verifier.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.options) != 2 || client.options[0].Continue != "" || client.options[1].Continue != "page-two" {
		t.Fatalf("list options = %#v, want paginated cluster-wide scan", client.options)
	}
}

func TestVerifyStoredControllerStateAcceptsKubernetesListHints(t *testing.T) {
	oversizedPage := make([]unstructured.Unstructured, storedControllerStatePageSize+1)
	for index := range oversizedPage {
		oversizedPage[index] = schemaWithControllerState("tenant-a", fmt.Sprintf("schema-%03d", index), int64(1))
	}
	terminalRemainingEstimate := int64(23)
	terminalPage := schemaStatePage(
		"10",
		"",
		schemaWithoutControllerState("tenant-b", "terminal"),
	)
	terminalPage.SetRemainingItemCount(&terminalRemainingEstimate)
	client := &schemaListClient{pages: []*unstructured.UnstructuredList{
		schemaStatePage("10", "oversized"),
		schemaStatePage("10", "terminal", oversizedPage...),
		terminalPage,
	}}

	if err := VerifyStoredControllerState(context.Background(), storedStateClientsWithSchemas(client), 1); err != nil {
		t.Fatalf("VerifyStoredControllerState rejected valid List behavior: %v", err)
	}
	wantContinues := []string{"", "oversized", "terminal"}
	if len(client.options) != len(wantContinues) {
		t.Fatalf("PtahSchema list calls = %d, want %d", len(client.options), len(wantContinues))
	}
	for index, wantContinue := range wantContinues {
		if client.options[index].Continue != wantContinue || client.options[index].Limit != storedControllerStatePageSize {
			t.Fatalf("PtahSchema list call %d = %#v, want Continue %q and Limit %d", index, client.options[index], wantContinue, storedControllerStatePageSize)
		}
	}
}

func TestVerifyStoredControllerStateRejectsMalformedPagination(t *testing.T) {
	tests := []struct {
		name   string
		client *schemaListClient
		want   string
	}{
		{
			name: "missing resource version",
			client: &schemaListClient{
				pages:                        []*unstructured.UnstructuredList{{}},
				preserveEmptyResourceVersion: true,
			},
			want: "empty resourceVersion",
		},
		{
			name: "changed resource version",
			client: &schemaListClient{pages: []*unstructured.UnstructuredList{
				schemaStatePage("10", "next", schemaWithoutControllerState("tenant-a", "first")),
				schemaStatePage("11", "", schemaWithoutControllerState("tenant-a", "second")),
			}},
			want: "resourceVersion changed across pages",
		},
		{
			name: "duplicate object name",
			client: &schemaListClient{pages: []*unstructured.UnstructuredList{
				schemaStatePage("10", "next", schemaWithoutControllerState("tenant-a", "same")),
				schemaStatePage("10", "", schemaWithoutControllerState("tenant-a", "same")),
			}},
			want: "tenant-a/same more than once",
		},
		{
			name: "duplicate object UID",
			client: &schemaListClient{pages: []*unstructured.UnstructuredList{
				schemaStatePage("10", "next", schemaWithoutControllerState("tenant-a", "first")),
				schemaStatePage("10", "", schemaWithUID("tenant-a", "second", "uid-tenant-a-first")),
			}},
			want: "share UID uid-tenant-a-first",
		},
		{
			name: "repeated continue token",
			client: &schemaListClient{pages: []*unstructured.UnstructuredList{
				schemaStatePage("10", "next", schemaWithoutControllerState("tenant-a", "first")),
				schemaStatePage("10", "next", schemaWithoutControllerState("tenant-a", "second")),
			}},
			want: "repeated continue token",
		},
		{
			name: "negative remaining count",
			client: &schemaListClient{pages: []*unstructured.UnstructuredList{
				schemaStatePageWithRemaining("10", "", -1),
			}},
			want: "negative remainingItemCount -1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := VerifyStoredControllerState(context.Background(), storedStateClientsWithSchemas(test.client), 1)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyStoredControllerState error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRuntimeVerifierRejectsMalformedStoredControllerState(t *testing.T) {
	verifier := readyRuntimeVerifier(t)
	state := storedStateClientsWithSchemas(&schemaListClient{pages: []*unstructured.UnstructuredList{{
		Items: []unstructured.Unstructured{schemaWithControllerState("tenant-a", "schema-a", "future")},
	}}})
	verifier.StoredState = &state
	verifier.SupportedControllerStateVersion = 1
	err := verifier.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "malformed stored controller state") {
		t.Fatalf("Verify error = %v, want malformed-state refusal", err)
	}
}

func TestVerifyStoredControllerStateScansImmutablePlanAndApprovalSpecs(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		version   any
		configure func(*StoredControllerStateClients, *schemaListClient)
		want      string
	}{
		{
			name:    "future plan state",
			kind:    "PtahSchemaPlan",
			version: int64(2),
			configure: func(clients *StoredControllerStateClients, client *schemaListClient) {
				clients.Plans = client
			},
			want: "controller downgrade refused",
		},
		{
			name:    "negative approval state",
			kind:    "PtahSchemaApproval",
			version: int64(-1),
			configure: func(clients *StoredControllerStateClients, client *schemaListClient) {
				clients.Approvals = client
			},
			want: "invalid stored controller state version -1",
		},
		{
			name:    "malformed approval state",
			kind:    "PtahSchemaApproval",
			version: "future",
			configure: func(clients *StoredControllerStateClients, client *schemaListClient) {
				clients.Approvals = client
			},
			want: "malformed stored controller state",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := schemaWithControllerStateAt("tenant-a", "object-a", test.version, "spec", "controllerStateVersion")
			client := &schemaListClient{pages: []*unstructured.UnstructuredList{{Items: []unstructured.Unstructured{object}}}}
			clients := emptyStoredStateClients()
			test.configure(&clients, client)

			err := VerifyStoredControllerState(context.Background(), clients, 1)
			if err == nil || !strings.Contains(err.Error(), test.want) ||
				!strings.Contains(err.Error(), test.kind+" tenant-a/object-a") ||
				!strings.Contains(err.Error(), "spec.controllerStateVersion") {
				t.Fatalf("VerifyStoredControllerState error = %v, want %s object/path diagnostics and %q", err, test.kind, test.want)
			}
		})
	}
}

func TestVerifyStoredControllerStateListsEveryDurableKind(t *testing.T) {
	schemas := &schemaListClient{pages: []*unstructured.UnstructuredList{{
		Items: []unstructured.Unstructured{schemaWithoutControllerState("tenant-a", "legacy-schema")},
	}}}
	plans := &schemaListClient{pages: []*unstructured.UnstructuredList{{
		Items: []unstructured.Unstructured{schemaWithoutControllerState("tenant-a", "legacy-plan")},
	}}}
	approvals := &schemaListClient{pages: []*unstructured.UnstructuredList{{
		Items: []unstructured.Unstructured{
			schemaWithControllerStateAt("tenant-a", "legacy-approval", int64(0), "spec", "controllerStateVersion"),
		},
	}}}
	clients := StoredControllerStateClients{Schemas: schemas, Plans: plans, Approvals: approvals}

	if err := VerifyStoredControllerState(context.Background(), clients, 1); err != nil {
		t.Fatal(err)
	}
	for name, client := range map[string]*schemaListClient{
		"PtahSchema": schemas, "PtahSchemaPlan": plans, "PtahSchemaApproval": approvals,
	} {
		if len(client.options) != 1 || client.options[0].Limit != storedControllerStatePageSize {
			t.Fatalf("%s List options = %#v, want one exhaustive paginated scan", name, client.options)
		}
	}
}

func schemaWithoutControllerState(namespace, name string) unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"namespace": namespace, "name": name},
	}}
	object.SetUID(types.UID("uid-" + namespace + "-" + name))
	return object
}

func schemaWithUID(namespace, name, uid string) unstructured.Unstructured {
	object := schemaWithoutControllerState(namespace, name)
	object.SetUID(types.UID(uid))
	return object
}

func schemaStatePage(resourceVersion, continueToken string, items ...unstructured.Unstructured) *unstructured.UnstructuredList {
	page := &unstructured.UnstructuredList{Items: items}
	page.SetResourceVersion(resourceVersion)
	page.SetContinue(continueToken)
	return page
}

func schemaStatePageWithRemaining(resourceVersion, continueToken string, remaining int64) *unstructured.UnstructuredList {
	page := schemaStatePage(resourceVersion, continueToken)
	page.SetRemainingItemCount(&remaining)
	return page
}

func schemaWithControllerState(namespace, name string, version any) unstructured.Unstructured {
	return schemaWithControllerStateAt(namespace, name, version, "status", "executionBinding", "controllerStateVersion")
}

func schemaWithControllerStateAt(namespace, name string, version any, path ...string) unstructured.Unstructured {
	object := schemaWithoutControllerState(namespace, name)
	setControllerStateAt(&object, version, path...)
	return object
}

func setControllerStateAt(object *unstructured.Unstructured, version any, path ...string) {
	if err := unstructured.SetNestedField(object.Object, version, path...); err != nil {
		panic(err)
	}
}

func emptyStoredStateClients() StoredControllerStateClients {
	return StoredControllerStateClients{
		Schemas:   &schemaListClient{},
		Plans:     &schemaListClient{},
		Approvals: &schemaListClient{},
	}
}

func storedStateClientsWithSchemas(schemas ControllerStateListClient) StoredControllerStateClients {
	clients := emptyStoredStateClients()
	clients.Schemas = schemas
	return clients
}

func readyRuntimeVerifier(t *testing.T) *RuntimeVerifier {
	t.Helper()
	candidates := mustCandidates(t)
	crdManager := &Manager{Client: &memoryClient{objects: readyObjects(candidates)}, PollInterval: time.Millisecond}
	expected := RuntimeInvariants{
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		CoordinationNamespace:        "ptah-locks",
		LeaderElection:               true,
		LeaderElectionID:             "ptah-operator.operator.ptah.dev",
		WebhookServiceName:           "ptah-webhook",
		WebhookTimeoutSeconds:        5,
		HookServiceAccountName:       "ptah-crd-manager",
		ControllerServiceAccountName: "ptah-controller",
		ControllerDeploymentName:     "ptah-controller",
		CertificateDeploymentName:    "ptah-cert-rotator",
		ControllerStateVersion:       1,
		AdmissionContractVersion:     CurrentAdmissionContractVersion,
		ReleaseSequence:              1,
	}
	annotations := expected.annotations()
	mutatingClient := &mutatingAdmissionClient{object: &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: AdmissionConfigurationName, Annotations: copyStrings(annotations)},
		Webhooks:   []admissionregistrationv1.MutatingWebhook{readyMutatingApprovalWebhook(expected)},
	}}
	validatingClient := &validatingAdmissionClient{object: &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: AdmissionConfigurationName, Annotations: copyStrings(annotations)},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			readyValidatingApprovalWebhook(expected),
			readyPodIntentWebhook(expected),
			readyControllerWriteWebhook(expected),
		},
	}}
	return &RuntimeVerifier{
		CRDs: crdManager, Mutating: mutatingClient, Validating: validatingClient,
		Expected: expected, PollEvery: time.Millisecond,
	}
}

func readyMutatingApprovalWebhook(expected RuntimeInvariants) admissionregistrationv1.MutatingWebhook {
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	equivalent := admissionregistrationv1.Equivalent
	never := admissionregistrationv1.NeverReinvocationPolicy
	return admissionregistrationv1.MutatingWebhook{
		Name: mutatingApprovalWebhookName, AdmissionReviewVersions: []string{"v1"},
		FailurePolicy: &fail, SideEffects: &none, MatchPolicy: &equivalent,
		ReinvocationPolicy: &never, TimeoutSeconds: valuePointer(expected.WebhookTimeoutSeconds),
		ClientConfig: readyWebhookClientConfig(expected, mutatingApprovalPath),
		Rules:        approvalRules([]admissionregistrationv1.OperationType{admissionregistrationv1.Create}),
	}
}

func readyValidatingApprovalWebhook(expected RuntimeInvariants) admissionregistrationv1.ValidatingWebhook {
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	equivalent := admissionregistrationv1.Equivalent
	return admissionregistrationv1.ValidatingWebhook{
		Name: validatingApprovalWebhookName, AdmissionReviewVersions: []string{"v1"},
		FailurePolicy: &fail, SideEffects: &none, MatchPolicy: &equivalent,
		TimeoutSeconds: valuePointer(expected.WebhookTimeoutSeconds),
		ClientConfig:   readyWebhookClientConfig(expected, validatingApprovalPath),
		Rules: approvalRules([]admissionregistrationv1.OperationType{
			admissionregistrationv1.Create, admissionregistrationv1.Update,
		}),
	}
}

func readyPodIntentWebhook(expected RuntimeInvariants) admissionregistrationv1.ValidatingWebhook {
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	equivalent := admissionregistrationv1.Equivalent
	scope := admissionregistrationv1.NamespacedScope
	return admissionregistrationv1.ValidatingWebhook{
		Name: podIntentWebhookName, AdmissionReviewVersions: []string{"v1"},
		FailurePolicy: &fail, SideEffects: &none, MatchPolicy: &equivalent,
		TimeoutSeconds: valuePointer(expected.WebhookTimeoutSeconds),
		ClientConfig:   readyWebhookClientConfig(expected, podIntentPath),
		ObjectSelector: exactPodIntentObjectSelector(),
		Rules: []admissionregistrationv1.RuleWithOperations{{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{""}, APIVersions: []string{"v1"},
				Resources: []string{"pods", "pods/ephemeralcontainers", "pods/resize"}, Scope: &scope,
			},
		}},
		MatchConditions: []admissionregistrationv1.MatchCondition{{
			Name: podIntentMatchConditionName, Expression: podIntentMatchExpression,
		}},
	}
}

func readyControllerWriteWebhook(expected RuntimeInvariants) admissionregistrationv1.ValidatingWebhook {
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	exact := admissionregistrationv1.Exact
	scope := admissionregistrationv1.NamespacedScope
	return admissionregistrationv1.ValidatingWebhook{
		Name: controllerWriteWebhookName, AdmissionReviewVersions: []string{"v1"},
		FailurePolicy: &fail, SideEffects: &none, MatchPolicy: &exact,
		TimeoutSeconds: valuePointer(controllerWriteWebhookTimeoutSeconds),
		ClientConfig:   readyWebhookClientConfig(expected, controllerWritePath),
		Rules: []admissionregistrationv1.RuleWithOperations{
			{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
				Rule:       admissionregistrationv1.Rule{APIGroups: []string{"batch"}, APIVersions: []string{"v1"}, Resources: []string{"jobs"}, Scope: &scope},
			},
			{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"}, Scope: &scope},
			},
			{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule:       admissionregistrationv1.Rule{APIGroups: []string{"operator.ptah.dev"}, APIVersions: []string{"v1alpha1"}, Resources: []string{"ptahschemaplans"}, Scope: &scope},
			},
		},
		MatchConditions: []admissionregistrationv1.MatchCondition{{
			Name: controllerWriteMatchConditionName,
			Expression: fmt.Sprintf(
				"request.userInfo.username == 'system:serviceaccount:%s:%s'",
				expected.ReleaseNamespace,
				expected.ControllerServiceAccountName,
			),
		}},
	}
}

func readyWebhookClientConfig(expected RuntimeInvariants, path string) admissionregistrationv1.WebhookClientConfig {
	return admissionregistrationv1.WebhookClientConfig{
		CABundle: []byte("test CA"),
		Service: &admissionregistrationv1.ServiceReference{
			Namespace: expected.ReleaseNamespace,
			Name:      expected.WebhookServiceName,
			Path:      valuePointer(path),
			Port:      valuePointer(int32(443)),
		},
	}
}

func approvalRules(operations []admissionregistrationv1.OperationType) []admissionregistrationv1.RuleWithOperations {
	scope := admissionregistrationv1.NamespacedScope
	return []admissionregistrationv1.RuleWithOperations{{
		Operations: operations,
		Rule: admissionregistrationv1.Rule{
			APIGroups: []string{"operator.ptah.dev"}, APIVersions: []string{"v1alpha1"},
			Resources: []string{"ptahschemaapprovals"}, Scope: &scope,
		},
	}}
}

func validatingWebhook(t *testing.T, verifier *RuntimeVerifier, name string) *admissionregistrationv1.ValidatingWebhook {
	t.Helper()
	webhooks := verifier.Validating.(*validatingAdmissionClient).object.Webhooks
	for i := range webhooks {
		if webhooks[i].Name == name {
			return &webhooks[i]
		}
	}
	t.Fatalf("validating webhook %s is missing", name)
	return nil
}

func valuePointer[T any](value T) *T {
	return &value
}

func copyStrings(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

type mutatingAdmissionClient struct {
	object        *admissionregistrationv1.MutatingWebhookConfiguration
	onGet         func()
	dryRunUpdates int
	realUpdates   int
	dryRunError   error
}

func (c *mutatingAdmissionClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	if c.onGet != nil {
		onGet := c.onGet
		c.onGet = nil
		onGet()
	}
	if c.object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "mutatingwebhookconfigurations"}, name)
	}
	return c.object.DeepCopy(), nil
}

func (c *mutatingAdmissionClient) Update(_ context.Context, object *admissionregistrationv1.MutatingWebhookConfiguration, options metav1.UpdateOptions) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	if len(options.DryRun) != 0 {
		c.dryRunUpdates++
		if c.dryRunError != nil {
			return nil, c.dryRunError
		}
		return object.DeepCopy(), nil
	}
	c.realUpdates++
	c.object = object.DeepCopy()
	return c.object.DeepCopy(), nil
}

type validatingAdmissionClient struct {
	object        *admissionregistrationv1.ValidatingWebhookConfiguration
	calls         int
	onGet         func(int)
	dryRunUpdates int
	realUpdates   int
	dryRunError   error
}

func (c *validatingAdmissionClient) Update(_ context.Context, object *admissionregistrationv1.ValidatingWebhookConfiguration, options metav1.UpdateOptions) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	if len(options.DryRun) != 0 {
		c.dryRunUpdates++
		if c.dryRunError != nil {
			return nil, c.dryRunError
		}
		return object.DeepCopy(), nil
	}
	c.realUpdates++
	c.object = object.DeepCopy()
	return c.object.DeepCopy(), nil
}

func (c *validatingAdmissionClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	c.calls++
	if c.onGet != nil {
		c.onGet(c.calls)
	}
	if c.object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingwebhookconfigurations"}, name)
	}
	return c.object.DeepCopy(), nil
}

var _ MutatingWebhookClient = (*mutatingAdmissionClient)(nil)
var _ ValidatingWebhookClient = (*validatingAdmissionClient)(nil)
var _ MutatingWebhookUpdater = (*mutatingAdmissionClient)(nil)
var _ ValidatingWebhookUpdater = (*validatingAdmissionClient)(nil)

type schemaListClient struct {
	pages                        []*unstructured.UnstructuredList
	options                      []metav1.ListOptions
	preserveEmptyResourceVersion bool
}

func (c *schemaListClient) List(_ context.Context, options metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	c.options = append(c.options, options)
	index := len(c.options) - 1
	if index >= len(c.pages) {
		result := &unstructured.UnstructuredList{}
		if !c.preserveEmptyResourceVersion {
			result.SetResourceVersion("fixture-resource-version")
		}
		return result, nil
	}
	if c.pages[index] == nil {
		return nil, nil
	}
	result := c.pages[index].DeepCopy()
	if result.GetResourceVersion() == "" && !c.preserveEmptyResourceVersion {
		result.SetResourceVersion("fixture-resource-version")
	}
	return result, nil
}

var _ ControllerStateListClient = (*schemaListClient)(nil)
