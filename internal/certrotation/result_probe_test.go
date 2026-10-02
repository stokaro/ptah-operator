package certrotation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

type noProbePublication struct{ t *testing.T }

func (p noProbePublication) PublishAuthorized(context.Context, resultstore.Binding, []byte, string, func(context.Context) error) (resultstore.Receipt, error) {
	p.t.Error("certificate probe attempted publication")
	return resultstore.Receipt{}, errors.New("probe cannot publish")
}

func TestResultRotationProbesEveryClientCAOverRealTLS(t *testing.T) {
	for _, scenario := range []string{"both client roots", "missing new client root", "wrong server certificate", "not ready", "changed endpoint identity"} {
		t.Run(scenario, func(t *testing.T) {
			f := newResultRotationFixture(t)
			old, err := f.r.generateKeys()
			if err != nil {
				t.Fatal(err)
			}
			next, err := f.r.generateKeys()
			if err != nil {
				t.Fatal(err)
			}
			st := resultJournal{Phase: "prepare", Current: old, Next: &next}
			desired, _ := st.projections()
			cert, err := tls.X509KeyPair(old.ServerCertificate, old.ServerCertificateKey)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "wrong server certificate" {
				cert, err = tls.X509KeyPair(next.ServerCertificate, next.ServerCertificateKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(old.ClientCA)
			if scenario != "missing new client root" {
				roots.AppendCertsFromPEM(next.ClientCA)
			}
			receiver, err := resultdelivery.NewReceiver(resultdelivery.ReceiverConfig{Store: noProbePublication{t}, Authorize: func(context.Context, resultdelivery.Identity) error {
				t.Error("non-operation probe reached authorization")
				return resultdelivery.ErrAuthority
			}, MaxConcurrent: 1, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			httpServer, err := receiver.Server(cert, roots)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(receiver)
			server.TLS = httpServer.TLSConfig
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.StartTLS()
			defer server.Close()
			address := server.Listener.Addr().(*net.TCPAddr)
			slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: "results-1", Labels: map[string]string{discoveryv1.LabelServiceName: "results"}}, AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Name: ptr.To("https"), Port: ptr.To(int32(address.Port))}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{address.IP.String()}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(scenario != "not ready")}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "system", Name: "manager", UID: "manager-uid"}}}}
			if _, err := f.api.DiscoveryV1().EndpointSlices("system").Create(t.Context(), slice, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if scenario == "changed endpoint identity" {
				var calls int
				f.api.PrependReactor("list", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
					calls++
					got := slice.DeepCopy()
					got.Endpoints[0].TargetRef.UID = types.UID(fmt.Sprintf("pod-%d", calls))
					return true, &discoveryv1.EndpointSliceList{Items: []discoveryv1.EndpointSlice{*got}}, nil
				})
			}
			_, oldCA, _ := f.r.inspectKeys(old)
			_, nextCA, _ := f.r.inspectKeys(next)
			err = f.r.probeResultProjection(t.Context(), desired, []certificateMaterial{oldCA, nextCA})
			if scenario == "both client roots" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("incomplete endpoint convergence accepted")
			}
		})
	}
}
