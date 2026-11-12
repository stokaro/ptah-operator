package resultservice

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// failingJobReads answers every Job read with err once a row sets it, the way
// a throttled or unreachable API server answers the authority check, and
// passes every other read through.
type failingJobReads struct {
	client.Reader
	err atomic.Pointer[error]
}

func (r *failingJobReads) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, job := object.(*batchv1.Job); job {
		if err := r.err.Load(); err != nil {
			return *err
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

// lockedBuffer is the log sink. The receiver writes it from its own
// goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	out bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.String()
}

// deliveryMode is one way the receiver authenticates a runner: its own setup,
// and a preflight sender for the identity it pinned or issued.
type deliveryMode struct {
	identity resultdelivery.Identity
	client   client.Client
	reader   *failingJobReads
	config   Config
	sender   func(*testing.T, *Service) *resultdelivery.Sender
}

func tokenDelivery(t *testing.T) deliveryMode {
	config := mountedTrust(t)
	trust, err := os.ReadFile(filepath.Join(config.CertificateDirectory, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	f := resulttest.NewPodToken(t, "schema-observe", trust)
	c := withoutSecretWrites{&identifyingAPI{Client: f.Client(t)}}
	config.TokenReviews = tokenReviewFunc(func(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
		return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{resultdelivery.TokenAudience}, User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:" + f.Pod.Namespace + ":" + f.Pod.Spec.ServiceAccountName, UID: "account-uid",
			Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {f.Pod.Name}, "authentication.kubernetes.io/pod-uid": {string(f.Pod.UID)}},
		}}}, nil
	})
	return deliveryMode{identity: f.Identity, client: c, reader: &failingJobReads{Reader: noSecrets{Reader: c}}, config: config,
		sender: func(t *testing.T, s *Service) *resultdelivery.Sender {
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(s.ServerTrust())
			sender, err := resultdelivery.NewTokenSender("https://"+s.address, f.Identity, &tls.Config{RootCAs: roots}, singleAttempt, func() (string, error) { return "bound.pod.token", nil })
			if err != nil {
				t.Fatal(err)
			}
			return sender
		}}
}

func certificateDelivery(t *testing.T) deliveryMode {
	f := resulttest.New(t, "schema-observe")
	c := &identifyingAPI{Client: f.Client(t)}
	return deliveryMode{identity: f.Identity, client: c, reader: &failingJobReads{Reader: noSecrets{Reader: c}}, config: mountedTrust(t),
		sender: func(t *testing.T, s *Service) *resultdelivery.Sender {
			issued, err := s.Ensure(t.Context(), f.Identity)
			if err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{}
			if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Job.Namespace, Name: issued.Name}, secret); err != nil {
				t.Fatal(err)
			}
			credential, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(secret.Data["ca.crt"])
			sender, err := resultdelivery.NewSender("https://"+s.address, f.Identity, &tls.Config{Certificates: []tls.Certificate{credential}, RootCAs: roots}, singleAttempt)
			if err != nil {
				t.Fatal(err)
			}
			return sender
		}}
}

// singleAttempt keeps one request per row, so a 503 leaves one line rather
// than one per retry.
var singleAttempt = resultdelivery.RetryPolicy{Attempts: 1, Interval: time.Millisecond, AttemptTimeout: time.Second, TotalTimeout: 2 * time.Second}

