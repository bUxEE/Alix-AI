package store

import (
	"errors"

	"modernc.org/sqlite"
)

// SQLite result codes (https://www.sqlite.org/rescode.html). The driver turns
// on extended result codes, so a code is compared on its low byte.
const (
	sqliteBusy       = 5
	sqliteConstraint = 19

	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

func sqliteCode(err error) (code int, ok bool) {
	var se *sqlite.Error
	if errors.As(err, &se) && se != nil {
		return se.Code(), true
	}
	return 0, false
}

// IsBusy reports whether err is SQLITE_BUSY (or an extended variant such as
// BUSY_SNAPSHOT or BUSY_RECOVERY): another connection or process holds a lock
// past the busy timeout. DB.Tx and DB.View already retry these.
func IsBusy(err error) bool {
	c, ok := sqliteCode(err)
	return ok && c&0xff == sqliteBusy
}

// IsConstraint reports whether err is any SQLite constraint violation
// (UNIQUE, PRIMARY KEY, NOT NULL, CHECK, FOREIGN KEY).
func IsConstraint(err error) bool {
	c, ok := sqliteCode(err)
	return ok && c&0xff == sqliteConstraint
}

// IsUniqueViolation reports whether err is a UNIQUE or PRIMARY KEY violation,
// the usual "this already exists" signal behind an idempotency key.
func IsUniqueViolation(err error) bool {
	c, ok := sqliteCode(err)
	return ok && (c == sqliteConstraintUnique || c == sqliteConstraintPrimaryKey)
}
