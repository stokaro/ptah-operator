package main

import (
	"errors"
	"time"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
)

// Result trust runs independently of webhook trust: during bootstrap each
// publishes the files the manager needs before waiting for its endpoints.
// A separate Lease prevents either endpoint wait from blocking the other.
type resultRotationOptions struct {
	SecretName, JournalName, PolicyName, ServiceName, LeaseName string
}

func (o resultRotationOptions) enabled() bool { return o.SecretName != "" }

func (o resultRotationOptions) validate(base certrotation.Config, supervisor supervisorConfig) error {
	count := 0
	for _, value := range []string{o.SecretName, o.JournalName, o.PolicyName, o.ServiceName, o.LeaseName} {
		if value != "" {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	if count != 5 {
		return errors.New("all five result rotation object flags are required together")
	}
	if o.LeaseName == base.LeaseName || o.ServiceName == base.ServiceName || o.SecretName == o.JournalName || o.SecretName == base.SecretName || o.SecretName == base.StagingSecretName || o.JournalName == base.SecretName || o.JournalName == base.StagingSecretName {
		return errors.New("result rotation must use dedicated Secrets, Service, and Lease")
	}
	// Both policy barriers and the scheduled passes between them must finish
	// before the oldest certificate that triggered renewal expires.
	const overlap = resultcredentials.MaxCredentialLifetime + 5*time.Minute
	cycle := supervisor.RunInterval + supervisor.OperationTimeout // overflow checked by validateRuntimeRelationships
	if cycle >= base.RenewalThreshold/3 || 2*overlap >= base.RenewalThreshold-3*cycle {
		return errors.New("result rotation renewal threshold must exceed three scheduled cycles and both credential overlap waits")
	}
	return nil
}

func (o resultRotationOptions) config(base certrotation.Config) certrotation.ResultConfig {
	base.SecretName, base.StagingSecretName = o.SecretName, o.JournalName
	base.ServiceName, base.ServiceNamespace = o.ServiceName, base.Namespace
	base.LeaseName, base.EndpointPortName = o.LeaseName, "https"
	base.RecreateMissingSecret = false
	return certrotation.ResultConfig{Config: base, PolicyName: o.PolicyName}
}
