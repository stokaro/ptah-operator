//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// databaseSQL runs one statement on an engine's server in the database given,
// as the server's own administrator, and returns what the client printed. It
// is hack/e2e-sql.sh's statement shape, which the migration and reference-data
// phases shared: one definition, so the two cannot come to read a database
// differently.
func databaseSQL(ctx context.Context, cluster *harness.Cluster, namespace string, engine migrationEngine,
	database, statement string,
) (string, error) {
	var command []string
	switch engine.name {
	case "postgresql":
		command = []string{"sh", "-ec",
			`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -Atqc "$2"`,
			"sh", database, statement}
	case "mysql":
		command = []string{"sh", "-ec",
			`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"`,
			"sh", database, statement}
	default:
		return "", fmt.Errorf("unsupported engine %q", engine.name)
	}
	return engineExec(ctx, cluster, namespace, engine, command)
}

// serverSQL runs one statement against the server's own database as its
// administrator, for the statements that create and drop databases.
func serverSQL(ctx context.Context, cluster *harness.Cluster, namespace string, engine migrationEngine,
	statement string,
) (string, error) {
	var command []string
	switch engine.name {
	case "postgresql":
		command = []string{"sh", "-ec",
			`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atqc "$1"`,
			"sh", statement}
	case "mysql":
		command = []string{"sh", "-ec",
			`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -Nse "$1"`,
			"sh", statement}
	default:
		return "", fmt.Errorf("unsupported engine %q", engine.name)
	}
	return engineExec(ctx, cluster, namespace, engine, command)
}

// engineExec runs a client command inside the engine's database Deployment.
// A failure carries what the client said, which names the statement's fault.
func engineExec(ctx context.Context, cluster *harness.Cluster, namespace string, engine migrationEngine,
	command []string,
) (string, error) {
	stdout, stderr, err := cluster.Kubectl(ctx,
		append([]string{"-n", namespace, "exec", "deployment/" + engine.service, "--"}, command...)...)
	if err != nil {
		return string(stdout), fmt.Errorf("exec in deployment/%s: %w: %s", engine.service, err, strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}
