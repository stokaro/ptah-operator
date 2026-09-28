package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// The intervals the data plane sets on its schemas. Each proof that moves a
// schema from one to another relies on the two being different, so a spec
// patch is a generation change and not a no-op; TestDataPlaneIntervalsDiffer
// holds them to that.
const (
	reconcileInterval      = "1m"
	tagMoveInterval        = "2m"
	approvalInterval       = "5m"
	staleApprovalInterval  = "4m"
	quiescentInterval      = "30m"
	blockedRefreshSeconds  = 90
	blockedRefreshInterval = "90s"
	// waitTimeout bounds every wait the phase makes. It has to cover three
	// blocked refresh intervals and two minutes beside them, the longest
	// sequence one wait covers.
	waitTimeout = 600 * time.Second
	// tlsProxyEndpointWaitAttempts is how many one-second readings the phase
	// gives the TLS proxy's Service to route to the captured Pod alone.
	tlsProxyEndpointWaitAttempts = 60
)

// The admission fixtures: a RuntimeClass whose overhead, node selector and
// toleration every operation Pod must carry once admitted.
const (
	admissionRuntimeClass = "ptah-e2e-runtime"
	admissionRuntimeTaint = "operator.ptah.run/e2e-runtime"
	digestPinPolicyName   = "e2e-digest-pin-verification-policy"
)

// The database fixtures. The passwords are derived from the namespace, so a
// rerun against the same namespace reads the same credentials.
const (
	pgUser                     = "ptah_e2e"
	pgDatabase                 = "ptah_e2e"
	pgSecret                   = "e2e-postgresql-db"
	pgService                  = "e2e-postgresql"
	customCAPGDatabase         = "ptah_e2e_custom_ca"
	customCAPGSecret           = "e2e-postgresql-custom-ca-db"
	customCACoordinationKey    = "e2e/custom-ca/app"
	fourEyesPGDatabase         = "ptah_e2e_four_eyes"
	fourEyesPGSecret           = "e2e-postgresql-four-eyes-db"
	podMetadataPGDatabase      = "ptah_e2e_pod_metadata"
	podMetadataPGSecret        = "e2e-postgresql-pod-metadata-db"
	mysqlUser                  = "ptah_e2e"
	mysqlDatabase              = "ptah_e2e"
	mysqlSecret                = "e2e-mysql-db"
	mysqlService               = "e2e-mysql"
	externalPGSecret           = "e2e-postgresql-external-db"
	externalPGSchema           = "e2e-postgresql-external-longpod"
	externalPGCoordinationKey  = "e2e/postgresql-external/app"
	registryAuthSecret         = "e2e-registry-auth"
	registryPullSecret         = "e2e-registry-pull"
	digestPinDockerAuthSecret  = "e2e-registry-digest-pin-docker-auth"
	tlsProxyCAConfigMap        = "e2e-registry-tls-ca"
	tlsProxyCertSecret         = "e2e-registry-tls-server"
	tlsProxyGoodAuthSecret     = "e2e-registry-tls-auth"
	tlsProxyBadCAAuthSecret    = "e2e-registry-tls-auth-bad-ca"
	tlsProxyBadAuthoritySecret = "e2e-registry-tls-auth-bad-authority"
	tlsProxyWrongAuthority     = "registry-mismatch.invalid:5443"
	tlsProxyBadCASHA256        = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	schemaArtifactType         = "application/vnd.stokaro.ptah.schema.v1"
)

// posixCksum is the checksum POSIX cksum prints: CRC-32 with the polynomial
// 0x04C11DB7, most significant bit first, over the data and then its length
// in the fewest bytes, least significant first, complemented. The shell phases
// derived the fixture passwords with it, and a rerun against a namespace an
// earlier run set up must derive the same ones.
func posixCksum(data []byte) uint32 {
	var crc uint32
	update := func(value byte) {
		crc = crc<<8 ^ cksumTable[byte(crc>>24)^value]
	}
	for _, value := range data {
		update(value)
	}
	for length := len(data); length > 0; length >>= 8 {
		update(byte(length))
	}
	return ^crc
}

var cksumTable = func() (table [256]uint32) {
	for index := range table {
		value := uint32(index) << 24
		for range 8 {
			if value&0x80000000 != 0 {
				value = value<<1 ^ 0x04C11DB7
			} else {
				value <<= 1
			}
		}
		table[index] = value
	}
	return table
}()

