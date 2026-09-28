//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// databasesAndFixtures stands up what every later row, and every migration
// suite, reads in the test namespace: the registry endpoint, both database
// servers with their Secrets, the authenticated HTTPS registry proxy, the
// external PostgreSQL's route, the custom-CA database, and the admission
// fixtures.
func (d *dataPlane) databasesAndFixtures() {
	d.logf("creating registry endpoint and isolated databases")
	d.createRegistryService()
	d.createDatabases()
	d.createAuthenticatedTLSProxy()
	for _, deployment := range []string{pgService, mysqlService} {
		if err := d.cluster.WaitForRollout(d.ctx, d.in.TestNamespace, deployment, waitTimeout); err != nil {
			d.fatalf("%v", err)
		}
	}
	d.waitForDatabase("postgresql")
	d.waitForDatabase("mysql")
	d.createExternalPostgresqlEndpoint()
	d.createCustomCADatabase()
	d.reportDatabaseVersions()
	d.createAdmissionFixtures()
	d.createDigestPinPolicyFixture()
}

// createRegistryService routes the registry Service in the test namespace to
// the registry container's address on the kind network.
func (d *dataPlane) createRegistryService() {
	d.t.Helper()
	d.mustApply(
		map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": d.in.RegistryService},
			"spec": map[string]any{"ports": []any{map[string]any{
				"name": "http", "port": int64(5000), "protocol": "TCP", "targetPort": int64(5000),
			}}},
		},
		map[string]any{
			"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
			"metadata": map[string]any{
				"namespace": d.in.TestNamespace, "name": d.in.RegistryService + "-docker",
				"labels": map[string]any{"kubernetes.io/service-name": d.in.RegistryService},
			},
			"addressType": "IPv4",
			"endpoints":   []any{map[string]any{"addresses": []any{d.in.RegistryIP}}},
			"ports":       []any{map[string]any{"name": "http", "port": int64(5000), "protocol": "TCP"}},
		},
	)
}

func databaseLabels(name string) map[string]any {
	return map[string]any{"app.kubernetes.io/name": name, "app.kubernetes.io/component": "e2e-database"}
}

func secretEnv(name, secret, key string) map[string]any {
	return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}}
}

// databaseDeployment is one database server with its data on an emptyDir and
// its credentials read from its Secret.
func databaseDeployment(namespace, name, container, image, port string, portNumber, initialDelay int64,
	dataPath string, env []any,
) map[string]any {
	labels := databaseLabels(name)
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"namespace": namespace, "name": name, "labels": labels},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"automountServiceAccountToken": false,
					"imagePullSecrets":             []any{map[string]any{"name": registryPullSecret}},
					"containers": []any{map[string]any{
						"name": container, "image": image, "imagePullPolicy": "IfNotPresent",
						"env":   env,
						"ports": []any{map[string]any{"name": port, "containerPort": portNumber}},
						"readinessProbe": map[string]any{
							"tcpSocket":           map[string]any{"port": port},
							"initialDelaySeconds": initialDelay, "periodSeconds": int64(2),
						},
						"volumeMounts": []any{map[string]any{"name": "data", "mountPath": dataPath}},
					}},
					"volumes": []any{map[string]any{"name": "data", "emptyDir": map[string]any{}}},
				},
			},
		},
	}
}

func databaseService(namespace, name, port string, portNumber int64) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"selector": databaseLabels(name),
			"ports":    []any{map[string]any{"name": port, "port": portNumber, "targetPort": port}},
		},
	}
}

