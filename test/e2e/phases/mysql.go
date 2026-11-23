package phases

import (
	"fmt"
	"regexp"
	"strconv"
)

// mysqlImageTag reads the release line out of a digest-pinned MySQL image's tag:
// the major and minor version, such as 8.4 or 9.7. hack/e2e-kind.sh carries the
// same expression in mysql_image_line, and hack/database_lines_test.go holds the
// two to one table of images.
var mysqlImageTag = regexp.MustCompile(`^[^@]*:([1-9][0-9]*\.[0-9]+)([.-][^/:@]*)?@sha256:[0-9a-f]{64}$`)

// MySQLImageLine is the MySQL release line a digest-pinned image names in its
// tag, as major.minor. MySQL numbers a line by both, so 9.7 and 9.6 are
// different lines, and a tag that names only a major names no line.
func MySQLImageLine(image string) (string, error) {
	match := mysqlImageTag.FindStringSubmatch(image)
	if match == nil {
		return "", fmt.Errorf("%q does not name a MySQL release line in a digest-pinned tag", image)
	}
	return match[1], nil
}

// DatabaseImageLine is the release line a digest-pinned image of engine names in
// its tag: the major version for PostgreSQL, major.minor for MySQL. A job of the
// added database lines is named after it, and a release reads the job by that
// name.
func DatabaseImageLine(engine, image string) (string, error) {
	switch engine {
	case "postgresql":
		major, err := PostgresImageMajor(image)
		if err != nil {
			return "", err
		}
		return strconv.Itoa(major), nil
	case "mysql":
		return MySQLImageLine(image)
	default:
		return "", fmt.Errorf("engine %q has no release line in an image tag", engine)
	}
}