// A receiver answers an authorization that reached no verdict with 503, which
// tells the runner only to retry. The manager's log has to say why, through
// the encoder the manager runs, as the API status code and reason or whether
// the deadline passed, and never an error's text. A refusal is the 403 itself
// and a delivery is the receipt, so neither logs.
func TestReceiverLogsWhyAnAuthorizationReachedNoVerdict(t *testing.T) {
	const private = "private-api-server-diagnostic"
	const noVerdict = "result authorization reached no verdict"
	jobs := schema.GroupResource{Group: "batch", Resource: "jobs"}
	type row struct {
		name    string
		jobRead error
		answer  string
		want    map[string]any
	}
	rows := []row{
		{name: "Job read throttled", jobRead: apierrors.NewTooManyRequests(private, 1), answer: "503",
			want: map[string]any{"msg": noVerdict, "code": 429.0, "reason": "TooManyRequests"}},
		{name: "Job read internal error", jobRead: apierrors.NewInternalError(errors.New(private)), answer: "503",
			want: map[string]any{"msg": noVerdict, "code": 500.0, "reason": "InternalError"}},
		{name: "Job read deadline", jobRead: &url.Error{Op: "Get", URL: "https://" + private + ".invalid/", Err: context.DeadlineExceeded}, answer: "503",
			want: map[string]any{"msg": noVerdict, "deadlineExceeded": true}},
		{name: "Job read transport", jobRead: errors.New(private), answer: "503",
			want: map[string]any{"msg": noVerdict, "deadlineExceeded": false}},
		{name: "Job gone", jobRead: apierrors.NewNotFound(jobs, private), answer: "403"},
		{name: "delivered", answer: ""},
	}
	for mode, setup := range map[string]func(*testing.T) deliveryMode{"token": tokenDelivery, "certificate": certificateDelivery} {
		t.Run(mode, func(t *testing.T) {
			m := setup(t)
			preflight := func(t *testing.T, jobRead error, answer string, prepare func(*testing.T, *Service)) string {
				t.Helper()
				var out lockedBuffer
				s, err := New(m.config, m.client, m.reader)
				if err != nil {
					t.Fatal(err)
				}
				stop := runningIn(t, logf.IntoContext(t.Context(), zap.New(zap.WriteTo(&out))), s)
				defer stop()
				if _, err := s.Ensure(t.Context(), m.identity); err != nil {
					t.Fatal(err)
				}
				sender := m.sender(t, s)
				defer sender.Close()
				if prepare != nil {
					prepare(t, s)
				}
				if jobRead != nil {
					m.reader.err.Store(&jobRead)
					defer m.reader.err.Store(nil)
				}
				err = sender.Check(t.Context())
				switch {
				case answer == "" && err != nil:
					t.Fatalf("preflight failed: %v", err)
				case answer != "" && (err == nil || err.Error() != "result receiver preflight returned HTTP "+answer):
					t.Fatalf("preflight answered %v, want HTTP %s", err, answer)
				}
				return out.String()
			}
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					got := preflight(t, row.jobRead, row.answer, nil)
					holdLine(t, got, row.want, m.identity)
				})
			}
			if mode == "token" {
				// A valid claim whose pin is not recorded yet waits for it,
				// and the line says that rather than an API failure.
				t.Run("pin not recorded", func(t *testing.T) {
					got := preflight(t, nil, "503", func(t *testing.T, s *Service) {
						issued, err := s.Ensure(t.Context(), m.identity)
						if err != nil {
							t.Fatal(err)
						}
						record := &api.PtahResultRecord{ObjectMeta: metav1.ObjectMeta{Namespace: m.identity.Binding.Namespace, Name: issued.Name}}
						if err := m.client.Delete(t.Context(), record); err != nil {
							t.Fatal(err)
						}
					})
					holdLine(t, got, map[string]any{"msg": "result authorization is waiting for the Job's adoption or the Pod's pin"}, m.identity)
				})
			}
		})
	}
}

// holdLine holds the log to one line with exactly want's keys beside the
// receiver's own, or to no line when want is nil, and to no private text.
func holdLine(t *testing.T, out string, want map[string]any, identity resultdelivery.Identity) {
	t.Helper()
	if strings.Contains(out, "private") {
		t.Fatalf("the log carries the text of an API error:\n%s", out)
	}
	if want == nil {
		if out != "" {
			t.Fatalf("an answer with a verdict logged:\n%s", out)
		}
		return
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want one log line, got %d:\n%s", len(lines), out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("decode %q: %v", lines[0], err)
	}
	expected := maps.Clone(want)
	maps.Copy(expected, map[string]any{"level": "info", "logger": "result-receiver", "namespace": identity.Binding.Namespace, "pod": identity.Binding.PodName})
	delete(got, "ts")
	if !maps.Equal(got, expected) {
		t.Fatalf("log line\n got %v\nwant %v", got, expected)
	}
}