// createDatabases stands up both database servers and the Secrets the rows
// hand their schemas: the registry's credential for the runner, its pull
// credential, the digest-pin row's Docker config, and the database URLs.
func (d *dataPlane) createDatabases() {
	d.t.Helper()
	namespace := d.in.TestNamespace
	username, password := d.registry.Username, d.registry.Password
	pullConfig := dockerConfigJSON(d.registryHost, username, password)
	secret := func(name, kind string, immutable bool, values map[string]string) map[string]any {
		document := map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"namespace": namespace, "name": name},
			"type":     kind, "data": secretData(values),
		}
		if immutable {
			document["immutable"] = true
		}
		return document
	}
	d.mustApply(
		secret(registryAuthSecret, "Opaque", false, map[string]string{
			"username": username, "password": password, "registry": d.registryHost, "allowPlainHTTP": "true",
		}),
		secret(registryPullSecret, "kubernetes.io/dockerconfigjson", false, map[string]string{".dockerconfigjson": pullConfig}),
		secret(digestPinDockerAuthSecret, "kubernetes.io/dockerconfigjson", true, map[string]string{
			".dockerconfigjson": pullConfig, "registry": d.registryHost, "allowPlainHTTP": "true",
		}),
		secret(pgSecret, "Opaque", false, map[string]string{
			"username": pgUser, "password": d.credentials.pgPassword, "database": pgDatabase, "url": d.credentials.pgURL,
		}),
		secret(customCAPGSecret, "Opaque", true, map[string]string{
			"username": pgUser, "password": d.credentials.pgPassword, "database": customCAPGDatabase,
			"url": d.credentials.customCAPGURL,
		}),
		databaseDeployment(namespace, pgService, "postgresql", d.in.PostgresImage, "postgresql", 5432, 2,
			"/var/lib/postgresql/data", []any{
				secretEnv("POSTGRES_USER", pgSecret, "username"),
				secretEnv("POSTGRES_PASSWORD", pgSecret, "password"),
				secretEnv("POSTGRES_DB", pgSecret, "database"),
				map[string]any{"name": "PGDATA", "value": "/var/lib/postgresql/data/pgdata"},
			}),
		databaseService(namespace, pgService, "postgresql", 5432),
		secret(mysqlSecret, "Opaque", false, map[string]string{
			"username": mysqlUser, "password": d.credentials.mysqlPassword, "rootPassword": d.credentials.mysqlRootPassword,
			"database": mysqlDatabase, "url": d.credentials.mysqlURL,
		}),
		databaseDeployment(namespace, mysqlService, "mysql", d.in.MySQLImage, "mysql", 3306, 5, "/var/lib/mysql", []any{
			secretEnv("MYSQL_USER", mysqlSecret, "username"),
			secretEnv("MYSQL_PASSWORD", mysqlSecret, "password"),
			secretEnv("MYSQL_ROOT_PASSWORD", mysqlSecret, "rootPassword"),
			secretEnv("MYSQL_DATABASE", mysqlSecret, "database"),
		}),
		databaseService(namespace, mysqlService, "mysql", 3306),
	)
}

