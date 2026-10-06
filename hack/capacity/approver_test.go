package main

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestCapacityApproverUsesASeparateAuthenticatedClient(t *testing.T) {
	for _, mode := range []string{"distinct", "same user", "missing user", "review refused", "other cluster"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				approver := r.Header.Get("Authorization") == "Bearer approval-token"
				if strings.HasSuffix(r.URL.Path, "/selfsubjectreviews") {
					if mode == "review refused" && approver {
						http.Error(w, "review refused", http.StatusForbidden)
						return
					}
					username := "workload-writer"
					if approver && mode != "same user" {
						username = "approval-writer"
					}
					if mode == "missing user" && approver {
						username = ""
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview", "status": map[string]any{"userInfo": map[string]any{"username": username}}})
					return
				}
				if !approver || r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "approvals") {
					t.Error("approval used the workload credentials or an unexpected route")
					http.Error(w, "approval refused", http.StatusForbidden)
					return
				}
				var object unstructured.Unstructured
				if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
					t.Error(err)
					return
				}
				object.SetUID("approved-uid")
				_ = json.NewEncoder(w).Encode(&object)
			}))
			defer server.Close()
			authority := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			base := &rest.Config{Host: server.URL, BearerToken: "workload-token", QPS: 50, Burst: 100,
				TLSClientConfig: rest.TLSClientConfig{CAData: authority}}
			fallback, err := dynamic.NewForConfig(base)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := server.URL
			if mode == "other cluster" {
				endpoint += "/other"
			}
			config := clientcmdapi.Config{
				Clusters:  map[string]*clientcmdapi.Cluster{"lab": {Server: endpoint, CertificateAuthorityData: authority}},
				AuthInfos: map[string]*clientcmdapi.AuthInfo{"approver": {Token: "approval-token"}},
				Contexts:  map[string]*clientcmdapi.Context{"lab": {Cluster: "lab", AuthInfo: "approver"}}, CurrentContext: "lab",
			}
			path := filepath.Join(t.TempDir(), "approver-kubeconfig")
			if err := clientcmd.WriteToFile(config, path); err != nil {
				t.Fatal(err)
			}
			writer, actors, err := capacityApprover(t.Context(), path, base, fallback)
			if mode != "distinct" {
				if err == nil || writer != nil || actors != nil {
					t.Fatal("accepted an unproven distinct approver", actors, err)
				}
				return
			}
			if err != nil || actors == nil || actors.WorkloadWriter != "workload-writer" || actors.Approver != "approval-writer" {
				t.Fatal(actors, err)
			}
			steps := &scenarios{dynamic: fallback, approver: writer}
			for _, family := range []string{"schema", "migration"} {
				object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaApproval", "metadata": map[string]any{"name": "approval", "namespace": "work"}}}
				if family == "migration" {
					object.SetKind("PtahMigrationApproval")
				}
				got, err := steps.approvalWriter().Resource(backlogApprovalResource(family)).Namespace("work").Create(t.Context(), object, metav1.CreateOptions{})
				if err != nil || got.GetUID() != "approved-uid" {
					t.Fatal(got, err)
				}
			}
		})
	}
}

func TestCapacityWithoutAnApproverKeepsTheExistingWriter(t *testing.T) {
	writer := fake.NewSimpleDynamicClient(runtime.NewScheme())
	got, actors, err := capacityApprover(t.Context(), "", nil, writer)
	if err != nil || actors != nil || got != writer || (&scenarios{dynamic: writer}).approvalWriter() != writer {
		t.Fatal("changed the existing lab client", actors, err)
	}
}

func TestRetentionApprovalDoesNotUseTheWorkloadWriter(t *testing.T) {
	raw, err := os.ReadFile("testdata/retention-stale-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var reading map[string]*unstructured.Unstructured
	if err := json.Unmarshal(raw, &reading); err != nil {
		t.Fatal(err)
	}
	schema := reading["schema"]
	planName, _, _ := unstructured.NestedString(schema.Object, "status", "plan", "name")
	planUID, _, _ := unstructured.NestedString(schema.Object, "status", "plan", "uid")
	plan := &unstructured.Unstructured{Object: map[string]any{"apiVersion": schema.GetAPIVersion(), "kind": "PtahSchemaPlan",
		"metadata": map[string]any{"name": planName, "namespace": schema.GetNamespace(), "uid": planUID},
		"spec":     map[string]any{"schemaRef": map[string]any{"name": schema.GetName(), "uid": string(schema.GetUID())}, "fingerprint": "original-plan"}}}
	workload := fake.NewSimpleDynamicClient(runtime.NewScheme(), plan)
	workload.PrependReactor("create", "ptahschemaapprovals", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the workload writer cannot approve its own plan")
	})
	approver := fake.NewSimpleDynamicClient(runtime.NewScheme())
	approver.PrependReactor("create", "ptahschemaapprovals", func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		object.SetUID("admitted-approval")
		object.SetCreationTimestamp(metav1.NewTime(time.Now()))
		return true, object, nil
	})
	steps := &scenarios{dynamic: workload, approver: approver}
	approval, err := steps.createFaultApproval(t.Context(), "schema", schema)
	if err != nil || approval.GetUID() != "admitted-approval" {
		t.Fatal("retention recovery did not use its distinct approver", approval, err)
	}
}