// fixtureCredentials are the database credentials the phase hands its
// fixtures. Every one of them is a protected pattern: none may reach a status,
// an Event, a log, a metric or a non-Secret object.
type fixtureCredentials struct {
	pgPassword        string
	pgURL             string
	customCAPGURL     string
	fourEyesPGURL     string
	podMetadataPGURL  string
	mysqlPassword     string
	mysqlRootPassword string
	mysqlURL          string
}

func deriveFixtureCredentials(namespace string) fixtureCredentials {
	suffix := strconv.FormatUint(uint64(posixCksum([]byte(namespace))), 10)
	pgPassword := "e2ePg" + suffix + "Q7"
	mysqlPassword := "e2eMy" + suffix + "Q7"
	pgURL := func(database string) string {
		return "postgres://" + pgUser + ":" + pgPassword + "@" + pgService + "." + namespace +
			".svc.cluster.local:5432/" + database + "?sslmode=disable"
	}
	return fixtureCredentials{
		pgPassword:        pgPassword,
		pgURL:             pgURL(pgDatabase),
		customCAPGURL:     pgURL(customCAPGDatabase),
		fourEyesPGURL:     pgURL(fourEyesPGDatabase),
		podMetadataPGURL:  pgURL(podMetadataPGDatabase),
		mysqlPassword:     mysqlPassword,
		mysqlRootPassword: "e2eMyRoot" + suffix + "Q7",
		mysqlURL: "mysql://" + mysqlUser + ":" + mysqlPassword + "@tcp(" + mysqlService + "." + namespace +
			".svc.cluster.local:3306)/" + mysqlDatabase,
	}
}

// credentialScanner finds a protected credential in bytes the phase is about
// to trust as credential-free: a status, an Event, a log, a metric exposition,
// an error message. It is grep -F -f over the protected patterns, and it
// refuses to be built with nothing to look for or with an empty pattern, which
// would match everything and nothing usefully.
type credentialScanner struct {
	patterns [][]byte
}

func newCredentialScanner(patterns ...string) (credentialScanner, error) {
	if len(patterns) == 0 {
		return credentialScanner{}, errors.New("credential scanner has no non-empty protected patterns")
	}
	scanner := credentialScanner{}
	for _, pattern := range patterns {
		if pattern == "" || strings.Contains(pattern, "\n") {
			return credentialScanner{}, errors.New("credential scanner has an empty or multi-line protected pattern")
		}
		scanner.patterns = append(scanner.patterns, []byte(pattern))
	}
	return scanner, nil
}

// leaks reports whether content carries any protected pattern.
func (s credentialScanner) leaks(content []byte) bool {
	for _, pattern := range s.patterns {
		if bytes.Contains(content, pattern) {
			return true
		}
	}
	return false
}

// ready reports whether the scanner has patterns to scan with. A scanner that
// was never built answers every scan with a leak.
func (s credentialScanner) ready() bool {
	return len(s.patterns) > 0
}

var (
	ipv4Address   = regexp.MustCompile(`^[0-9]+(\.[0-9]+){3}$`)
	dnsLabel      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	ownerLabel    = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)
	containerID   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	numericPort   = regexp.MustCompile(`^[0-9]+$`)
	ptahVersionOK = regexp.MustCompile(`^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$`)
	sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// dataPlaneInputsOK is the refusal the phase gives before it reads the
