package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func firstReview(uid, pod, digest string) admissionv1.AdmissionReview {
	manifest, _ := json.Marshal(map[string]string{"digest": digest})
	object, _ := json.Marshal(map[string]any{"metadata": map[string]string{"name": "original", "namespace": "first-test"}, "spec": map[string]any{"type": "intent", "data": manifest}})
	return admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{UID: types.UID(uid), Namespace: "first-test", Operation: admissionv1.Create, Resource: metav1.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahresultrecords"}, UserInfo: authenticationv1.UserInfo{Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {pod}}}, Object: runtime.RawExtension{Raw: object}}}
}

func barrierRequest(p *resultACKProxy, ctx context.Context, review admissionv1.AdmissionReview) *httptest.ResponseRecorder {
	body, _ := json.Marshal(review)
	response := httptest.NewRecorder()
	p.publicationBarrier(response, httptest.NewRequest(http.MethodPost, "/first-publication", bytes.NewReader(body)).WithContext(ctx))
	return response
}

func TestFirstPublicationRequiresBothManagerAdmissions(t *testing.T) {
	f := newACKFixture(t)
	f.proxy.evidence.FirstGateEnabled = true
	f.proxy.firstNamespace = "first-test"
	if f.admin(http.MethodPost, "/resume-first").Code != http.StatusConflict {
		t.Fatal("resumed before the first request")
	}
	done := make(chan error, 1)
	go func() {
		r, err := f.put(f.body)
		if r != nil {
			r.Body.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for f.admin(http.MethodGet, "/pending-first").Code != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("original upload did not pause")
		}
		time.Sleep(time.Millisecond)
	}
	var pending firstPending
	if err := json.Unmarshal(f.admin(http.MethodGet, "/pending-first").Body.Bytes(), &pending); err != nil || !bytes.Equal(pending.Payload, f.body) || pending.Digest != digest(f.body) || pending.Path != "/v1/results/original" {
		t.Fatal("pending original bytes changed")
	}
	if f.calls.Load() != 0 {
		t.Fatal("first upload reached storage before the gate")
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- barrierRequest(f.proxy, t.Context(), firstReview("one", "manager-a", pending.Digest)) }()
	for {
		f.proxy.mu.Lock()
		count := len(f.proxy.evidence.FirstAdmissions)
		f.proxy.mu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first manager did not arrive")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-first:
		t.Fatal("first manager escaped alone")
	default:
	}
	if f.admin(http.MethodPost, "/resume-first").Code != http.StatusConflict {
		t.Fatal("original upload resumed before both admissions")
	}
	duplicate := barrierRequest(f.proxy, t.Context(), firstReview("duplicate", "manager-a", pending.Digest))
	var refused admissionv1.AdmissionReview
	if json.Unmarshal(duplicate.Body.Bytes(), &refused) != nil || refused.Response.Allowed {
		t.Fatal("one manager satisfied both sides of the barrier")
	}
	second := barrierRequest(f.proxy, t.Context(), firstReview("two", "manager-b", pending.Digest))
	for _, response := range []*httptest.ResponseRecorder{<-first, second} {
		var review admissionv1.AdmissionReview
		if json.Unmarshal(response.Body.Bytes(), &review) != nil || review.Response == nil || !review.Response.Allowed {
			t.Fatal("both manager writes were not released")
		}
	}
	f.proxy.mu.Lock()
	a, b := f.proxy.evidence.FirstAdmissions[0], f.proxy.evidence.FirstAdmissions[1]
	f.proxy.mu.Unlock()
	if a.ReleasedAt == nil || b.ReleasedAt == nil || a.ReleasedAt.Before(b.ArrivedAt) || b.ReleasedAt.Before(a.ArrivedAt) {
		t.Fatal("admissions did not overlap")
	}
	for range 2 {
		if f.admin(http.MethodPost, "/resume-first").Code != http.StatusNoContent {
			t.Fatal("first release was not idempotent")
		}
	}
	if err := <-done; err == nil || f.calls.Load() != 1 {
		t.Fatal("original delivery did not retain lost-ACK behavior")
	}
	retry := barrierRequest(f.proxy, t.Context(), firstReview("retry", "manager-a", pending.Digest))
	var admitted admissionv1.AdmissionReview
	if json.Unmarshal(retry.Body.Bytes(), &admitted) != nil || !admitted.Response.Allowed {
		t.Fatal("removed barrier blocked subsequent publication admission")
	}
	if f.admin(http.MethodGet, "/pending-first").Code != http.StatusNotFound {
		t.Fatal("fixture retained the released payload")
	}
	if bytes.Contains(f.admin(http.MethodGet, "/evidence").Body.Bytes(), f.body) {
		t.Fatal("ordinary evidence contains payload")
	}
}

func TestFirstPublicationRefusesForeignOrCanceledAdmission(t *testing.T) {
	for _, mutate := range []func(*admissionv1.AdmissionReview){
		func(r *admissionv1.AdmissionReview) { r.Request.Namespace = "other" },
		func(r *admissionv1.AdmissionReview) { r.Request.Resource.Group = "other" },
		func(r *admissionv1.AdmissionReview) { r.Request.Operation = admissionv1.Update },
		func(r *admissionv1.AdmissionReview) { r.Request.UID = "" },
		func(r *admissionv1.AdmissionReview) { r.Request.UserInfo.Extra = nil },
	} {
		p := newResultACKProxy(nil, "", nil)
		p.firstNamespace = "first-test"
		p.pending = &firstPending{Path: "/v1/results/original", Digest: "expected"}
		review := firstReview("one", "manager-a", "expected")
		mutate(&review)
		response := barrierRequest(p, t.Context(), review)
		var result admissionv1.AdmissionReview
		if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Response == nil || result.Response.Allowed || len(p.evidence.FirstAdmissions) != 0 {
			t.Fatal("foreign admission entered the barrier")
		}
	}
	p := newResultACKProxy(nil, "", nil)
	p.firstNamespace = "first-test"
	p.pending = &firstPending{Path: "/v1/results/original", Digest: "expected"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response := barrierRequest(p, ctx, firstReview("one", "manager-a", "expected"))
	var result admissionv1.AdmissionReview
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Response.Allowed || p.evidence.FirstAdmissions[0].ReleasedAt != nil {
		t.Fatal("canceled admission looked released")
	}
}
