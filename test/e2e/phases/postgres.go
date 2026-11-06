package phases

import (
	"fmt"
	"regexp"
	"strconv"
)

// postgresImageTag reads the major version out of a digest-pinned image's tag.
// hack/e2e-kind.sh carries the same expression in postgres_image_major, and
// hack/database_lines_test.go holds the two to one table of images.
var postgresImageTag = regexp.MustCompile(`^[^@]*:([1-9][0-9]*)[^/:@]*@sha256:[0-9a-f]{64}$`)

// PostgresImageMajor is the PostgreSQL major version a digest-pinned image names
// in its tag. The release line a lifecycle runs on arrives as that one image
// reference, and the acceptance holds the server it reaches to this number, so a
// tag whose digest belongs to another line fails the run instead of reporting
// the wrong line as proven.
func PostgresImageMajor(image string) (int, error) {
	match := postgresImageTag.FindStringSubmatch(image)
	if match == nil {
		return 0, fmt.Errorf("%q does not name a PostgreSQL major version in a digest-pinned tag", image)
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("%q names major version %q: %w", image, match[1], err)
	}
	return major, nil
}

// PostgresDataDirectory is the directory the official image of a major version
// declares as its volume, which PostgreSQL 18's image moved up one level. A
// mount anywhere else leaves Docker an anonymous volume at the declared path,
// and with the image's own PGDATA a mount at the old path stops 18 from starting.
func PostgresDataDirectory(major int) string {
	if major >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}