// cluster: an input the phase cannot use as the driver meant it.
func dataPlaneInputsOK(in phases.DataPlaneInputs) error {
	if in.Mode != "full" && in.Mode != "prepare" {
		return fmt.Errorf("E2E_DATAPLANE_MODE accepts full or prepare, got %s", in.Mode)
	}
	if !digestPinnedImage.MatchString(in.ControllerImage) {
		return errors.New("E2E_CONTROLLER_IMAGE must be pinned by a lowercase SHA-256 digest")
	}
	if err := controllerRevisionOK(in.ControllerRevision); err != nil {
		return err
	}
	if !positiveInteger.MatchString(in.ControllerStateVersion) {
		return errors.New("E2E_CONTROLLER_STATE_VERSION must be a positive integer")
	}
	switch in.DockerContext {
	case "default", "orbstack", "":
		return errors.New("E2E_DOCKER_CONTEXT must name an explicit nonlocal Docker context")
	}
	if len(in.PtahVersion) < 1 || len(in.PtahVersion) > 128 {
		return errors.New("E2E_PTAH_VERSION must contain between 1 and 128 bytes")
	}
	if !ptahVersionOK.MatchString(in.PtahVersion) {
		return errors.New("E2E_PTAH_VERSION must not contain control or edge-whitespace characters")
	}
	if !containerID.MatchString(in.RegistryContainerID) || !containerID.MatchString(in.ExternalPostgresContainerID) {
		return errors.New("Docker fixture IDs must be exact 64-character lowercase IDs")
	}
	if !ipv4Address.MatchString(in.ExternalPostgresIP) {
		return errors.New("E2E_EXTERNAL_POSTGRES_IP must be an IPv4 address on the kind Docker network")
	}
	if !dnsLabel.MatchString(in.ExternalPostgresService) {
		return errors.New("E2E_EXTERNAL_POSTGRES_SERVICE must be a DNS label")
	}
	if !ownerLabel.MatchString(in.ExternalPostgresOwner) {
		return errors.New("E2E_EXTERNAL_POSTGRES_OWNER contains unsupported characters")
	}
	if !digestPinnedImage.MatchString(in.ExternalPostgresImage) {
		return errors.New("E2E_EXTERNAL_POSTGRES_IMAGE must be digest-pinned")
	}
	if !dnsLabel.MatchString(in.TLSProxyService) {
		return errors.New("E2E_TLS_PROXY_SERVICE must be a DNS label")
	}
	if in.TLSProxyCAFile == in.TLSProxyCertFile || in.TLSProxyCAFile == in.TLSProxyKeyFile ||
		in.TLSProxyCertFile == in.TLSProxyKeyFile {
		return errors.New("TLS proxy CA, certificate, and private key must be separate files")
	}
	for _, image := range []string{in.ExecutorImage, in.RunnerImage, in.FixtureImage, in.PostgresImage, in.MySQLImage} {
		if !digestPinnedImage.MatchString(image) {
			return fmt.Errorf("data-plane images must be pinned by a lowercase SHA-256 digest: %s", image)
		}
	}
	if !ipv4Address.MatchString(in.RegistryIP) {
		return errors.New("E2E_REGISTRY_IP must be an IPv4 address on the kind Docker network")
	}
	if !numericPort.MatchString(in.RegistryPort) {
		return errors.New("E2E_REGISTRY_PORT must be numeric")
	}
	if port, err := strconv.Atoi(in.RegistryPort); err != nil || port < 1024 || port > 65535 {
		return errors.New("E2E_REGISTRY_PORT must be between 1024 and 65535")
	}
	return nil
}

var credentialName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// registryCredentials is the registry's login, as the driver wrote it: an
// object of exactly a username and a password.
type registryCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func parseRegistryCredentials(content []byte) (registryCredentials, error) {
	refusal := errors.New("E2E_REGISTRY_CREDENTIALS_FILE has an invalid shape")
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(content, &keys); err != nil || len(keys) != 2 {
		return registryCredentials{}, refusal
	}
	var credentials registryCredentials
	if err := strictUnmarshal(content, &credentials); err != nil {
		return registryCredentials{}, refusal
	}
	if !credentialName.MatchString(credentials.Username) || credentials.Password == "" ||
		strings.Contains(credentials.Password, "\n") {
		return registryCredentials{}, refusal
	}
	return credentials, nil
}

// externalPostgresCredentials is the external PostgreSQL's login: the one Ptah
// is given, bound to the Service that routes to the container.
type externalPostgresCredentials struct {
	Database string `json:"database"`
	Password string `json:"password"`
	URL      string `json:"url"`
	Username string `json:"username"`
}

func parseExternalPostgresCredentials(content []byte, service, namespace string) (externalPostgresCredentials, error) {
	refusal := errors.New("E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE has an invalid or misbound shape")
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(content, &keys); err != nil || len(keys) != 4 {
		return externalPostgresCredentials{}, refusal
	}
	var credentials externalPostgresCredentials
	if err := strictUnmarshal(content, &credentials); err != nil {
		return externalPostgresCredentials{}, refusal
	}
	authority := service + "." + namespace + ".svc.cluster.local:5432"
	if !credentialName.MatchString(credentials.Username) || !credentialName.MatchString(credentials.Password) ||
		!credentialName.MatchString(credentials.Database) ||
		credentials.URL != "postgres://"+credentials.Username+":"+credentials.Password+"@"+authority+"/"+
			credentials.Database+"?sslmode=disable" {
		return externalPostgresCredentials{}, refusal
	}
	return credentials, nil
}

// strictUnmarshal decodes a document that may carry no field target does not
// declare.
func strictUnmarshal(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing content after the document")
	}
	return nil
}

// observedJob is one Job the phase has seen in the test namespace. The ledger
// of them is how a proof counts the Jobs a schema started between two
// moments, including Jobs the controller's TTL has since deleted.
type observedJob struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Created   string `json:"created"`
	Schema    string `json:"schema"`
	Operation string `json:"operation"`
}

