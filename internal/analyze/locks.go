// Package analyze predicts the locks PostgreSQL takes for each migration statement and flags risky ones.
package analyze

import (
	"encoding/json"
	"fmt"
)

// LockMode is a PostgreSQL table-level lock mode, weakest first.
type LockMode int

// Table lock modes, in the order of the PostgreSQL documentation.
const (
	NoLock LockMode = iota
	AccessShare
	RowShare
	RowExclusive
	ShareUpdateExclusive
	Share
	ShareRowExclusive
	Exclusive
	AccessExclusive
)

var lockNames = [...]string{
	"none", "ACCESS SHARE", "ROW SHARE", "ROW EXCLUSIVE", "SHARE UPDATE EXCLUSIVE",
	"SHARE", "SHARE ROW EXCLUSIVE", "EXCLUSIVE", "ACCESS EXCLUSIVE",
}

// pgNames maps lock modes to their pg_locks.mode spelling.
var pgNames = [...]string{
	"", "AccessShareLock", "RowShareLock", "RowExclusiveLock", "ShareUpdateExclusiveLock",
	"ShareLock", "ShareRowExclusiveLock", "ExclusiveLock", "AccessExclusiveLock",
}

// String returns the documentation name of the mode.
func (m LockMode) String() string {
	if m < NoLock || m > AccessExclusive {
		return fmt.Sprintf("LockMode(%d)", int(m))
	}
	return lockNames[m]
}

// MarshalJSON writes the mode as its name.
func (m LockMode) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }

// ModeFromPG parses a pg_locks.mode value.
func ModeFromPG(s string) (LockMode, bool) {
	for i, n := range pgNames {
		if i > 0 && n == s {
			return LockMode(i), true
		}
	}
	return NoLock, false
}

// conflicts is the table-level lock conflict matrix from the PostgreSQL docs (section 13.3.1).
var conflicts = [9][9]bool{
	AccessShare:          {AccessExclusive: true},
	RowShare:             {Exclusive: true, AccessExclusive: true},
	RowExclusive:         {Share: true, ShareRowExclusive: true, Exclusive: true, AccessExclusive: true},
	ShareUpdateExclusive: {ShareUpdateExclusive: true, Share: true, ShareRowExclusive: true, Exclusive: true, AccessExclusive: true},
	Share:                {RowExclusive: true, ShareUpdateExclusive: true, ShareRowExclusive: true, Exclusive: true, AccessExclusive: true},
	ShareRowExclusive:    {RowExclusive: true, ShareUpdateExclusive: true, Share: true, ShareRowExclusive: true, Exclusive: true, AccessExclusive: true},
	Exclusive:            {RowShare: true, RowExclusive: true, ShareUpdateExclusive: true, Share: true, ShareRowExclusive: true, Exclusive: true, AccessExclusive: true},
	AccessExclusive:      {AccessShare: true, RowShare: true, RowExclusive: true, ShareUpdateExclusive: true, Share: true, ShareRowExclusive: true, Exclusive: true, AccessExclusive: true},
}

// Conflicts reports whether two lock modes cannot be held at once by different transactions.
func Conflicts(a, b LockMode) bool {
	if a <= NoLock || b <= NoLock || a > AccessExclusive || b > AccessExclusive {
		return false
	}
	return conflicts[a][b]
}

// BlocksReads reports whether plain SELECTs (ACCESS SHARE) wait for this lock.
func (m LockMode) BlocksReads() bool { return Conflicts(m, AccessShare) }

// BlocksWrites reports whether INSERT/UPDATE/DELETE (ROW EXCLUSIVE) wait for this lock.
func (m LockMode) BlocksWrites() bool { return Conflicts(m, RowExclusive) }
