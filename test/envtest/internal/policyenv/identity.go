package policyenv

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The user extras a projected ServiceAccount token carries. The credential
// boundary policies read them to tell a token minted for a Pod from one that
// was not.
const (
	podNameExtra = "authentication.kubernetes.io/pod-name"
	podUIDExtra  = "authentication.kubernetes.io/pod-uid"
)

// Identity is who a request claims to be. The suite impersonates it, which
// is the same user information a real token would give the API server:
// admission sees the username, the groups and the extras, and nothing else.
type Identity struct {
	Username string
	// Groups left empty let the API server derive them, which for a
	// ServiceAccount is exactly the groups its token would carry.
	Groups []string
	Extra  map[string][]string
}

func (identity Identity) String() string {
	var extras []string
	for key, values := range identity.Extra {
		extras = append(extras, key+"="+strings.Join(values, ","))
	}
	sort.Strings(extras)
	if len(extras) == 0 {
		return identity.Username
	}
	return identity.Username + " (" + strings.Join(extras, " ") + ")"
}

// BoundTo returns identity as a token bound to the named Pod presents it.
func (identity Identity) BoundTo(pod, uid string) Identity {
	bound := identity
	bound.Extra = map[string][]string{podNameExtra: {pod}, podUIDExtra: {uid}}
	return bound
}

// ServiceAccount is the identity of a ServiceAccount token with no Pod bound.
func ServiceAccount(namespace, name string) Identity {
	return Identity{Username: serviceaccount.MakeUsername(namespace, name)}
}

// The Pods the protected identities run in. Only the shape of each name
// matters to the policies: the Deployment or Job name, and the suffixes the
// ReplicaSet and Job controllers generate.
const (
	replicaSuffix = "-7d9c5b8f4-x2k9q"
	jobPodSuffix  = "-h4k9z"
)

// Manager is the manager's ServiceAccount as its own Pod presents it.
func (env *Env) Manager() Identity {
	names := env.Chart.Names
	return ServiceAccount(names.Namespace, names.Manager).
		BoundTo(names.ManagerDeployment+replicaSuffix, "7f6c1d0e-0000-4000-8000-000000000001")
}

// Certificate is the certificate rotator as its own Pod presents it.
func (env *Env) Certificate() Identity {
	names := env.Chart.Names
	return ServiceAccount(names.Namespace, names.Certificate).
		BoundTo(names.CertificateDeployment+replicaSuffix, "7f6c1d0e-0000-4000-8000-000000000002")
}

// Hook is the CRD manager hook as the Pod of its reconcile Job presents it.
// The hook Job carries the ServiceAccount's name.
func (env *Env) Hook() Identity {
	names := env.Chart.Names
	return ServiceAccount(names.Namespace, names.Hook).
		BoundTo(names.Hook+jobPodSuffix, "7f6c1d0e-0000-4000-8000-000000000003")
}

// User is an ordinary user: RBAC lets it write, and nothing makes it an
// administrator of admission.
func User() Identity {
	return Identity{Username: OrdinaryUser}
}

// JobController is the Job controller, the one identity that creates a Job's
// Pods.
func JobController() Identity {
	return ServiceAccount("kube-system", "job-controller")
}

// As returns a client that acts as identity. Clients are cached: each one
// discovers the API on first use.
func (env *Env) As(identity Identity) (client.Client, error) {
	key := identity.String() + "|" + strings.Join(identity.Groups, ",")
	env.mu.Lock()
	defer env.mu.Unlock()
	if cached, ok := env.clients[key]; ok {
		return cached, nil
	}
	config := rest.CopyConfig(env.Plane.Config)
	config.Impersonate = rest.ImpersonationConfig{
		UserName: identity.Username,
		Groups:   identity.Groups,
		Extra:    identity.Extra,
	}
	created, err := client.New(config, client.Options{Scheme: env.Scheme})
	if err != nil {
		return nil, fmt.Errorf("client for %s: %w", identity, err)
	}
	env.clients[key] = created
	return created, nil
}
