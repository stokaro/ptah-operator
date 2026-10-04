package resultcredentials

import (
	"context"
	"crypto/tls"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AuthorizePublication binds a publication to the canonical issued credential
// and current operation. A claimed identity in record bytes is not authority.
// Reader is the issuer's direct API reader; no Secret is read.
func (i *Issuer) AuthorizePublication(ctx context.Context, b resultstore.Binding) (resultdelivery.Identity, error) {
	if i == nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	if _, err := resultstore.Name(b); err != nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	record := &api.PtahResultRecord{}
	key := client.ObjectKey{Namespace: b.Namespace, Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)}
	if err := i.reader.Get(ctx, key, record); err != nil {
		return resultdelivery.Identity{}, err
	}
	secret, err := recordSecret(record)
	if err != nil {
		return resultdelivery.Identity{}, err
	}
	cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	identity, err := resultdelivery.ClientIdentity(cert)
	if err != nil || identity.Binding != b {
		return resultdelivery.Identity{}, ErrCredential
	}
	if _, err := i.validate(secret, identity); err != nil {
		return resultdelivery.Identity{}, err
	}
	if err := (resultauthority.Authorizer{Reader: i.reader}).Check(ctx, identity); err != nil {
		return resultdelivery.Identity{}, err
	}
	return identity, nil
}
