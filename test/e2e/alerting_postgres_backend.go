package e2e

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

var alBackendStartEpoch = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,6})?$`)
var alBackendDatabase = regexp.MustCompile(`^[a-z][a-z0-9_]+$`)

// The database returns these fields in one reading while the original Pod is
// still running. The start timestamp prevents a reused PID from identifying a
// later connection as the interrupted writer.
func alPostgresBackendTermination(database, reading string, pod *corev1.Pod) (string, error) {
	fields := strings.Split(reading, "/")
	if len(fields) != 3 || pod == nil || !alBackendDatabase.MatchString(database) ||
		!executorBackendMatchesPod(strings.Join(fields[:2], "/"), pod, pod.UID) || !alBackendStartEpoch.MatchString(fields[2]) {
		return "", errors.New("PostgreSQL backend lacks its original Pod and session identity")
	}
	started, err := strconv.ParseFloat(fields[2], 64)
	pid, pidErr := strconv.ParseInt(fields[0], 10, 32)
	if err != nil || started <= 0 || math.IsInf(started, 0) || pidErr != nil || pid <= 0 {
		return "", errors.New("PostgreSQL backend has an invalid process or start time")
	}
	return fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid=%d AND datname='%s' AND usename='%s' AND host(client_addr)='%s' AND extract(epoch FROM backend_start)=%s", pid, database, migrationDatabaseUser, fields[1], fields[2]), nil
}
