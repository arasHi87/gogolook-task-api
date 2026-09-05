package postgres

import (
	"strconv"
	"strings"
)

// upMigrationVersion parses golang-migrate's file naming convention,
// <version>_<name>.up.sql, and reports the version of an up migration.
//
// Down migrations and anything else return false, so counting pending work
// does not double-count every migration.
func upMigrationVersion(name string) (uint, bool) {
	if !strings.HasSuffix(name, ".up.sql") {
		return 0, false
	}
	digits, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return uint(v), true
}
