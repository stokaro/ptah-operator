// Command e2e-git-server serves one Git repository for the acceptance phase
// that reads schemas and migration directories from Git: over smart HTTP
// behind TLS with a token, and over SSH with one authorized key.
//
// The repository is a Spec in a file, which the phase keeps in a ConfigMap.
// The server builds it again whenever the file changes, so the phase moves a
// branch by editing the ConfigMap. It speaks only the part of upload-pack that
// go-git, and so Ptah's pull, uses.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

type server struct {
	repositoryPath string
	byID           bool

	mu         sync.RWMutex
	repository *served
	spec       []byte
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	specFile := flag.String("spec", "", "the repository spec, JSON")
	name := flag.String("repository", "app.git", "the repository's path on the server")
	byID := flag.Bool("allow-fetch-by-id", false, "let a client want a commit no ref names")
	httpAddress := flag.String("https-listen", ":8443", "where to serve smart HTTP over TLS")
	certificate := flag.String("tls-certificate", "", "the server certificate, PEM")
	key := flag.String("tls-key", "", "the server key, PEM")
	tokenFile := flag.String("token-file", "", "the file holding the token a client sends as its password")
	sshAddress := flag.String("ssh-listen", ":2222", "where to serve SSH")
	hostKey := flag.String("ssh-host-key", "", "the SSH host key, OpenSSH PEM")
	authorizedKey := flag.String("ssh-authorized-key", "", "the one public key a client may use, authorized_keys format")
	flag.Parse()

	s := &server{repositoryPath: "/" + strings.TrimPrefix(*name, "/"), byID: *byID}
	if err := s.reload(*specFile); err != nil {
		return err
	}
	token, err := os.ReadFile(*tokenFile) // #nosec G304 -- the fixture's own mounted Secret
	if err != nil {
		return fmt.Errorf("read the token: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go s.watch(ctx, *specFile)

	listener, err := net.Listen("tcp", *sshAddress)
	if err != nil {
		return err
	}
	config, err := s.sshConfig(*hostKey, *authorizedKey)
	if err != nil {
		return err
	}
	go s.serveSSH(ctx, listener, config)

	httpServer := &http.Server{
		Addr: *httpAddress, Handler: s.handler(bytes.TrimSpace(token)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = httpServer.Close()
		_ = listener.Close()
	}()
	log.Printf("serving %s over https %s and ssh %s", s.repositoryPath, *httpAddress, *sshAddress)
	if err := httpServer.ListenAndServeTLS(*certificate, *key); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// reload builds the repository again when the spec file holds other bytes.
func (s *server) reload(file string) error {
	data, err := os.ReadFile(file) // #nosec G304 -- the fixture's own mounted ConfigMap
	if err != nil {
		return fmt.Errorf("read the repository spec: %w", err)
	}
	s.mu.RLock()
	same := bytes.Equal(data, s.spec)
	s.mu.RUnlock()
	if same {
		return nil
	}
	spec, err := gitrepo.ParseSpec(data)
	if err != nil {
		return err
	}
	built, err := gitrepo.Build(spec)
	if err != nil {
		return err
	}
	repository := newServed(built)
	s.mu.Lock()
	s.repository, s.spec = repository, data
	s.mu.Unlock()
	for name, hash := range repository.refs {
		log.Printf("%s is %s", name, hash)
	}
	return nil
}

func (s *server) watch(ctx context.Context, file string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reload(file); err != nil {
				log.Printf("the repository spec was not reloaded: %v", err)
			}
		}
	}
}

func (s *server) current() *served {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repository
}

// handler serves upload-pack over smart HTTP to a client that sends the
// token as its password, under any username.
func (s *server) handler(token []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+s.repositoryPath+"/info/refs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != "git-upload-pack" {
			http.Error(w, "only git-upload-pack is served", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		if err := advertise(w, s.current(), s.byID, true); err != nil {
			log.Printf("advertise: %v", err)
		}
	})
	mux.HandleFunc("POST "+s.repositoryPath+"/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		if err := uploadPack(r.Body, w, s.current(), s.byID); err != nil {
			log.Printf("upload-pack over https: %v", err)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(password), token) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="e2e"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *server) sshConfig(hostKeyFile, authorizedKeyFile string) (*ssh.ServerConfig, error) {
	hostKeyPEM, err := os.ReadFile(hostKeyFile) // #nosec G304 -- the fixture's own mounted Secret
	if err != nil {
		return nil, fmt.Errorf("read the SSH host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(hostKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse the SSH host key: %w", err)
	}
	authorizedLine, err := os.ReadFile(authorizedKeyFile) // #nosec G304 -- the fixture's own mounted Secret
	if err != nil {
		return nil, fmt.Errorf("read the authorized key: %w", err)
	}
	authorized, _, _, _, err := ssh.ParseAuthorizedKey(authorizedLine)
	if err != nil {
		return nil, fmt.Errorf("parse the authorized key: %w", err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), authorized.Marshal()) {
			return &ssh.Permissions{}, nil
		}
		return nil, errors.New("the key is not authorized")
	}}
	config.AddHostKey(signer)
	return config, nil
}

func (s *server) serveSSH(ctx context.Context, listener net.Listener, config *ssh.ServerConfig) {
	for ctx.Err() == nil {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("accept: %v", err)
			}
			return
		}
		go s.serveSSHConnection(connection, config)
	}
}

func (s *server) serveSSHConnection(connection net.Conn, config *ssh.ServerConfig) {
	defer func() { _ = connection.Close() }()
	_, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		log.Printf("ssh handshake: %v", err)
		return
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions are served")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.serveSSHSession(channel, channelRequests)
	}
}

// serveSSHSession runs one "git-upload-pack <path>" exec request.
func (s *server) serveSSHSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			return
		}
		command, path, _ := strings.Cut(payload.Command, " ")
		path = strings.Trim(path, `'"`)
		if command != "git-upload-pack" || "/"+strings.TrimPrefix(path, "/") != s.repositoryPath {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		repository := s.current()
		status := uint32(0)
		if err := advertise(channel, repository, s.byID, false); err != nil {
			status = 1
		} else if err := uploadPack(channel, channel, repository, s.byID); err != nil {
			log.Printf("upload-pack over ssh: %v", err)
			status = 1
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}