func observedJobRecords(jobs []batchv1.Job) ([]observedJob, error) {
	records := make([]observedJob, 0, len(jobs))
	for index := range jobs {
		job := &jobs[index]
		record := observedJob{
			UID:       string(job.UID),
			Name:      job.Name,
			Schema:    job.Labels["operator.ptah.run/schema"],
			Operation: job.Labels["operator.ptah.run/operation"],
		}
		if !job.CreationTimestamp.IsZero() {
			record.Created = job.CreationTimestamp.UTC().Format(time.RFC3339)
		}
		if record.UID == "" || record.Name == "" || record.Created == "" {
			return nil, errors.New("job ledger record contains invalid identity fields")
		}
		records = append(records, record)
	}
	return records, nil
}

// jobLedger is the observed-Job ledger: every Job the phase has seen, once
// each, in the order it first saw them.
type jobLedger struct {
	records []observedJob
	seen    map[string]bool
}

func newJobLedger() *jobLedger {
	return &jobLedger{seen: map[string]bool{}}
}

// add records every Job the ledger does not hold yet.
func (l *jobLedger) add(records []observedJob) {
	for _, record := range records {
		if l.seen[record.UID] {
			continue
		}
		l.seen[record.UID] = true
		l.records = append(l.records, record)
	}
}

// checkpoint is the set of Job UIDs the ledger held for one schema at one
// moment, sorted. A proof counts what appeared between two of them.
type checkpoint []string

// sortedCheckpoint is the set of the UIDs given.
func sortedCheckpoint(uids []string) checkpoint {
	sorted := slices.Clone(uids)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

func (c checkpoint) holds(uid string) bool {
	_, found := slices.BinarySearch(c, uid)
	return found
}

// schemaCheckpoint is every Job of the schema, or of the schema and one
// operation when operation is not empty, that the ledger holds.
func (l *jobLedger) checkpoint(schema, operation string) checkpoint {
	uids := checkpoint{}
	for _, record := range l.records {
		if record.Schema == schema && (operation == "" || record.Operation == operation) {
			uids = append(uids, record.UID)
		}
	}
	slices.Sort(uids)
	return slices.Compact(uids)
}

// between is every record of the schema, and of the operation when it is not
// empty, whose UID after holds and before does not, once each.
func (l *jobLedger) between(schema, operation string, before, after checkpoint) []observedJob {
	return l.selected(schema, operation, before, func(uid string) bool { return after.holds(uid) })
}

// since is every record of the schema, and of the operation when it is not
// empty, whose UID before does not hold, once each.
func (l *jobLedger) since(schema, operation string, before checkpoint) []observedJob {
	return l.selected(schema, operation, before, func(string) bool { return true })
}

func (l *jobLedger) selected(schema, operation string, before checkpoint, admitted func(string) bool) []observedJob {
	var found []observedJob
	seen := map[string]bool{}
	for _, record := range l.records {
		if record.Schema != schema || (operation != "" && record.Operation != operation) {
			continue
		}
		if before.holds(record.UID) || !admitted(record.UID) || seen[record.UID] {
			continue
		}
		seen[record.UID] = true
		found = append(found, record)
	}
	return found
}

// jobBoundaryUnchanged holds the Jobs a schema started since a checkpoint to
// exactly the UIDs a proof captured, count of them, and returns them sorted:
// a Job that appeared after the capture is work the proof did not account for.
func jobBoundaryUnchanged(records []observedJob, expected []string, count int) ([]string, error) {
	actual := make([]string, 0, len(records))
	for _, record := range records {
		actual = append(actual, record.UID)
	}
	actual = sortedCheckpoint(actual)
	switch {
	case len(expected) != count:
		return nil, fmt.Errorf("the proof captured %d Jobs, not %d", len(expected), count)
	case len(actual) != count || !slices.Equal(actual, expected):
		return nil, fmt.Errorf("the ledger holds %v since the checkpoint, not the captured %v", actual, expected)
	}
	return actual, nil
}

// uidLedger is a set of Job UIDs a credential audit has reached.
type uidLedger struct {
	uids []string
	seen map[string]bool
}

func newUIDLedger() *uidLedger {
	return &uidLedger{seen: map[string]bool{}}
}

func (l *uidLedger) holds(uid string) bool {
	return l.seen[uid]
}

// add records uid unless the ledger holds it.
func (l *uidLedger) add(uid string) {
	if uid == "" || l.seen[uid] {
		return
	}
	l.seen[uid] = true
	l.uids = append(l.uids, uid)
}
