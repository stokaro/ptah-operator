package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type firstAdmission struct {
	RequestUID    string     `json:"requestUID"`
	ManagerPodUID string     `json:"managerPodUID"`
	IntentName    string     `json:"intentName"`
	PayloadDigest string     `json:"payloadDigest"`
	ArrivedAt     time.Time  `json:"arrivedAt"`
	ReleasedAt    *time.Time `json:"releasedAt,omitempty"`
}

type firstPending struct {
	Path    string `json:"path"`
	Payload []byte `json:"payload"`
	Digest  string `json:"digest"`
}

// publicationBarrier is test-only admission on the disposable namespace. It
// releases neither intent CREATE until both receiving managers have reached it.
// Normal transport still requires the original Pod's exact mTLS identity.
func (p *resultACKProxy) publicationBarrier(w http.ResponseWriter, r *http.Request) {
	var review admissionv1.AdmissionReview
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || r.Method != http.MethodPost || r.URL.Path != "/first-publication" || json.Unmarshal(data, &review) != nil || review.Request == nil {
		http.Error(w, "invalid fixture admission", http.StatusBadRequest)
		return
	}
	request := review.Request
	reply := func(allowed bool) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"}, Response: &admissionv1.AdmissionResponse{UID: request.UID, Allowed: allowed}})
	}
	var candidate struct {
		Metadata struct{ Name, Namespace string } `json:"metadata"`
		Spec     struct {
			Type string `json:"type"`
			Data []byte `json:"data"`
		} `json:"spec"`
	}
	if json.Unmarshal(request.Object.Raw, &candidate) != nil || request.Operation != admissionv1.Create || request.Resource.Group != "operator.ptah.run" || request.Resource.Resource != "ptahresultrecords" || request.Resource.Version != "v1alpha1" || request.UID == "" || request.Namespace != p.firstNamespace || candidate.Metadata.Namespace != p.firstNamespace || candidate.Spec.Type != "intent" {
		reply(false)
		return
	}
	var manifest struct {
		Digest string `json:"digest"`
	}
	podUIDs := request.UserInfo.Extra["authentication.kubernetes.io/pod-uid"]
	if json.Unmarshal(candidate.Spec.Data, &manifest) != nil || len(podUIDs) != 1 || podUIDs[0] == "" {
		reply(false)
		return
	}
	p.mu.Lock()
	// Only the first pair is a barrier. Later retries still traverse the real
	// publication admission, including while webhook removal propagates.
	if len(p.evidence.FirstAdmissions) == 2 && p.evidence.FirstAdmissions[0].ReleasedAt != nil && p.evidence.FirstAdmissions[1].ReleasedAt != nil && candidate.Metadata.Name == p.evidence.FirstAdmissions[0].IntentName {
		p.mu.Unlock()
		reply(true)
		return
	}
	if p.pending == nil || candidate.Metadata.Name != strings.TrimPrefix(p.pending.Path, "/v1/results/") || manifest.Digest != p.pending.Digest || len(p.evidence.FirstAdmissions) >= 2 {
		p.mu.Unlock()
		reply(false)
		return
	}
	for _, previous := range p.evidence.FirstAdmissions {
		if previous.RequestUID == string(request.UID) || previous.ManagerPodUID == podUIDs[0] {
			p.mu.Unlock()
			reply(false)
			return
		}
	}
	index := len(p.evidence.FirstAdmissions)
	p.evidence.FirstAdmissions = append(p.evidence.FirstAdmissions, firstAdmission{RequestUID: string(request.UID), ManagerPodUID: podUIDs[0], IntentName: candidate.Metadata.Name, PayloadDigest: manifest.Digest, ArrivedAt: time.Now().UTC()})
	if len(p.evidence.FirstAdmissions) == 2 {
		close(p.firstAdmissions)
	}
	p.mu.Unlock()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case <-p.firstAdmissions:
		p.mu.Lock()
		now := time.Now().UTC()
		p.evidence.FirstAdmissions[index].ReleasedAt = &now
		p.mu.Unlock()
		reply(true)
	case <-timer.C:
		reply(false)
	case <-r.Context().Done():
		reply(false)
	}
}
