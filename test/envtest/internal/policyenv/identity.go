package policyenv

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The user extras a projected ServiceAccount token carries. No installed
// policy reads them; the protected identities carry them so that each request
// reaches admission as the one a real Pod's token would send.
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

// replicaSuffix is what the ReplicaSet controller appends to the name of a
// Deployment's Pod.
const replicaSuffix = "-7d9c5b8f4-x2k9q"

// Manager is the manager's ServiceAccount as its own Pod presents it.
func (env *Env) Manager() Identity {
	names := env.Chart.Names
	return ServiceAccount(names.Namespace, names.Manager).
		BoundTo(names.ManagerDeployment+replicaSuffix, "7f6c1d0e-0000-4000-8000-000000000001")
}

// User is an ordinary user: RBAC lets it write, and nothing makes it an
// administrator of admission.
func User() Identity {
	return Identity{Username: OrdinaryUser}
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
