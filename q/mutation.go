package q

// MutKind describes how a Mutation changes a column.
type MutKind int

const (
	MutSet MutKind = iota
	MutInc
	MutExpr
)

// Mutation is one SET item of an UPDATE.
type Mutation struct {
	Kind  MutKind
	Field string
	Value any    // MutSet: new value; MutInc: delta
	SQL   string // MutExpr: raw expression with '?' placeholders
	Args  []any
}

// Set assigns a value: q.Set("status", "done").
func Set(field string, v any) Mutation { return Mutation{Kind: MutSet, Field: field, Value: v} }

// Inc increments a numeric column: q.Inc("views", 1).
func Inc(field string, n any) Mutation { return Mutation{Kind: MutInc, Field: field, Value: n} }

// Dec decrements a numeric column.
func Dec(field string, n int64) Mutation { return Mutation{Kind: MutInc, Field: field, Value: -n} }

// SetExpr assigns a raw SQL expression: q.SetExpr("total", "price * ?", qty).
func SetExpr(field, sql string, args ...any) Mutation {
	return Mutation{Kind: MutExpr, Field: field, SQL: sql, Args: args}
}

// F references another column as a value: q.Eq("updated_at", q.F("created_at")).
type F string

// OnConflict configures upsert behaviour for inserts.
type OnConflict struct {
	// Columns forming the conflict target (required for Postgres/SQLite).
	Columns []string
	// DoNothing ignores conflicting rows.
	DoNothing bool
	// Update lists columns to overwrite with the incoming values.
	Update []string
}
