package main

import (
	"net/http"
	"strings"

	"k8s.io/client-go/rest"
)

// Give the lab's TCP load balancer another backend choice for each workload
// write. A persistent connection can otherwise leave an API server with no
// admission population for the whole soak. Reads and watches keep their normal
// connections; the operator's own clients are not changed.
func distributeCapacityWrites(config *rest.Config) {
	config.Wrap(func(base http.RoundTripper) http.RoundTripper {
		return &capacityWriteTransport{base: base}
	})
}

type capacityWriteTransport struct{ base http.RoundTripper }

func (t *capacityWriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasPrefix(request.URL.Path, "/apis/operator.ptah.run/") {
		switch request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			request = request.Clone(request.Context())
			request.Close = true
		}
	}
	return t.base.RoundTrip(request)
}

func (t *capacityWriteTransport) WrappedRoundTripper() http.RoundTripper { return t.base }
