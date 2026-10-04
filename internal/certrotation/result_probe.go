package certrotation

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"
)

func (r *ResultRotator) probeState(ctx context.Context, st resultJournal, projection map[string][]byte) error {
	_, current, err := r.inspectKeys(st.Current)
	if err != nil {
		return err
	}
	authorities := []certificateMaterial{current}
	if st.Next != nil && st.Phase != "leaf" {
		_, next, err := r.inspectKeys(*st.Next)
		if err != nil {
			return err
		}
		if st.Phase == "retire" {
			authorities = []certificateMaterial{next}
		} else {
			authorities = append(authorities, next)
		}
	}
	return r.probe(ctx, projection, authorities)
}

// Probe every ready endpoint with a certificate under each still-valid client
// CA. A HEAD without an operation identity must reach the receiver's HTTP 401:
// finishing a TLS 1.3 client handshake alone does not prove the server accepted
// the client certificate. The exact served leaf is checked as well as its chain.
func (r *ResultRotator) probeResultProjection(ctx context.Context, projection map[string][]byte, authorities []certificateMaterial) error {
	leaf, _, err := parseSingleCertificate(projection["tls.crt"])
	if err != nil {
		return errors.New("invalid result probe server certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(projection["ca.crt"]) {
		return errors.New("invalid result probe server trust")
	}
	var clients []tls.Certificate
	for _, ca := range authorities {
		if !certificateCurrentlyValid(ca.ca, r.now()) {
			continue
		}
		c, err := r.probeCredential(ca)
		if err != nil {
			return err
		}
		clients = append(clients, c)
	}
	if len(clients) == 0 {
		return errors.New("result probe has no valid client authority")
	}
	probeCtx, cancel := context.WithTimeout(ctx, r.config.ProbeTimeout)
	defer cancel()
	ticker := time.NewTicker(r.config.ProbeInterval)
	defer ticker.Stop()
	endpoints := &Rotator{client: r.client, config: r.config.Config}
	for {
		before, err := endpoints.endpointSnapshot(probeCtx)
		if err == nil {
			for _, endpoint := range before {
				for _, credential := range clients {
					err = probeResultEndpoint(probeCtx, net.JoinHostPort(endpoint.address, fmt.Sprint(endpoint.port)), r.config.ServiceName+"."+r.config.Namespace+".svc", leaf, roots, credential)
					if err != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			if err == nil {
				after, readErr := endpoints.endpointSnapshot(probeCtx)
				if readErr == nil && slices.Equal(before, after) {
					return nil
				}
			}
		}
		select {
		case <-probeCtx.Done():
			return errors.New("result receiver endpoints did not converge before the probe deadline")
		case <-ticker.C:
		}
	}
}
func (r *ResultRotator) probeCredential(ca certificateMaterial) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), r.random)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial(r.random)
	if err != nil {
		return tls.Certificate{}, err
	}
	notAfter := minTime(r.now().Add(r.config.ProbeTimeout+time.Minute), ca.ca.NotAfter)
	cert := &x509.Certificate{SerialNumber: serial, NotBefore: r.now().Add(-time.Minute), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(r.random, cert, ca.ca, key.Public(), ca.caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
func probeResultEndpoint(ctx context.Context, address, serverName string, leaf *x509.Certificate, roots *x509.CertPool, credential tls.Certificate) error {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, RootCAs: roots, Certificates: []tls.Certificate{credential}, VerifyConnection: func(s tls.ConnectionState) error {
		if len(s.PeerCertificates) != 1 || !bytes.Equal(s.PeerCertificates[0].Raw, leaf.Raw) {
			return errors.New("result receiver serves another certificate")
		}
		return nil
	}}
	transport := &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+serverName+"/", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("result receiver TLS probe failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return errors.New("result receiver did not refuse the non-operation probe")
	}
	return nil
}
