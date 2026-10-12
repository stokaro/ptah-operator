package webhook_test

import (
	"context"
	"errors"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Select the production public-pin admission implementation for this serial
// test; the other rows use the legacy certificate issuer on the same server.
type publicPinAdmission struct{ resultcredentials.PodBindings }

func (publicPinAdmission) ValidateCreate(context.Context, *corev1.Secret) error {
	return resultcredentials.ErrCredential
}

type enrollmentBoundaryClient struct {
	client.Client
	beforeCreate  func()
	afterCreate   func()
	afterReadback func()
}

func (c *enrollmentBoundaryClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if c.beforeCreate != nil {
		action := c.beforeCreate
		c.beforeCreate = nil
		action()
	}
	err := c.Client.Create(ctx, object, options...)
	if err == nil && c.afterCreate != nil {
		action := c.afterCreate
		c.afterCreate = nil
		action()
	}
	return err
}

func (c *enrollmentBoundaryClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	err := c.Client.Get(ctx, key, object, options...)
	if record, ok := object.(*api.PtahResultRecord); err == nil && ok && record.Spec.Type == "credential" && c.afterReadback != nil {
		action := c.afterReadback
		c.afterReadback = nil
		action()
	}
	return err
}

func TestPublicPodPinAuthorityAtAdmissionAndReturn(t *testing.T) {
	plane.Require(t)
	previous := controllerWriteValidator.ResultCredentials
	controllerWriteValidator.ResultCredentials = publicPinAdmission{resultcredentials.PodBindings{Reader: admin}}
	defer func() { controllerWriteValidator.ResultCredentials = previous }()
	for _, boundary := range []string{"valid", "before CREATE", "after CREATE", "after existing read"} {
		t.Run(boundary, func(t *testing.T) {
			f, identity, store := publicationFixture(t, false)
			apiClient := &enrollmentBoundaryClient{Client: store.Client}
			revocations := 0
			revoke := func() {
				f.schema.Status.ActiveOperation = nil
				writeStatus(t, f.schema)
				revocations++
			}
			if boundary == "before CREATE" {
				apiClient.beforeCreate = revoke
			}
			if boundary == "after CREATE" {
				apiClient.afterCreate = revoke
			}
			pins := resultcredentials.PodBindings{Writer: apiClient, Reader: apiClient}
			if boundary == "after existing read" {
				if _, err := pins.Ensure(t.Context(), identity); err != nil {
					t.Fatal(err)
				}
				apiClient.afterReadback = revoke
			}
			credential, err := pins.Ensure(t.Context(), identity)
			key := client.ObjectKey{Namespace: f.namespace, Name: jobconfig.CredentialName(identity.Binding.UID, identity.Binding.OperationID, identity.Binding.JobName)}
			stored := &api.PtahResultRecord{}
			readErr := store.Reader.Get(t.Context(), key, stored)
			switch boundary {
			case "valid":
				if err != nil || credential.UID == "" || readErr != nil || stored.UID != credential.UID || !resultcredentials.MatchesPodBinding(stored, identity) {
					t.Fatalf("valid public pin was not persisted exactly: enrollment=%v readback=%v", err, readErr)
				}
			case "before CREATE":
				requireDenied(t, err, controllerWriteWebhook, "immutable operation binding")
				if revocations != 1 || credential.UID != "" || !apierrors.IsNotFound(readErr) {
					t.Fatalf("revoked CREATE persisted a pin: revoked=%d credential=%+v readback=%v", revocations, credential, readErr)
				}
			case "after CREATE", "after existing read":
				if revocations != 1 || !errors.Is(err, resultdelivery.ErrAuthority) || credential.UID != "" || readErr != nil {
					t.Fatalf("revocation after persistence authorized return: revoked=%d credential=%+v error=%v readback=%v", revocations, credential, err, readErr)
				}
				if _, err := pins.AuthorizePublication(t.Context(), identity.Binding); !errors.Is(err, resultdelivery.ErrAuthority) {
					t.Fatalf("persisted pin granted publication after revocation: %v", err)
				}
			}
		})
	}
}