// createAuthenticatedTLSProxy stands up the HTTPS registry proxy a custom CA
// signs, and three registry credentials for it: one that grants its authority
// and its CA, one whose CA grant is wrong, and one whose authority grant is
// wrong. The proxy counts the requests it serves on a port no Service routes
// to, so a row can tell whether a refusal happened before the registry was
// reached.
func (d *dataPlane) createAuthenticatedTLSProxy() {
	d.t.Helper()
	d.logf("creating authenticated HTTPS custom-CA registry proxy")
	namespace := d.in.TestNamespace
	certificate, err := os.ReadFile(d.in.TLSProxyCertFile)
	d.check(err, "could not render the task-scoped TLS proxy certificate Secret")
	key, err := os.ReadFile(d.in.TLSProxyKeyFile)
	d.check(err, "could not render the task-scoped TLS proxy certificate Secret")
	caBundle, err := os.ReadFile(d.in.TLSProxyCAFile)
	d.check(err, "could not render the task-scoped TLS proxy CA ConfigMap")
	d.mustCreate(
		map[string]any{
			"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "kubernetes.io/tls",
			"metadata": map[string]any{"namespace": namespace, "name": tlsProxyCertSecret},
			"data": map[string]any{
				"tls.crt": base64.StdEncoding.EncodeToString(certificate),
				"tls.key": base64.StdEncoding.EncodeToString(key),
			},
		},
		map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
			"metadata": map[string]any{"namespace": namespace, "name": tlsProxyCAConfigMap},
			"data":     map[string]any{"ca.pem": string(caBundle)},
		},
	)

	labels := map[string]any{
		"app.kubernetes.io/name": d.in.TLSProxyService, "app.kubernetes.io/component": "e2e-tls-registry-proxy",
	}
	authSecret := func(name, authority, caDigest string) map[string]any {
		return map[string]any{
			"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
			"metadata": map[string]any{"namespace": namespace, "name": name},
			"data": secretData(map[string]string{
				"username": d.registry.Username, "password": d.registry.Password,
				"registry": authority, "caSHA256": caDigest,
			}),
		}
	}
	restricted := map[string]any{
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "runAsNonRoot": true,
		"runAsUser": int64(65532), "runAsGroup": int64(65532),
		"capabilities":   map[string]any{"drop": []any{"ALL"}},
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
	d.mustCreate(
		authSecret(tlsProxyGoodAuthSecret, d.tlsProxy.authority, d.tlsProxy.caSHA256),
		authSecret(tlsProxyBadCAAuthSecret, d.tlsProxy.authority, tlsProxyBadCASHA256),
		authSecret(tlsProxyBadAuthoritySecret, tlsProxyWrongAuthority, d.tlsProxy.caSHA256),
		map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"namespace": namespace, "name": d.in.TLSProxyService, "labels": labels},
			"spec": map[string]any{
				"replicas": int64(1),
				"selector": map[string]any{"matchLabels": labels},
				"template": map[string]any{
					"metadata": map[string]any{"labels": labels},
					"spec": map[string]any{
						"automountServiceAccountToken":  false,
						"enableServiceLinks":            false,
						"imagePullSecrets":              []any{map[string]any{"name": registryPullSecret}},
						"terminationGracePeriodSeconds": int64(10),
						"securityContext": map[string]any{
							"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532),
							"fsGroup": int64(65532), "fsGroupChangePolicy": "OnRootMismatch",
							"seccompProfile": map[string]any{"type": "RuntimeDefault"},
						},
						"containers": []any{map[string]any{
							"name": "tls-registry-proxy", "image": d.in.FixtureImage, "imagePullPolicy": "IfNotPresent",
							"command": []any{"/e2e-handcraft-oci"},
							"args": []any{
								"tls-proxy", "--listen=:5443", "--upstream=http://" + d.registryHost,
								"--cert-file=/tls/tls.crt", "--key-file=/tls/tls.key",
							},
							"ports": []any{
								map[string]any{"name": "tls", "containerPort": int64(5443), "protocol": "TCP"},
								map[string]any{"name": "admin", "containerPort": int64(8081), "protocol": "TCP"},
							},
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
								"limits":   map[string]any{"cpu": "100m", "memory": "64Mi"},
							},
							"readinessProbe": map[string]any{
								"tcpSocket": map[string]any{"port": "tls"}, "initialDelaySeconds": int64(1), "periodSeconds": int64(2),
							},
							"livenessProbe": map[string]any{
								"tcpSocket": map[string]any{"port": "admin"}, "initialDelaySeconds": int64(2), "periodSeconds": int64(5),
							},
							"securityContext": restricted,
							"volumeMounts":    []any{map[string]any{"name": "tls", "mountPath": "/tls", "readOnly": true}},
						}},
						"volumes": []any{map[string]any{
							"name": "tls",
							"secret": map[string]any{
								"secretName": tlsProxyCertSecret, "defaultMode": int64(288),
								"items": []any{
									map[string]any{"key": "tls.crt", "path": "tls.crt", "mode": int64(288)},
									map[string]any{"key": "tls.key", "path": "tls.key", "mode": int64(288)},
								},
							},
						}},
					},
				},
			},
		},
		map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"namespace": namespace, "name": d.in.TLSProxyService},
			"spec": map[string]any{
				"ipFamilyPolicy": "SingleStack",
				"selector":       labels,
				"ports":          []any{map[string]any{"name": "tls", "port": int64(5443), "targetPort": "tls", "protocol": "TCP"}},
			},
		},
	)
	if err := d.cluster.WaitForRollout(d.ctx, namespace, d.in.TLSProxyService, waitTimeout); err != nil {
		d.fatalf("%v", err)
	}

	configMap := &corev1.ConfigMap{}
	d.mustGet(tlsProxyCAConfigMap, configMap)
	if configMap.Immutable == nil || !*configMap.Immutable || !slices.Equal(slices.Sorted(maps.Keys(configMap.Data)), []string{"ca.pem"}) ||
		len(configMap.BinaryData) != 0 || configMap.Data["ca.pem"] != string(caBundle) {
		d.fatalf("TLS proxy CA ConfigMap is not an immutable single-key trust bundle")
	}
	secrets := map[string]*corev1.Secret{}
	for _, name := range []string{tlsProxyGoodAuthSecret, tlsProxyBadCAAuthSecret, tlsProxyBadAuthoritySecret} {
		secret := &corev1.Secret{}
		d.mustGet(name, secret)
		secrets[name] = secret
	}
	if err := tlsProxyAuthSecretsOrthogonal(secrets, d.tlsProxy); err != nil {
		d.fatalf("TLS proxy auth Secrets lost their orthogonal fixed authority and CA grants: %v", err)
	}
	deployment := &appsv1.Deployment{}
	d.mustGet(d.in.TLSProxyService, deployment)
	if err := tlsProxyDeploymentHardened(deployment, d.in.FixtureImage, "http://"+d.registryHost); err != nil {
		d.fatalf("TLS registry proxy Deployment lost its hardened credential-free contract: %v", err)
	}
	service := &corev1.Service{}
	d.mustGet(d.in.TLSProxyService, service)
	if !tlsProxyServiceSingleStack(service, d.in.TLSProxyService) {
		d.fatalf("TLS proxy Service %s lost its exact single-stack routing contract", d.in.TLSProxyService)
	}
}

