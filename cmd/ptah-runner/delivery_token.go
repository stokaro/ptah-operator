package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/runner"
	"k8s.io/apimachinery/pkg/types"
)

func prepareTokenDelivery(endpoint, path string, operation runner.Operation, environment []string) (*runnerDelivery, error) {
	invalid := errors.New("invalid result delivery configuration")
	if endpoint == "" || !filepath.IsAbs(path) {
		return nil, invalid
	}
	values := map[string]string{}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !(strings.HasPrefix(key, "PTAH_RESULT_") || key == runner.EnvOperationID || key == runner.EnvExpectedDatabaseEngine) {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return nil, invalid
		}
		values[key] = value
	}
	data := []byte(values[jobconfig.IdentityTemplate])
	var identity resultdelivery.Identity
	if len(data) == 0 || len(data) > 4<<10 || json.Unmarshal(data, &identity) != nil {
		return nil, invalid
	}
	canonical, _ := json.Marshal(identity)
	if !bytes.Equal(canonical, data) || identity.Binding.JobUID != "" || identity.Binding.PodName != "" || identity.Binding.PodUID != "" ||
		identity.Binding.Namespace != values[jobconfig.PodNamespace] || strconv.FormatInt(identity.Binding.Generation, 10) != values[jobconfig.Generation] ||
		identity.Binding.OperationID != values[runner.EnvOperationID] || identity.Binding.Operation != string(operation) ||
		identity.Engine != strings.ToLower(values[runner.EnvExpectedDatabaseEngine]) {
		return nil, invalid
	}
	identity.Binding.JobUID = types.UID(values[jobconfig.JobUID])
	identity.Binding.PodName = values[jobconfig.PodName]
	identity.Binding.PodUID = types.UID(values[jobconfig.PodUID])
	if _, err := resultdelivery.CertificateURI(identity); err != nil {
		return nil, invalid
	}
	trust := values[jobconfig.ServerTrust]
	roots := x509.NewCertPool()
	if len(trust) == 0 || len(trust) > 64<<10 || !roots.AppendCertsFromPEM([]byte(trust)) {
		return nil, invalid
	}
	// Reopen the projection on every request. Kubelet rotates a bound token by
	// replacing its symlink; holding an open file would retain the expired token.
	token := func() (string, error) {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 8<<10 {
			return "", invalid
		}
		f, err := os.Open(path)
		if err != nil {
			return "", invalid
		}
		defer f.Close()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 8<<10 {
			return "", invalid
		}
		data, err := io.ReadAll(io.LimitReader(f, (8<<10)+1))
		if err != nil || !resultdelivery.ValidToken(string(data)) {
			return "", invalid
		}
		return string(data), nil
	}
	if _, err := token(); err != nil {
		return nil, invalid
	}
	sender, err := resultdelivery.NewTokenSender(endpoint, identity, &tls.Config{RootCAs: roots}, deliveryRetry, token)
	if err != nil {
		return nil, invalid
	}
	return &runnerDelivery{sender: sender, identity: identity}, nil
}
