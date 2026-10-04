package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/internal/plancontract"
)

// The native planner, rather than a padded or rewritten plan document, emits
// the boundary payload. HTML-sensitive bytes let the artifact remain smaller
// than its JSON plan. A short ASCII suffix fills the serializer's remainder.
func planSizeSchema(engine string, repeated, suffix int) ([]byte, error) {
	if repeated < 0 || repeated > int(plancontract.MaxExecutableBytes)+1024 || suffix < 0 || suffix > 127 {
		return nil, errors.New("invalid plan-size fixture lengths")
	}
	var prefix, ending string
	switch engine {
	case "postgres":
		prefix = "CREATE TABLE e2e_plan_size_limit (id BIGINT NOT NULL PRIMARY KEY, payload TEXT DEFAULT '"
		ending = "');\n"
	case "mysql":
		// MySQL's TEXT default needs an expression that the pinned SQL source
		// parser does not accept. Keep each literal within VARCHAR's declared
		// length, spreading the same total payload over a fixed table set.
		const tables = 100
		if repeated/tables+(repeated%tables+tables-1)/tables+suffix > 16000 {
			return nil, errors.New("MySQL plan-size fixture exceeds its executable VARCHAR defaults")
		}
		var document strings.Builder
		for i := range tables {
			count := repeated / tables
			if i < repeated%tables {
				count++
			}
			ascii := ""
			if i == tables-1 {
				ascii = strings.Repeat("x", suffix)
			}
			fmt.Fprintf(&document, "CREATE TABLE e2e_plan_size_limit_%03d (id BIGINT NOT NULL PRIMARY KEY, payload VARCHAR(16000) DEFAULT '%s%s');\n", i, strings.Repeat("<", count), ascii)
		}
		return []byte(document.String()), nil
	default:
		return nil, errors.New("plan-size fixture needs postgres or mysql")
	}
	return []byte(prefix + strings.Repeat("<", repeated) + strings.Repeat("x", suffix) + ending), nil
}

func runPlanSizeSchema(args []string) error {
	if len(args) != 4 {
		return errors.New("usage: e2e-handcraft-oci plan-size-schema <postgres|mysql> <repeated> <suffix> <output>")
	}
	repeated, err := strconv.Atoi(args[1])
	if err != nil {
		return errors.New("invalid plan-size repeated length")
	}
	suffix, err := strconv.Atoi(args[2])
	if err != nil {
		return errors.New("invalid plan-size suffix length")
	}
	content, err := planSizeSchema(args[0], repeated, suffix)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(args[3], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create plan-size schema fixture")
	}
	_, writeErr := output.Write(content)
	closeErr := output.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("write plan-size schema fixture")
	}
	return nil
}
