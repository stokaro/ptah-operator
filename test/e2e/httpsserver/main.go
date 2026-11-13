// Command e2e-https-server serves files for the acceptance phase and the
// runner harness rows that read schemas and migration directories from an
// HTTPS download pinned by its digest.
//
// It serves every file of one directory, read again on every request, so a
// phase replaces a download's bytes by editing the ConfigMap the directory is
// mounted from. The first path element says how a file is reached:
//
//	/public/<file>      no credential
//	/basic/<file>       HTTP Basic, the user and password the flags name
//	/bearer/<file>      a Bearer token
//	/same/<prefix>/...  a redirect to /<prefix>/... on the host the client asked
//	/away/<prefix>/...  a redirect to /<prefix>/... on the other origin
//
// Every request is logged, and GET /requests answers the log as JSON, so a
// phase can show that a host or a path received nothing.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Request is one request the server received, as /requests answers it.
type Request struct {
	Host string `json:"host"`
	Path string `json:"path"`
	// Credential is the scheme of the Authorization header, or empty.
	Credential string `json:"credential,omitempty"`
	Status     int    `json:"status"`
}

type server struct {
	root        string
	user        string
	password    string
	token       string
	otherOrigin string

	mu       sync.Mutex
	requests []Request
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	root := flag.String("root", "", "the directory whose files are served")
	listen := flag.String("listen", ":8443", "where to serve HTTPS")
	certificate := flag.String("tls-certificate", "", "the server certificate, PEM")
	key := flag.String("tls-key", "", "the server key, PEM")
	user := flag.String("basic-user", "", "the user name Basic authentication takes")
	passwordFile := flag.String("basic-password-file", "", "the file holding the Basic password")
	tokenFile := flag.String("token-file", "", "the file holding the Bearer token")
	otherOrigin := flag.String("other-origin", "", "the origin /away/ redirects to, https://host[:port]")
	flag.Parse()

	s := &server{root: *root, user: *user, otherOrigin: strings.TrimSuffix(*otherOrigin, "/")}
	for target, file := range map[*string]string{&s.password: *passwordFile, &s.token: *tokenFile} {
		data, err := os.ReadFile(file) // #nosec G304 -- the fixture's own mounted Secret
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		*target = strings.TrimSpace(string(data))
	}
	if s.root == "" || s.user == "" || s.password == "" || s.token == "" || !strings.HasPrefix(s.otherOrigin, "https://") {
		return errors.New("-root, -basic-user, -basic-password-file, -token-file and an https:// -other-origin are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	httpServer := &http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = httpServer.Close()
	}()
	log.Printf("serving %s over https %s", s.root, *listen)
	if err := httpServer.ListenAndServeTLS(*certificate, *key); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/requests" && r.Method == http.MethodGet {
		s.mu.Lock()
		answer, err := json.Marshal(s.requests)
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
		return
	}
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.serve(recorder, r)
	scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	entry := Request{Host: r.Host, Path: r.URL.Path, Credential: scheme, Status: recorder.status}
	s.mu.Lock()
	s.requests = append(s.requests, entry)
	s.mu.Unlock()
	log.Printf("%s %s %s %d", entry.Host, entry.Path, entry.Credential, entry.Status)
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "only GET", http.StatusMethodNotAllowed)
		return
	}
	access, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch access {
	case "same":
		http.Redirect(w, r, "/"+rest, http.StatusFound)
		return
	case "away":
		http.Redirect(w, r, s.otherOrigin+"/"+rest, http.StatusFound)
		return
	case "public":
	case "basic":
		user, password, ok := r.BasicAuth()
		if !ok || !equal(user, s.user) || !equal(password, s.password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="e2e"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	case "bearer":
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !equal(token, s.token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="e2e"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	name, ok := servedName(rest)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.root, name)) // #nosec G304 -- one element, refused above if it is not a plain name
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

// servedName is the file a path names: one plain element, since the files
// come from a ConfigMap, whose keys hold no slash.
func servedName(rest string) (string, bool) {
	if rest == "" || strings.ContainsAny(rest, `/\`) || rest == "." || rest == ".." || strings.HasPrefix(rest, "..") {
		return "", false
	}
	return rest, true
}

func equal(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