// waitForDatabase waits for a database server to answer a query as the
// application user.
func (d *dataPlane) waitForDatabase(engine string) {
	d.t.Helper()
	query := d.psqlDefault
	if engine == "mysql" {
		query = d.mysql
	}
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if result, err := query("SELECT 1"); err == nil && removeWhitespace(result) == "1" {
			return
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("%s did not become queryable", engine)
}

var (
	postgresServerVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,2}$`)
	mysqlServerVersion    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([._+-][0-9A-Za-z.-]+)*$`)
)

func (d *dataPlane) reportDatabaseVersions() {
	d.t.Helper()
	postgres, err := d.psqlDefault("SHOW server_version")
	d.check(err, "read the PostgreSQL server version")
	postgres = removeLineBreaks(postgres)
	if !postgresServerVersion.MatchString(postgres) {
		d.fatalf("PostgreSQL returned an unexpected server_version")
	}
	mysql, err := d.mysql("SELECT VERSION()")
	d.check(err, "read the MySQL server version")
	mysql = removeLineBreaks(mysql)
	if !mysqlServerVersion.MatchString(mysql) {
		d.fatalf("MySQL returned an unexpected server version")
	}
	d.logf("PostgreSQL server version %s", postgres)
	d.logf("MySQL server version %s", mysql)
}

// createCustomCADatabase gives the custom-CA rows a database of their own on
// the lifecycle's server, under a Secret distinct from the lifecycle's.
func (d *dataPlane) createCustomCADatabase() {
	d.t.Helper()
	primary, custom := &corev1.Secret{}, &corev1.Secret{}
	d.mustGet(pgSecret, primary)
	d.mustGet(customCAPGSecret, custom)
	if custom.Immutable == nil || !*custom.Immutable || custom.Type != corev1.SecretTypeOpaque ||
		!slices.Equal(slices.Sorted(maps.Keys(custom.Data)), []string{"database", "password", "url", "username"}) ||
		string(custom.Data["username"]) != pgUser || string(custom.Data["database"]) != customCAPGDatabase ||
		string(custom.Data["url"]) != d.credentials.customCAPGURL ||
		!bytes.Equal(custom.Data["password"], primary.Data["password"]) {
		d.fatalf("custom-CA PostgreSQL Secret lost its distinct fixed target binding")
	}
	count, err := d.psqlDefault("SELECT count(*) FROM pg_database WHERE datname='" + customCAPGDatabase + "'")
	d.check(err, "look the custom-CA PostgreSQL database up")
	switch removeWhitespace(count) {
	case "0":
		_, err := d.psqlDefault("CREATE DATABASE " + customCAPGDatabase)
		d.check(err, "create the custom-CA PostgreSQL database")
	case "1":
	default:
		d.fatalf("custom-CA PostgreSQL database lookup returned an unexpected result")
	}
	current, err := d.psql(customCAPGDatabase, "SELECT current_database()")
	d.check(err, "query the custom-CA PostgreSQL database")
	if removeWhitespace(current) != customCAPGDatabase {
		d.fatalf("custom-CA PostgreSQL database did not become independently queryable")
	}
}

