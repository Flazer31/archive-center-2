package store

import (
	"errors"

	"github.com/go-sql-driver/mysql"
)

// conflictRetries is how often a management write runs again when another
// write changed the rows it read (full review R4, 11-05: a C16 edit and an
// unmerge in flight together made the unmerge fail with 1020).
const conflictRetries = 3

// againOnConflict runs a whole transaction again on MariaDB's "record has
// changed since last read" (1020) and deadlock (1213) errors. The transaction
// rolled back, so running it again from the start reads the newer rows.
func againOnConflict[T any](run func() (T, error)) (T, error) {
	for i := 0; ; i++ {
		v, err := run()
		var my *mysql.MySQLError
		if err == nil || i >= conflictRetries || !errors.As(err, &my) || (my.Number != 1020 && my.Number != 1213) {
			return v, err
		}
	}
}
