package dialect

import "strings"

// ErrorKind classifies driver errors so callers can react without
// depending on driver-specific error types.
type ErrorKind int

const (
	ErrOther ErrorKind = iota
	ErrUnique
	ErrForeignKey
	ErrNotNull
	ErrCheck
)

func (k ErrorKind) String() string {
	switch k {
	case ErrUnique:
		return "unique"
	case ErrForeignKey:
		return "foreign key"
	case ErrNotNull:
		return "not null"
	case ErrCheck:
		return "check"
	}
	return "other"
}

// Classify matches on SQLSTATE codes and messages, which are stable across
// the common drivers for each database.
func (d *sqlDialect) Classify(err error) ErrorKind {
	if err == nil {
		return ErrOther
	}
	m := strings.ToLower(err.Error())
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch d.kind {
	case KindPostgres:
		switch {
		case has("23505", "duplicate key value"):
			return ErrUnique
		case has("23503", "violates foreign key"):
			return ErrForeignKey
		case has("23502", "violates not-null"):
			return ErrNotNull
		case has("23514", "violates check constraint"):
			return ErrCheck
		}
	case KindMySQL:
		switch {
		case has("error 1062", "duplicate entry"):
			return ErrUnique
		case has("error 1451", "error 1452", "foreign key constraint fails"):
			return ErrForeignKey
		case has("error 1048", "cannot be null"):
			return ErrNotNull
		case has("error 3819", "check constraint"):
			return ErrCheck
		}
	case KindSQLite:
		switch {
		case has("unique constraint failed", "primary key constraint failed"):
			return ErrUnique
		case has("foreign key constraint failed"):
			return ErrForeignKey
		case has("not null constraint failed"):
			return ErrNotNull
		case has("check constraint failed"):
			return ErrCheck
		}
	case KindMSSQL:
		switch {
		case has("violation of unique key", "violation of primary key", "cannot insert duplicate key"):
			return ErrUnique
		case has("foreign key constraint"):
			return ErrForeignKey
		case has("cannot insert the value null"):
			return ErrNotNull
		case has("check constraint"):
			return ErrCheck
		}
	}
	return ErrOther
}