// createIsolatedPostgreSQLDatabase gives one row an empty PostgreSQL database
// of its own on the lifecycle's server, and an immutable Secret whose url names
// it. A PtahSchema declares the whole database: a schema planned against a
// database another schema already converged plans to drop that schema's tables
// too, the plan is destructive, allowDestructive is false, and the resource is
// Blocked with DestructiveChangesDisabled instead of awaiting approval. The
// name is under the ptah_e2e_ prefix and never the lifecycle's or the
// custom-CA fixture's, and a database that already exists fails the row: the
// proof needs one that holds nothing.
func (d *dataPlane) createIsolatedPostgreSQLDatabase(database, secret, url string) {
	d.t.Helper()
	if !strings.HasPrefix(database, "ptah_e2e_") {
		d.fatalf("isolated PostgreSQL database %s is not named under the ptah_e2e_ prefix", database)
	}
	if database == pgDatabase || database == customCAPGDatabase || secret == pgSecret || secret == customCAPGSecret {
		d.fatalf("isolated PostgreSQL database %s reuses the lifecycle's or the custom-CA database or Secret", database)
	}
	if url == "" {
		d.fatalf("isolated PostgreSQL database %s has no URL to hand its Secret", database)
	}
	count, err := d.psqlDefault("SELECT count(*) FROM pg_database WHERE datname='" + database + "'")
	d.check(err, "look PostgreSQL database %s up", database)
	if removeWhitespace(count) != "0" {
		d.fatalf("PostgreSQL database %s already exists, and the row needs one that holds nothing", database)
	}
	_, err = d.psqlDefault("CREATE DATABASE " + database)
	d.check(err, "create PostgreSQL database %s", database)
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": secret},
		"data":     secretData(map[string]string{"database": database, "url": url}),
	})
}

// isolatedPostgreSQLTableCount counts one table in one database on the
// lifecycle's server, so a row reads the database it converged rather than the
// lifecycle's.
func (d *dataPlane) isolatedPostgreSQLTableCount(database, table string) string {
	d.t.Helper()
	count, err := d.psql(database, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='"+table+"'")
	d.check(err, "count table %s in %s", table, database)
	return removeWhitespace(count)
}

// createAdmissionFixtures installs what admission adds to every operation Pod
// in the namespace: a LimitRange's defaults, a RuntimeClass with overhead, a
// node selector and a toleration, and the default ServiceAccount's pull
// Secret. The Job audit holds every admitted Pod to carrying them.
func (d *dataPlane) createAdmissionFixtures() {
	d.t.Helper()
	d.mustApply(
		map[string]any{
			"apiVersion": "v1", "kind": "LimitRange",
			"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": "ptah-operation-defaults"},
			"spec": map[string]any{"limits": []any{map[string]any{
				"type":           "Container",
				"defaultRequest": map[string]any{"cpu": "10m", "memory": "16Mi"},
				"default":        map[string]any{"cpu": "100m", "memory": "64Mi"},
			}}},
		},
		map[string]any{
			"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass",
			"metadata": map[string]any{"name": admissionRuntimeClass},
			"handler":  "runc",
			"overhead": map[string]any{"podFixed": map[string]any{"memory": "8Mi"}},
			"scheduling": map[string]any{
				"nodeSelector": map[string]any{"kubernetes.io/os": "linux"},
				"tolerations": []any{map[string]any{
					"key": admissionRuntimeTaint, "operator": "Exists", "effect": "NoSchedule",
				}},
			},
		},
	)
	account := &corev1.ServiceAccount{}
	account.Namespace, account.Name = d.in.TestNamespace, "default"
	d.check(d.mergePatch(account, map[string]any{"imagePullSecrets": []any{map[string]any{"name": registryPullSecret}}}),
		"give the default ServiceAccount the registry pull Secret")
}

// createDigestPinPolicyFixture installs the verification policy that refuses a
// mutable requested reference, and holds the digest-pin row's Docker config
// Secret to the grants its owner fixed.
func (d *dataPlane) createDigestPinPolicyFixture() {
	d.t.Helper()
	policy, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy-digest-pin.yaml"))
	if err != nil {
		d.fatalf("digest-pin verification policy fixture is missing")
	}
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": digestPinPolicyName},
		"data":     map[string]any{"policy.yaml": string(policy)},
	})
	configMap := &corev1.ConfigMap{}
	d.mustGet(digestPinPolicyName, configMap)
	if configMap.Immutable == nil || !*configMap.Immutable || !strings.Contains(configMap.Data["policy.yaml"], "require_digest_pin: true") {
		d.fatalf("digest-pin verification policy ConfigMap is not immutable or strict")
	}
	secret := &corev1.Secret{}
	d.mustGet(digestPinDockerAuthSecret, secret)
	if err := digestPinDockerSecretExact(secret, d.registryHost, d.registry.Username, d.registry.Password); err != nil {
		d.fatalf("digest-pin Docker config Secret lost its fixed credential-owner grants: %v", err)
	}
}

