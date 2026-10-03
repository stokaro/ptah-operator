// Package resultservice connects durable delivery to the manager lifecycle.
// Trust is provisioned separately and reloaded from bounded mounted files.
package resultservice

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Config struct {
	Endpoint, Address, CertificateDirectory         string
	Uploads                                         int
	UploadTimeout                                   time.Duration
	Consumer                                        resultconsumer.Options
	ReloadInterval                                  time.Duration
	EnrollmentPolicyNamespace, EnrollmentPolicyName string
	// TokenReviews selects receiver-audience Pod authentication. The manager
	// supplies its API client; runners receive no API permissions.
	TokenReviews resultauthority.TokenReviewer
}

type Service struct {
	Consumer        *resultconsumer.Reader
	server          *http.Server
	address         string
	ready           atomic.Bool
	started         atomic.Bool
	trust           atomic.Pointer[trustSnapshot]
	reloadFailed    atomic.Bool
	enrollmentReady atomic.Bool
	reloadMu        sync.Mutex
	config          Config
	writer          client.Client
	reader          client.Reader
}

// New never generates or fetches trust through the Kubernetes API. Both replicas
// must mount the same dedicated client signing authority and overlapping trust.
// Required files: tls.crt, tls.key, ca.crt (server trust), client-ca.crt,
// client-ca.key, and client-trust.crt (current and overlapping client signers).
func New(config Config, writer client.Client, reader client.Reader) (*Service, error) {
	if writer == nil || reader == nil || config.CertificateDirectory == "" || jobconfig.ValidateEndpoint(config.Endpoint) != nil {
		return nil, errors.New("invalid result service configuration")
	}
	if _, _, err := net.SplitHostPort(config.Address); err != nil {
		return nil, errors.New("invalid result service listen address")
	}
	if (config.EnrollmentPolicyName == "") != (config.EnrollmentPolicyNamespace == "") {
		return nil, errors.New("result enrollment policy needs both namespace and name")
	}
	if config.ReloadInterval == 0 {
		config.ReloadInterval = 5 * time.Second
	}
	if config.ReloadInterval < time.Millisecond || config.ReloadInterval > time.Minute {
		return nil, errors.New("invalid result trust reload interval")
	}
	service := &Service{address: config.Address, config: config, writer: writer, reader: reader}
	if err := service.reload(); err != nil {
		return nil, err
	}
	service.enrollmentReady.Store(config.EnrollmentPolicyName == "")
	trust := service.trust.Load()
	store := resultstore.Store{Client: writer, Reader: reader}
	receiverConfig := resultdelivery.ReceiverConfig{Store: store, Authorize: (resultauthority.Authorizer{Reader: reader}).Check, VerifyClient: service.verifyClient, MaxConcurrent: config.Uploads, Timeout: config.UploadTimeout}
	if config.TokenReviews != nil {
		receiverConfig.VerifyClient = nil
		receiverConfig.AuthenticateToken = (resultauthority.TokenVerifier{Reviews: config.TokenReviews, Reader: reader}).Verify
		receiverConfig.Authorize = func(ctx context.Context, identity resultdelivery.Identity) error {
			pinned, err := service.AuthorizePublication(ctx, identity.Binding)
			if err != nil {
				return err
			}
			if pinned != identity {
				return resultdelivery.ErrAuthority
			}
			return nil
		}
	}
	receiver, err := resultdelivery.NewReceiver(receiverConfig)
	if err != nil {
		return nil, err
	}
	server, err := receiver.Server(trust.tls.Certificates[0], trust.tls.ClientCAs)
	if err != nil {
		return nil, err
	}
	server.TLSConfig.GetConfigForClient = service.tlsConfig
	// TLS failures must not emit certificate identities or transport diagnostics.
	server.ErrorLog = log.New(io.Discard, "", 0)
	consumer, err := resultconsumer.New(resultconsumer.StoreLoader{Store: store}, config.Consumer)
	if err != nil {
		return nil, err
	}
	service.Consumer, service.server = consumer, server
	return service, nil
}

func (*Service) NeedLeaderElection() bool { return false }
func (s *Service) Ready(*http.Request) error {
	if !s.ready.Load() || !s.enrollmentReady.Load() || s.reloadFailed.Load() || !time.Now().Before(s.trust.Load().notAfter) {
		return errors.New("result service is not ready")
	}
	return nil
}

// Start serves on every replica. Publication is idempotent through the API;
// neither the listener nor background reading depends on leader election.
func (s *Service) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("result service cannot be restarted")
	}
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return errors.New("result service listener could not start")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.server.BaseContext = func(net.Listener) context.Context { return runCtx }
	s.address = listener.Addr().String()
	consumerDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { consumerDone <- s.Consumer.Start(runCtx) }()
	go func() { serverDone <- s.server.ServeTLS(listener, "", "") }()
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		ticker := time.NewTicker(s.config.ReloadInterval)
		defer ticker.Stop()
		for {
			s.refreshTrust(runCtx)
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:

			}
		}
	}()
	s.ready.Store(true)
	var failure error
	serverFinished, consumerFinished := false, false
	select {
	case <-ctx.Done():
	case <-serverDone:
		serverFinished = true
		if ctx.Err() == nil {
			failure = errors.New("result service listener stopped")
		}
	case <-consumerDone:
		consumerFinished = true
		if ctx.Err() == nil {
			failure = errors.New("result consumer stopped")
		}
	}
	s.ready.Store(false)
	cancel()
	// Cancel active API operations and close idle and active TLS connections.
	// A canceled upload cannot turn partial publication into an acknowledgment.
	_ = s.server.Close()
	if !serverFinished {
		<-serverDone
	}
	if !consumerFinished {
		<-consumerDone
	}
	<-reloadDone
	return failure
}