// createExternalPostgresqlEndpoint routes a selectorless Service to the
// external PostgreSQL container on the kind network, hands the schema a URL
// Secret for it, and holds all three and the container to the shape that makes
// the database genuinely external: nothing in Kubernetes hosts it, and it is
// empty, owned by the login Ptah is given, and reached with no superuser.
func (d *dataPlane) createExternalPostgresqlEndpoint() {
	d.t.Helper()
	d.logf("creating selectorless external PostgreSQL endpoint")
	namespace, service, owner := d.in.TestNamespace, d.in.ExternalPostgresService, d.in.ExternalPostgresOwner
	externalLabels := map[string]any{
		"app.kubernetes.io/component": "e2e-external-database", "operator.ptah.run/e2e-owner": owner,
	}
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
		"metadata": map[string]any{"namespace": namespace, "name": externalPGSecret, "labels": externalLabels},
		"data":     secretData(map[string]string{"url": d.external.URL}),
	})
	serviceLabels := maps.Clone(externalLabels)
	serviceLabels["app.kubernetes.io/name"] = service
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"namespace": namespace, "name": service, "labels": serviceLabels},
		"spec": map[string]any{"ports": []any{map[string]any{
			"name": "postgresql", "port": int64(5432), "protocol": "TCP", "targetPort": int64(5432),
		}}},
	})
	route := &corev1.Service{}
	d.mustGet(service, route)
	if route.UID == "" {
		d.fatalf("external PostgreSQL Service has no UID")
	}
	sliceLabels := maps.Clone(externalLabels)
	sliceLabels["kubernetes.io/service-name"] = service
	sliceLabels["endpointslice.kubernetes.io/managed-by"] = "ptah-operator-e2e"
	d.mustCreate(map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata": map[string]any{
			"namespace": namespace, "name": service + "-docker", "labels": sliceLabels,
			"ownerReferences": []any{map[string]any{
				"apiVersion": "v1", "kind": "Service", "name": service, "uid": string(route.UID),
				"controller": true, "blockOwnerDeletion": false,
			}},
		},
		"addressType": "IPv4",
		"endpoints": []any{map[string]any{
			"addresses": []any{d.in.ExternalPostgresIP}, "conditions": map[string]any{"ready": true},
		}},
		"ports": []any{map[string]any{"name": "postgresql", "port": int64(5432), "protocol": "TCP"}},
	})

	secret := &corev1.Secret{}
	d.mustGet(externalPGSecret, secret)
	if secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeOpaque ||
		secret.Labels["app.kubernetes.io/component"] != "e2e-external-database" ||
		secret.Labels["operator.ptah.run/e2e-owner"] != owner ||
		!slices.Equal(slices.Sorted(maps.Keys(secret.Data)), []string{"url"}) || string(secret.Data["url"]) != d.external.URL {
		d.fatalf("external PostgreSQL Secret lost its exact URL-only binding")
	}
	d.mustGet(service, route)
	if route.Spec.Selector != nil || route.Labels["app.kubernetes.io/component"] != "e2e-external-database" ||
		route.Labels["operator.ptah.run/e2e-owner"] != owner || len(route.Spec.Ports) != 1 ||
		route.Spec.Ports[0].Name != "postgresql" || route.Spec.Ports[0].Port != 5432 ||
		route.Spec.Ports[0].Protocol != corev1.ProtocolTCP || route.Spec.Ports[0].TargetPort != intstr.FromInt32(5432) {
		d.fatalf("external PostgreSQL Service is not an exact selectorless route")
	}
	slice := &discoveryv1.EndpointSlice{}
	d.mustGet(service+"-docker", slice)
	if err := externalEndpointSliceExact(slice, service, route.UID, owner, d.in.ExternalPostgresIP); err != nil {
		d.fatalf("external PostgreSQL EndpointSlice lost its exact Docker route: %v", err)
	}
	d.assertExternalPGNotHostedInKubernetes()
	d.assertExternalPGContainerContract()
	d.assertExternalPGServerVersion()
	if count := removeWhitespace(d.externalPGQuery(
		"SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='e2e_widgets'")); count != "0" {
		d.fatalf("external PostgreSQL fixture was not empty before reconciliation")
	}
	if superuser := removeWhitespace(d.externalPGQuery("SELECT rolsuper FROM pg_roles WHERE rolname = current_user")); superuser != "f" {
		d.fatalf("external PostgreSQL fixture login is a superuser")
	}
	if owns := removeWhitespace(d.externalPGQuery(
		"SELECT pg_get_userbyid(datdba) = current_user FROM pg_database WHERE datname = current_database()")); owns != "t" {
		d.fatalf("external PostgreSQL fixture login does not retain database ownership")
	}
}

func (d *dataPlane) inspectContainer(id string) dockerInspection {
	d.t.Helper()
	output, err := d.docker("container", "inspect", id)
	d.check(err, "inspect container %s", id)
	var inspections []dockerInspection
	if err := json.Unmarshal([]byte(output), &inspections); err != nil || len(inspections) != 1 {
		d.fatalf("container %s did not inspect as exactly one container: %v", id, err)
	}
	return inspections[0]
}

// assertRegistryContainerContract holds the registry container to its
// identity, its running state, its task labels and, while it runs, its one
// address on the kind network.
func (d *dataPlane) assertRegistryContainerContract(running bool) {
	d.t.Helper()
	inspection := d.inspectContainer(d.in.RegistryContainerID)
	switch {
	case inspection.ID != d.in.RegistryContainerID:
		d.fatalf("registry container identity changed")
	case inspection.State.Running != running:
		d.fatalf("registry container running state is not %t", running)
	case inspection.Config.Labels["operator.ptah.run/e2e-owner"] != d.in.ExternalPostgresOwner:
		d.fatalf("registry container lost its task owner label")
	case inspection.Config.Labels["operator.ptah.run/e2e-component"] != "registry":
		d.fatalf("registry container lost its component label")
	}
	if running && !onlyKindNetwork(inspection, d.in.RegistryIP) {
		d.fatalf("registry container left its exact kind-network address")
	}
}

// assertExternalPGContainerContract holds the external PostgreSQL container to
// what makes it a disposable database outside the cluster: its identity and
// image, no restart policy, no published port, its data on an exact tmpfs and
// nothing persistent, and its one address on the kind network.
func (d *dataPlane) assertExternalPGContainerContract() {
	d.t.Helper()
	inspection := d.inspectContainer(d.in.ExternalPostgresContainerID)
	if err := externalContainerContract(inspection, d.in.ExternalPostgresContainerID, d.in.ExternalPostgresImage,
		d.in.ExternalPostgresOwner, d.in.ExternalPostgresIP); err != nil {
		d.fatalf("%v", err)
	}
}

// externalPGQuery asks the external database a question as the login Ptah is
// given: the container's superuser created that login, and asking as it would
// contradict the least-privilege checks the phase makes about the fixture.
func (d *dataPlane) externalPGQuery(query string) string {
	d.t.Helper()
	d.assertExternalPGContainerContract()
	output, err := d.docker("exec", "--env", "PGPASSWORD="+d.external.Password, d.in.ExternalPostgresContainerID,
		"psql", "-h", "127.0.0.1", "-U", d.external.Username, "-d", d.external.Database,
		"-v", "ON_ERROR_STOP=1", "-Atqc", query)
	if err != nil {
		d.scan([]byte(err.Error()), "the external PostgreSQL query error")
		d.fatalf("external PostgreSQL query failed: %v", err)
	}
	return output
}

var externalVersion = regexp.MustCompile(`^17[0-9]{4}$`)

func (d *dataPlane) assertExternalPGServerVersion() {
	d.t.Helper()
	if !externalVersion.MatchString(removeWhitespace(d.externalPGQuery("SHOW server_version_num"))) {
		d.fatalf("external PostgreSQL fixture is not major version 17")
	}
}

// assertExternalPGNotHostedInKubernetes holds the namespace to hosting no
// workload that is, or impersonates, the external database.
func (d *dataPlane) assertExternalPGNotHostedInKubernetes() {
	d.t.Helper()
	deployments, statefulSets, pods, jobs := &appsv1.DeploymentList{}, &appsv1.StatefulSetList{}, &corev1.PodList{}, &batchv1.JobList{}
	d.mustList(deployments)
	d.mustList(statefulSets)
	d.mustList(pods)
	d.mustList(jobs)
	if err := externalNotHosted(deployments.Items, statefulSets.Items, pods.Items, jobs.Items,
		d.in.ExternalPostgresService, d.in.ExternalPostgresImage); err != nil {
		d.fatalf("a Kubernetes workload hosts or impersonates external PostgreSQL: %v", err)
	}
}
