# kiln

A typed ORM, migration toolkit and code generator for Go, built on plain `database/sql`.
It supports **PostgreSQL, MySQL/MariaDB, SQLite and SQL Server**.

kiln started as a migration generator and grew into a full data layer inspired by
[go-rel/rel](https://github.com/go-rel/rel). It adds generics, auto-generated migrations,
cursor pagination and more ([comparison](#compared-with-other-go-libraries)).

```go
users := orm.For[User](db)

u, err := users.FindByID(ctx, 1)
page, err := users.Cursor(ctx, orm.CursorOpts{Limit: 20, Sort: "-created_at"}, q.Eq("active", true))
err = db.Transaction(ctx, func(ctx context.Context) error {
	return users.Insert(ctx, &User{Name: "Ada", Profile: &Profile{Bio: "..."}}, orm.Cascade())
})
```

```bash
kiln create Post title:string body:text user:references   # model + migration
# ...edit internal/domain/post.go by hand...
kiln diff add_post_slug                                    # migration written for you
kiln migrate up
```

## Features

**Data access**
- Typed repositories with generics: `orm.For[User](db)` returns `Repo[User]`; no `interface{}`, no casts.
- Plain structs as models. No base type to embed, no generated model code.
- Filters, sorting, joins, aggregates and sub-queries, with compile-time checked columns
  (`kiln gen`) or plain strings.
- Associations: belongs-to, has-one, has-many, many-to-many; batched preloading; cascade insert.
- Transactions in the `ctx`, nested ones as savepoints; `db.Begin` when a callback doesn't fit;
  automatic retry of deadlocks and serialization failures (`WithRetry`).
- Soft delete, timestamps, lifecycle hooks, change tracking, upserts, composite keys.
- Optimistic locking with one tag (`db:"version,lock"`): concurrent edits can't overwrite each other.
- Offset pagination with totals, keyset (cursor) pagination in both directions, streaming with `Each`.
- Read replicas, a pinned connection when you need session state, portable constraint errors.
- A default query timeout, so one stuck query can't exhaust the connection pool.
- Raw SQL is always possible (`db.Query`, `db.Exec`), so the ORM never boxes you in.
- OpenTelemetry tracing and Prometheus metrics with one line each (`kilnotel`, `kilnprom`).

**Performance**
- About 0.6 µs of overhead per query over hand-written `database/sql` code
  ([Performance](#performance)).
- `InsertAll` sends rows in batches on every database: 3–13× faster than one by one.
- Optional query result cache (memory or Redis) with automatic invalidation, and a prepared
  statement cache.
- Features you don't use cost nothing; allocation limits are checked in CI on every change.

**Migrations and tooling**
- Migrations written for you from model changes (`kiln diff`), without a database or against
  the live one.
- Generators that create models and edit your structs (`kiln create`, `add-column`, ...).
- `kiln lint` catches risky migrations (locks, data loss) before they reach production.
- Migration lock across processes, dry-run SQL, status, redo; models from an existing
  database (`kiln pull`); seeders.

**Testing**
- `ormtest`: typed mocks of your repositories, no database needed.
- `kilntest`: each test runs in a transaction that is rolled back at the end, plus factories.

**Extending**
- Plugins enabled with one line: add filters, set values on write, observe every query.
- Multi-tenancy built in (`tenant.Plugin("tenant_id")`): every query is scoped to the tenant in
  the `ctx`, and a missing tenant is an error, never a leak. Tracing and metrics plugins are on
  the way.

**Go-first design**
- Built on `database/sql`; the core has no dependencies, not even drivers.
- `ctx` first everywhere, errors you can check with `errors.Is`/`errors.As`, no panics while
  serving requests, no global configuration.
- Works on PostgreSQL, MySQL/MariaDB, SQLite and SQL Server; tested against all four in CI.

## Contents

Start with [Features](#features), or jump in:

1. [Install](#install)
2. [Getting started](#getting-started): a project from zero to a running query
3. [How it works](#how-it-works)
4. [CLI reference](#cli-reference)
5. [Models](#models)
6. [Querying](#querying)
7. [Writing data](#writing-data)
8. [Associations](#associations)
9. [Transactions](#transactions)
10. [Pagination](#pagination)
11. [Hooks, scopes, logging, replicas](#hooks-scopes-logging-replicas)
12. [Plugins](#plugins): one-line extensions, multi-tenancy, and writing your own
13. [Caching](#caching): query results (memory or Redis) and prepared statements
14. [Migrations](#migrations)
15. [Auto-generated migrations](#auto-generated-migrations)
16. [Migration safety checks](#migration-safety-checks): `kiln lint` catches risky changes before production
17. [Seeds](#seeds)
18. [Testing your code](#testing-your-code)
19. [Database notes](#database-notes)
20. [Troubleshooting](#troubleshooting)
21. [Compared with other Go libraries](#compared-with-other-go-libraries)
22. [Performance](#performance)
23. [Developing kiln](#developing-kiln)
24. [License](#license)

---

## Install

Install the CLI and add the library to your module:

```bash
go install github.com/mhdaslam790/kiln@latest   # the `kiln` command
go get github.com/mhdaslam790/kiln@latest       # the library, inside your module
```

The library has **no driver dependencies**, so install the driver for your database yourself:

| Database | Driver (`go get`) | `sql.Open` name | Example DSN |
|---|---|---|---|
| PostgreSQL | `github.com/jackc/pgx/v5/stdlib` | `pgx` | `postgres://user:pass@localhost:5432/app?sslmode=disable` |
| MySQL / MariaDB | `github.com/go-sql-driver/mysql` | `mysql` | `user:pass@tcp(localhost:3306)/app?parseTime=true` |
| SQLite (3.35+) | `modernc.org/sqlite` | `sqlite` | `file:app.db?_pragma=foreign_keys(1)` |
| SQL Server | `github.com/microsoft/go-mssqldb` | `sqlserver` | `sqlserver://sa:pass@localhost:1433?database=app` |

> MySQL needs `parseTime=true` so that `DATETIME` columns scan into `time.Time`.
> SQLite needs `foreign_keys(1)` for foreign keys to be enforced.

---

## Getting started

This walkthrough uses PostgreSQL. To use another database, pass a different `--dialect` and
use that database's DSN from the table above.

### 1. Initialise the project

Run this inside a Go module (a directory with a `go.mod`):

```bash
kiln init --dialect postgres
go get github.com/mhdaslam790/kiln github.com/jackc/pgx/v5/stdlib
```

This creates:

```
kiln.json                          # configuration (see below)
database/migrations/migrations.go  # `var Set migrate.Set`: every migration registers here
database/seeds/seeds.go            # `var Set seed.Set`: every seeder registers here
cmd/migrate/main.go                # a small program that runs migrations and seeds; edit it freely
.env                               # your DATABASE_URL; added to .gitignore, never committed
```

Models go in `internal/domain` (created by your first `kiln create`). See
[Project layout](#project-layout-clean-architecture) for how the folders fit together.

`kiln.json`:

```json
{
  "dialect": "postgres",
  "migrations": "database/migrations",
  "models": "internal/domain",
  "runner": "cmd/migrate",
  "seeds": "database/seeds",
  "repositories": "internal/repository",
  "usecases": "internal/usecase",
  "tenant_column": "tenant_id"
}
```

`tenant_column` is optional: set it when you use the [tenant plugin](#multi-tenancy-tenant)
and `kiln lint` also checks that tenant tables are indexed.

Prefer models at the top level without layers? `kiln init --layout flat` uses `models/`
instead of `internal/domain`. Every path is a setting, so you can also pick your own.

### 2. Generate models and migrations

```bash
kiln create User name:string email:string:unique bio:text:null --soft-delete
kiln create Post title:string:limit=200 body:text user:references published:bool:index
```

Each `create` writes a model and a migration:

```go
// internal/domain/post.go
type Post struct {
	ID        int64     `db:"id,primary"`
	Title     string    `db:"title" schema:"limit:200"`
	Body      string    `db:"body" schema:"type:text"`
	UserID    int64     `db:"user_id" schema:"references:users"`
	Published bool      `db:"published" schema:"index"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// database/migrations/20261123120000_create_posts.go
func init() {
	Set.Add(20261123120000, "create_posts",
		func(s *schema.Schema) {
			s.CreateTable("posts", func(t *schema.Table) {
				t.ID("id")
				t.String("title", schema.Required(), schema.Limit(200))
				t.Text("body", schema.Required())
				t.References("user_id", "users", schema.Required())
				t.Bool("published", schema.Required())
				t.Index([]string{"published"})
				t.Timestamps()
			})
		},
		func(s *schema.Schema) { s.DropTable("posts") },
	)
}
```

Migrations are plain Go files, so you can edit them, review them, and keep them in version control.

### 3. Run the migrations

Put your database URL in `.env` once (`kiln init` created it with an example):

```bash
# .env
DATABASE_URL=postgres://user:pass@localhost:5432/app?sslmode=disable
```

Every kiln command reads it, so there is nothing to export in each terminal:

```bash
kiln migrate sql      # preview the SQL (dry run)
kiln migrate up       # apply
kiln migrate status   # see what is applied
```

`kiln migrate` runs `go run ./cmd/migrate`. In production, build that program into a
binary and run it directly, with no CLI needed:

```bash
go build -o bin/migrate ./cmd/migrate
DATABASE_URL=... ./bin/migrate up
```

A real environment variable always wins over `.env`, so production servers that set
`DATABASE_URL` are never affected by a `.env` file.

### 4. Use the ORM in your application

```go
package main

import (
	"context"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/env"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"

	"example.com/app/internal/domain"
)

func main() {
	env.Load() // reads .env if present; real environment variables win
	ctx := context.Background()
	db, err := orm.Connect(ctx, "pgx", os.Getenv("DATABASE_URL"), dialect.Postgres())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	users := orm.For[domain.User](db)
	u := domain.User{Name: "Ada", Email: "ada@example.com"}
	if err := users.Insert(ctx, &u); err != nil { // fills u.ID, CreatedAt, UpdatedAt
		log.Fatal(err)
	}

	posts := orm.For[domain.Post](db)
	latest, err := posts.FindAll(ctx, q.Eq("user_id", u.ID), q.Sort("-created_at"), q.Limit(10))
	log.Println(latest, err)
}
```

If you already have a `*sql.DB`, use `orm.Open(sqlDB, dialect.Postgres())`.

### 5. Change the schema later

You have two ways to do it, and they can be mixed:

```bash
# A. Ask for a specific change
kiln add-column User avatar_url:string:null
kiln add-index Post title

# B. Edit the model structs yourself, then generate the migration from the difference
kiln diff add_slugs
kiln migrate up
```

### Project layout (clean architecture)

```
cmd/
  migrate/main.go          # migration + seed runner
  api/main.go              # your app: wires the layers together
database/                  # infrastructure only: schema history and starting data
  migrations/
  seeds/
internal/
  domain/                  # entities (your models), repository interfaces, domain errors
    user.go
    user_repository.go     # ← kiln scaffold User
    errors.go
    kiln_cols.go           # ← kiln gen
  repository/              # kiln implementations of the domain interfaces
    user_repository.go     # ← kiln scaffold User
  usecase/                 # business rules; depends only on domain
    user_usecase.go        # ← kiln scaffold User
kiln.json
```

Dependencies point inwards: `usecase` → `domain` ← `repository`. Usecases never import kiln,
so they can be unit tested with a fake repository.

**Layers are opt-in.** `kiln create` writes only the entity and its migration. Run
`kiln scaffold User` for models with business logic; it writes the three files once, as a
starting point you then edit:

```go
// internal/domain/user_repository.go: trim it to what the usecase needs
type UserRepository interface {
	FindByID(ctx context.Context, id int64) (User, error)
	FindByEmail(ctx context.Context, email string) (User, error) // added by you
	Create(ctx context.Context, rec *User) error
}

// internal/repository/user_repository.go
func (r *userRepository) FindByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := r.repo.Find(ctx, domain.UserCols.Email.Eq(email))
	return u, mapErr(err) // orm.ErrNotFound becomes domain.ErrNotFound
}

// cmd/api/main.go
users := usecase.NewUserUsecase(repository.NewUserRepository(db))
```

Simple models (lookup tables, join tables) don't need layers: use `orm.For[domain.Tag](db)`
directly. `scaffold` never overwrites your edits unless you pass `--force`.

**Where business logic goes:** in `domain` and `usecase`, not in `database/`. Migrations are
frozen history, so logic copied into them never gets updated. A seed that needs rules (hashing
a password, validating input) should call the usecase:

```go
// database/seeds/20261123_admin_user.go
uc := usecase.NewUserUsecase(repository.NewUserRepository(db))
return uc.Create(ctx, &domain.User{Name: "Admin", Email: "admin@example.com"})
```

---

## How it works

```
             ┌──────────────────────────── your module ───────────────────────────┐
             │                                                                    │
 kiln        │  domain/*.go              database/migrations/   database/seeds/   │
 CLI writes  │  (structs + tags)        (schema DSL in Go)          (Go funcs)    │
 and edits   │      │    │                     │                           │      │
 these files │      │    └────── kiln diff ───┤ replays migrations in      │      │
             │      │          (or --db:      │ memory, compares with      │      │
             │      │           inspects DB)  │ the models, writes a new   │      │
             │      ▼                         ▼ migration file             │      │
             │  orm.For[T]               migrate.Migrator  ◄───────────────┘      │
             │  (cached reflection)      (version table + lock)                   │
             │      │                         │                                   │
             │      ▼                         ▼                                   │
             │  q.Query (AST)            schema.Op (AST)                          │
             └──────┼─────────────────────────┼───────────────────────────────────┘
                    ▼                         ▼
          dialect: PostgreSQL │ MySQL │ SQLite │ SQL Server  →  SQL text + args
                    ▼
          database/sql + the driver you chose
```

**Packages**

| Package | Role |
|---|---|
| `q` | Immutable query AST: filters, sorting, joins, grouping, locking, mutations. It contains no SQL strings. |
| `schema` | Migration DSL. Each call records an `Op` (CreateTable, AddColumn, ...). `schema.State` replays Ops in memory. |
| `dialect` | Compiles `q.Query` and `schema.Op` into SQL for one database: quoting, placeholders (`$1`, `?`, `@p1`), `RETURNING`/`OUTPUT`, `LIMIT`/`TOP`/`OFFSET FETCH`, upserts and DDL. It also classifies constraint errors. |
| `orm` | Runs queries. `DB` wraps `*sql.DB`, and `Repo[T]` is the typed API. It handles reflection metadata, scanning, hooks, timestamps, soft delete, preload, cascade, change tracking and replicas. |
| `migrate` | Versioned runner (`schema_migrations` table + lock), diff engine, database inspection and Go code rendering. |
| `seed` | Ordered, named seeders, each run in a transaction. |
| `cache` | Storage for the query result cache: the `Store` interface and the in-memory LRU. Redis lives in the separate module `cache/rediscache`. |
| `kilnotel`, `kilnprom` | OpenTelemetry tracing and Prometheus metrics plugins, each its own module so the core needs neither. |
| `kilnvalidate` | Validation plugin (`validate` tags), its own module. |
| `audit` | Audit log plugin, its own module; `kiln audit init` writes its table migration. |
| `kilncrypt` | Column encryption with blind-index search and key rotation, its own module. |
| `kilnmask` | Masks personal data in API responses and logs (`mask` tags), its own module. |
| `kilnpg` | PostgreSQL arrays (`Array[T]` with `Has`, `Contains`, `Overlaps` filters) and enum types in migrations, its own module. |
| `kilnbreaker` | Circuit breaker: fail fast while the database is down, its own module. |
| `kilnrw` | Read-your-writes with read replicas, its own module. |
| `kilnshard` | Split data across databases by key (tenant, customer), its own module. |
| `ormtest` | In-memory mocks of `orm.Repository[T]`. |
| `kilntest` | Real-database test helpers: a rolled-back transaction per test, factories, `Migrate`. No global state. |
| `tenant` | Multi-tenancy plugin: scopes queries and writes to the tenant in the `ctx`. |

**Key mechanisms**

- **Model metadata.** On first use, each struct type is reflected once and cached. This finds
  the table name, columns, primary key(s), timestamp fields, soft-delete field, JSON fields
  and associations. Everything after that works from the cache.
- **Transactions travel in `context.Context`.** `db.Transaction(ctx, fn)` puts the `*sql.Tx`
  into the ctx it passes to `fn`. Every repo call made with that ctx joins the transaction, so
  your service functions don't need a `tx` parameter. A nested `Transaction` becomes a
  `SAVEPOINT`.
- **Writes.** Insert sets timestamps, runs `BeforeInsert`, compiles the INSERT for the
  dialect, and reads the generated key back (`RETURNING` on PostgreSQL/SQLite, `OUTPUT INSERTED`
  on SQL Server, `LastInsertId` on MySQL). It then runs `AfterInsert`. If an After-hook fails,
  the write is rolled back.
- **Preload** loads associations with one `IN (...)` query per level, never one query per row.
- **Migrations.** The runner takes a database-wide lock, so two app instances starting at once
  don't both migrate (PostgreSQL advisory lock, MySQL `GET_LOCK`, SQL Server
  `sp_getapplock`, and on SQLite a row in `kiln_migrate_lock`, taken over after 10 minutes
  if its process died). It then applies each pending migration and its `schema_migrations` row in
  one transaction. On SQLite, operations that `ALTER TABLE` cannot do are performed by the
  official rebuild procedure (copy into a new table, swap, re-index, check foreign keys).
- **Diffing.** `kiln diff` builds the *current* schema by replaying your migrations into a
  `schema.State` in memory, with no database needed. It builds the *wanted* schema from your
  model structs and `schema` tags, then compares them. With `--db`, the current schema
  is read from the live database instead (`information_schema`, `pg_catalog`, `PRAGMA`, `sys.*`).
  The result is rendered as DSL Go code into a new migration file. The CLI runs this inside
  your module through a temporary `go run` program, because it has to import your models.

---

## CLI reference

Run commands from anywhere inside your module. Paths come from `kiln.json`.

| Command | What it does |
|---|---|
| `kiln init [-d dialect] [--layout clean\|flat] [--migrations dir] [--models dir] [--runner dir] [--seeds dir] [--repositories dir] [--usecases dir] [--force]` | Create the config, the migrations and seeds packages, and the runner. With `--force`, it re-runs and regenerates the runner, keeping config values for flags you omit. |
| `kiln create <Model> field:type... [--soft-delete] [--no-timestamps] [--table name]` | New model + create-table migration |
| `kiln add-column <Model> field:type...` | Add fields to the model + migration |
| `kiln drop-column <Model> column...` | Remove fields + migration. The Down restores the columns. |
| `kiln rename-column <Model> old new` | Rename a field/column + migration |
| `kiln add-index <Model\|table> column... [-u]` | Index (or unique index) migration |
| `kiln drop-index <Model\|table> column... [-u]` | Drop-index migration |
| `kiln scaffold <Model> [--force]` | Repository interface, kiln implementation and usecase for a model, written once ([details](#project-layout-clean-architecture)) |
| `kiln new <name>` | Empty migration for hand-written changes or data migrations |
| `kiln diff [name] [--dry-run] [--db] [--dsn url]` | Migration from model changes ([details](#auto-generated-migrations)) |
| `kiln pull [table...] [--force] [--dsn url]` | Generate models from an existing database |
| `kiln erd [table...] [--out file] [--db] [--dsn url]` | Diagram of the tables and relations (Mermaid) ([details](#schema-diagram)) |
| `kiln lint [--all] [--strict] [--dsn url] [--tenant-column col]` | Check migrations for changes that are risky in production ([details](#migration-safety-checks)) |
| `kiln audit init [--table name]` | Write the migration for the audit log table ([details](#audit-log-audit)) |
| `kiln tenant rls [table...] [--column col]` | Write a migration that adds PostgreSQL row-level security to tenant tables ([details](#multi-tenancy-tenant)) |
| `kiln gen [--check]` | Typed columns for every model (`internal/domain/kiln_cols.go`) ([details](#typed-columns-compile-time-checked)) |
| `kiln migrate [up \| down [n] \| redo \| status \| sql] [--dsn url]` | Run migrations |
| `kiln new-seed <name>` | New seeder file |
| `kiln seed [name...] [--dsn url]` | Run all seeders, or only the named ones |

The database URL comes from `--dsn` when you pass it, otherwise from the `DATABASE_URL`
environment variable, otherwise from `DATABASE_URL` in the project's `.env` file.

### Field syntax

`name:type[:modifier...]`, e.g. `email:string:unique:limit=120`, `score:int:default=5`,
`author:references:to=users`.

| Types | |
|---|---|
| `string` (VARCHAR 255) `text` `int` `bigint` `float` `decimal` `bool` `datetime` `date` `time` `bytes` `json` `uuid` | Column types |
| `references` | `author:references` becomes `author_id BIGINT` + a foreign key to `authors(id)` + an index |

| Modifiers | |
|---|---|
| `null` | Allow NULL. The Go field becomes a pointer. Columns are `NOT NULL` by default. |
| `unique`, `index` | Unique constraint / plain index |
| `required` | Keep NOT NULL even without a default (see `add-column` below) |
| `limit=N`, `precision=P`, `scale=S` | Size of string / decimal |
| `default=V` | Column default |
| `to=table` | Referenced table for `references` |

`add-column` makes migrations safe on tables that already contain rows. `NOT NULL` columns
get a zero-value default (`''`, `0`, `false`). New `references` columns are nullable, because
`0` would break the foreign key.

---

## Models

A model is a plain struct. Conventions cover most cases, and tags handle the rest.

```go
type User struct {
	ID        int64                          // "ID" is the primary key, auto-increment
	Name      string                         // column "name"
	Email     string         `db:"email"`
	Bio       *string                        // pointer = nullable
	Prefs     map[string]any `db:"prefs,json"` // stored as JSON
	CreatedAt time.Time                      // set on insert
	UpdatedAt time.Time                      // set on insert and update
	DeletedAt *time.Time                     // enables soft delete

	Posts   []Post   // has many   (posts.user_id → users.id)
	Profile *Profile // has one    (profiles.user_id → users.id)
}

type Post struct {
	ID     int64
	UserID int64
	User   *User // belongs to  (posts.user_id → users.id)
	Title  string
}
```

| Rule | |
|---|---|
| Table name | snake_case plural of the type (`BlogPost` → `blog_posts`), or a `TableName() string` method |
| Column name | snake_case of the field (`AvatarURL` → `avatar_url`), or `db:"name"` |
| Primary key | the `ID` field, or fields tagged `primary`. Several `primary` fields make a composite key. |
| `db` tag options | `primary`, `auto` / `noauto` (auto-increment), `json`, `readonly`, `omitempty`; `db:"-"` skips the field |
| Associations | detected from struct/slice fields; override the keys with `ref:"owner_col" fk:"target_col"` |
| Embedding | embedded structs are flattened, so a shared `Base{ID, CreatedAt, UpdatedAt}` works |

The `schema:"..."` tag only matters to `kiln diff`/`pull` (see
[Auto-generated migrations](#auto-generated-migrations)). The ORM ignores it at runtime.

---

## Querying

```go
db := orm.Open(sqlDB, dialect.Postgres())
users := orm.For[User](db)

u, err := users.FindByID(ctx, 1)                      // orm.ErrNotFound if missing
u, err  = users.Find(ctx, q.Eq("email", "a@b.c"))     // first match
list, err := users.FindAll(ctx,
	q.Eq("active", true),
	q.Or(q.Gt("age", 18), q.Null("age")),
	q.Like("email", "%@corp.com"),
	q.Sort("-created_at", "id"), q.Limit(20), q.Offset(40))
n, err  := users.Count(ctx, q.In("role", "admin", "owner"))
ok, err := users.Exists(ctx, q.Eq("email", e))

if errors.Is(err, orm.ErrNotFound) { ... }
```

**Totals**, typed by the column (from [`kiln gen`](#typed-columns-compile-time-checked), or
`q.Col[int64]("amount")`):

```go
c := domain.OrderCols
total, err := orm.Sum(ctx, orders, c.Amount, c.Status.Eq("paid")) // int64, like the column
avg, err := orm.Avg(ctx, orders, c.Amount)                        // float64 (integers not rounded)
first, err := orm.Min(ctx, orders, c.CreatedAt)                   // time.Time; orm.ErrNotFound if none
largest, err := orm.Max(ctx, orders, c.Amount)
byStatus, err := orm.CountBy(ctx, orders, c.Status)              // map[Status]int64
revenue, err := orm.SumBy(ctx, orders, c.Country, c.Amount)       // map[string]int64
```

They apply the same filters, scopes, soft delete and plugins as `FindAll` (with the tenant
plugin, a tenant only adds up its own rows). `Sum` of nothing is 0. `CountBy`/`SumBy` leave out
rows where the grouping column is NULL. For anything else, `db.Select` takes any query.

**Filters**: `Eq Ne Lt Lte Gt Gte Like NotLike Null NotNull In NotIn InOf Between`,
combined with `And Or Not`, plus `Expr("lower(email) = ?", e)` for raw SQL fragments.
**Modifiers**: `Sort("-col")` (`-` means descending), `Limit`, `Offset`, `Page(page, size)`,
`Select(cols...)`, `Distinct()`, `Group`, `Having`, `Join`, `LeftJoinOn`, `ForUpdate()`
(row lock), `Unscoped()` (include soft-deleted rows).

`Sort` fields and `Set` targets are always column names (`name`, `users.name`, even
`display name`): kiln quotes them, so a `?sort=` parameter can't inject SQL, and an unknown
name is an error. Check user input against your own list of sortable columns anyway, so users
can't sort by columns you don't want exposed. Filter field names may be expressions
(`q.Gt("COUNT(*)", 5)`), so never build those from user input, and a column whose name has a
space can't be filtered by name (use `q.Expr`). `Update` of a row that no longer exists writes nothing and returns nil. Add a
`db:"version,lock"` field to get `ErrStale` instead.

Write `?` placeholders in `Expr` and raw SQL. They are converted to `$1`/`@p1` per dialect
(`??` gives a literal `?`).

**Inside JSON columns** (fields tagged `db:"prefs,json"`), on all four databases:

```go
users.FindAll(ctx, q.JSON("prefs", "theme").Eq("dark"))
users.FindAll(ctx, q.JSON("prefs", "limits", "max").Gt(10))           // numbers compare as numbers
users.FindAll(ctx, q.JSON("prefs", "beta").Eq(true))                  // JSON true/false
users.FindAll(ctx, q.JSON("doc", "items", "0", "sku").Eq("A-1"))      // digits = array index
users.FindAll(ctx, q.JSON("prefs", "note").IsNull())                  // missing or null
users.FindAll(ctx, q.JSONContains("prefs", map[string]any{"beta": true})) // PostgreSQL, MySQL
```

The Go type of the value picks the comparison: text, number or boolean. Keys are sent as
query parameters, so any key is safe, even one from user input. `JSONContains` (`@>` on
PostgreSQL, `JSON_CONTAINS` on MySQL) returns an error on SQLite and SQL Server instead of
running slowly. On PostgreSQL, index what you filter on, e.g. a GIN index for `JSONContains`.

**Arbitrary queries** go into any struct or scalar:

```go
var stats []struct {
	UserID int64 `db:"user_id"`
	N      int   `db:"n"`
}
err = db.Select(ctx, &stats, q.From("posts").
	Select("user_id", "COUNT(*) AS n").
	Where(q.In("user_id", q.From("users").Select("id").Where(q.Eq("active", true)))). // sub-query
	Group("user_id").
	Having(q.Gt("COUNT(*)", 5)))

var names []string
err = db.Query(ctx, &names, "SELECT name FROM users WHERE age > ?", 30)   // raw SQL
_, err = db.Exec(ctx, "UPDATE counters SET n = n + 1 WHERE id = ?", 1)
```

### Typed columns (compile-time checked)

Column names as strings (`q.Eq("emial", x)`) compile even when misspelled. Run
`kiln gen` once to get typed columns instead. The compiler then checks both the
**column name** and the **value type**:

```bash
kiln gen        # writes internal/domain/kiln_cols.go
```

```go
c := domain.UserCols

adults, err := users.FindAll(ctx, c.Age.Gte(18), c.Status.Eq("active"), c.CreatedAt.Desc())
u, err := users.Find(ctx, c.Email.Eq("ann@x.io"))
n, err := users.UpdateAny(ctx, c.Email.In("a@x.io", "b@x.io"), c.Status.Set("active"), c.Age.Inc(1))
err = db.Select(ctx, &names, users.Query(q.Select(q.Names(c.Name)...), c.Name.Asc()))

users.Find(ctx, c.Emial.Eq("x"))   // compile error: type domain.userCols has no field or method Emial
users.Find(ctx, c.Age.Eq("ten"))   // compile error: cannot use "ten" as int value
```

The generated file looks like this. Your own types (`Status`) and imported types
(`time.Time`, `uuid.UUID`) are kept:

```go
// Code generated by kiln gen. DO NOT EDIT.
var UserCols = userCols{ID: "id", Email: "email", Age: "age", Bio: "bio", Status: "status", ...}

type userCols struct {
	ID     q.Col[int64]
	Email  q.Col[string]
	Age    q.Col[int]
	Bio    q.Col[string] // nullable: a *string field; use Bio.IsNull() / NotNull()
	Status q.Col[Status]
	...
}
```

Every `q.Col[T]` has `Eq Ne Lt Lte Gt Gte Between In NotIn InQuery Like NotLike IsNull NotNull`,
`EqCol` (compare two columns), `Asc`/`Desc`, `Set`/`SetNull`/`Inc` for `UpdateAny`, and `Of("u")`
to qualify it for joins (`u.email`).

Keeping the file current:

- `kiln create`, `add-column`, `drop-column` and `rename-column` refresh it automatically once it
  exists.
- After editing structs by hand, run `kiln gen` again, or add `//go:generate kiln gen` to a
  models file.
- In CI, `kiln gen --check` fails when the file is stale.

String columns keep working everywhere, so you can switch gradually. `orm.Describe(User{})` returns
the same table and column information at runtime, which is useful for admin screens or exports.

---

## Writing data

```go
err = users.Insert(ctx, &u)                // fills u.ID, CreatedAt, UpdatedAt
err = users.InsertAll(ctx, batch)          // batched multi-row INSERTs; IDs filled in place
err = users.Update(ctx, &u)                // all columns
err = users.Update(ctx, &u, "Name")        // only these fields (+ updated_at)
err = users.Save(ctx, &u)                  // insert if new, update otherwise
err = users.Delete(ctx, &u)                // soft delete if the model has DeletedAt
err = users.Restore(ctx, &u)               // undo a soft delete
err = users.HardDelete(ctx, &u)            // real DELETE

// Set-based writes
n, err := users.UpdateAny(ctx, q.Lt("last_seen", cutoff), q.Set("active", false), q.Inc("logins", 1))
n, err  = users.DeleteAny(ctx, q.Eq("spam", true))

// Upserts (PostgreSQL, MySQL, SQLite)
users.Insert(ctx, &u, orm.OnConflictUpdate([]string{"email"}, "name")) // update name on conflict
users.Insert(ctx, &u, orm.OnConflictDoNothing("email"))

// Portable constraint errors
if orm.IsUniqueViolation(err) { ... }
if orm.IsForeignKeyViolation(err) { ... }
```

### Update only what changed

```go
cs := orm.Track(&u)              // take a snapshot
u.Name = "New name"
cs.Changed()                     // []string{"Name"}
users.UpdateChanged(ctx, &u, cs) // UPDATE users SET name = $1, updated_at = $2 WHERE id = $3
```

Without changes, `UpdateChanged` sends no query at all. After a successful update, the
snapshot is reset.

### Optimistic locking

Two people open the same record, both edit it, and the second save silently overwrites the
first. Tag an integer field with `lock` and kiln refuses that second save:

```go
type Doc struct {
	ID      int64
	Body    string
	Version int `db:"version,lock"`
}

err := docs.Update(ctx, &doc) // UPDATE docs SET ..., version = version + 1 WHERE id = ? AND version = ?
if errors.Is(err, orm.ErrStale) {
	// someone saved first: reload and reapply, or tell the user
}
```

- `Insert` starts the version at 1; each successful `Update` and `Delete` (soft or hard) bumps
  it in the database and in your struct, so the next save works without a reload.
- Partial updates (`Update(ctx, &doc, "Body")`) are checked too. The version itself is never
  written from the struct: it only ever increases by one.
- On `ErrStale` nothing was written and the struct is unchanged.
- Bulk calls (`UpdateAny`, `DeleteAny`, `InsertAll` conflict updates) are explicit queries and
  are not version-checked.
- No database locks are held. Models without a `lock` field are not affected.

Adding it to an existing table: `s.AddColumn("docs", "version", schema.Int, schema.Required(), schema.Default(0))`.

### Composite primary keys

```go
type PostTag struct {
	PostID int64  `db:"post_id,primary"`
	Tag    string `db:"tag,primary"`
	Weight int
}
tags := orm.For[PostTag](db)
pt, err := tags.FindByPK(ctx, postID, "go") // keys in field order
```

### Cascade insert

```go
u := User{
	Name:    "Ada",
	Profile: &Profile{Bio: "..."},                        // has one
	Posts:   []Post{{Title: "first"}, {Title: "second"}}, // has many
}
err := users.Insert(ctx, &u, orm.Cascade()) // one transaction; foreign keys set for you
```

Belongs-to parents are inserted first, then the record, then its children. Records that
already have a primary key are linked, not re-inserted.

---

## Associations

Associations are loaded on demand:

```go
users.Preload(ctx, &u, "Posts", "Profile")
users.PreloadAll(ctx, list, "Posts.Comments")                       // nested: one query per level
db.Preload(ctx, &u, "Posts", q.Eq("published", true), q.Sort("-id")) // filtered
```

### Many-to-many

Declare the join table with an `m2m` tag:

```go
type User struct {
	ID    int64
	Roles []Role `m2m:"user_roles"` // join columns user_id, role_id
}

type Article struct {
	ID      int64
	Related []Article `m2m:"related_articles,article_id,related_id"` // explicit columns (required for self-references)
}
```

```go
users.Preload(ctx, &u, "Roles")                         // join table, then roles: two queries
db.Preload(ctx, &list, "Roles", q.Sort("name"))         // filters/sorting apply to the roles

users.Link(ctx, &u, "Roles", &admin, &editor)           // add join rows; existing links are kept
users.Link(ctx, &u, "Roles", roleID)                    // bare keys work too
users.Unlink(ctx, &u, "Roles", &admin)                  // remove one link
users.Unlink(ctx, &u, "Roles")                          // remove all of u's links

users.Insert(ctx, &User{Roles: []Role{{Name: "new"}, existing}}, orm.Cascade()) // inserts new roles, links all
```

The join table is a normal table that you create in a migration:

```go
s.CreateTable("user_roles", func(t *schema.Table) {
	t.References("user_id", "users", schema.Required(), schema.OnDelete("CASCADE"))
	t.References("role_id", "roles", schema.Required(), schema.OnDelete("CASCADE"))
	t.PrimaryKey("user_id", "role_id")
})
```

---

## Transactions

```go
err := db.Transaction(ctx, func(ctx context.Context) error {
	if err := users.Insert(ctx, &u); err != nil {
		return err // any error rolls back (a panic does too)
	}
	// Nested: a SAVEPOINT; only this block rolls back if it fails.
	_ = db.Transaction(ctx, func(ctx context.Context) error { ... })
	return posts.Insert(ctx, &p)
})

db.TransactionOpts(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable}, fn)
u, err := users.Find(ctx, q.Eq("id", 1), q.ForUpdate()) // SELECT ... FOR UPDATE, inside a tx
```

Always use the `ctx` passed into `fn`. That ctx carries the transaction.

When a callback doesn't fit, for example a transaction that a test helper rolls back later,
use `Begin`. You call `Commit` or `Rollback` yourself:

```go
tx, err := db.Begin(ctx, nil)
err = users.Insert(tx.Context(), &u) // tx.Context() carries the transaction
err = tx.Commit()                    // or tx.Rollback()
```

`Begin` inside a transaction returns `orm.ErrNestedBegin`; use `Transaction` there to get a
savepoint.

**Retrying deadlocks and serialization failures.** Under load, databases abort some
transactions on purpose (deadlocks, `SERIALIZABLE` conflicts, MySQL lock wait timeouts, a
busy SQLite file) and expect you to run them again. `WithRetry` does that:

```go
db := orm.Open(sqlDB, dialect.Postgres(), orm.WithRetry(3))

err := db.Transaction(ctx, func(ctx context.Context) error { ... }) // up to 3 more attempts
```

- The **whole** function runs again in a new transaction, never one statement inside it, so
  keep side effects (sending email, calling other services) outside the function.
- Waits 10ms, 20ms, 40ms ... with jitter, at most 1s; stops early when `ctx` is done.
- Only those errors are retried. Your own errors, constraint errors and `ErrStale` come back
  at once. Nested transactions, `db.Begin` and single statements are not retried.
- `dialect.ErrRetryable` is the error kind it checks, if you want to retry elsewhere:
  `db.Dialect().Classify(err) == dialect.ErrRetryable`.

---

## Pagination

### Offset pages (with totals)

```go
page, err := users.Paginate(ctx, 2, 25, q.Sort("name"))
// page.Items, page.Total, page.Page, page.PerPage, page.TotalPages
```

Use this when you need page numbers. Every call runs a `COUNT(*)`, and deep pages get slower.

### Cursor pages (keyset)

```go
opts := orm.CursorOpts{Limit: 20, Sort: "-created_at"}
page, err := users.Cursor(ctx, opts, q.Eq("active", true))
// page.Items, page.HasNext, page.HasPrev, page.Next, page.Prev (opaque strings)

opts.After = page.Next                  // following page
next, err := users.Cursor(ctx, opts, q.Eq("active", true))

opts.After, opts.Before = "", next.Prev // and back again
prev, err := users.Cursor(ctx, opts, q.Eq("active", true))
```

Each page continues from the last row it saw
(`WHERE created_at < ? OR (created_at = ? AND id < ?)`) instead of using `OFFSET`. Page
10,000 is as fast as page 1, and rows aren't skipped or repeated when data changes between
requests. Ties on the sort column are broken by the primary key. Sorting by a nullable column
(a pointer or `sql.Null*` field) works too: rows with NULL come after every value when
ascending and first when descending, the same on every database. Pass cursors to clients as
is. A modified cursor returns `orm.ErrBadCursor`. Index the sort column for best performance.

### Every row, with flat memory

```go
for u, err := range users.Iter(ctx, q.Eq("active", true), q.Sort("created_at")) {
	if err != nil {
		return err
	}
	send(u)
}

err := users.Each(ctx, 1000, func(u User) error { ...; return nil }, q.Eq("active", true))
```

`Iter` (for `range`) and `Each` (a callback, with your batch size) read a table of any size in
keyset pages, so memory stays flat. No connection is held while your code runs, so the loop
body can query and write, even on SQLite. Breaking out of the loop stops reading.

- They follow a `q.Sort` on one field (then the primary key; NULLs last ascending, first
  descending), or the primary key without one. `q.Limit` caps the total.
- More sort fields or `q.Offset` are an error rather than a wrong result; use `Cursor` or
  `Paginate` for those.

---

## Hooks, scopes, logging, replicas

**Hooks**: implement any of these on the model's pointer type:

```go
func (u *User) BeforeInsert(ctx context.Context) error { u.Email = strings.ToLower(u.Email); return nil }
// BeforeInsert AfterInsert BeforeUpdate AfterUpdate BeforeDelete AfterDelete AfterFind
```

Returning an error aborts the operation. A failing `After*` hook rolls the write back.

**Scopes**: a repo with permanent filters, which is useful for multi-tenancy:

```go
orders := orm.For[Order](db).Scope(q.Eq("tenant_id", tenantID))
list, _ := orders.FindAll(ctx)                                         // always WHERE tenant_id = ...
orders.UpdateAny(ctx, q.Eq("status", "new"), q.Set("status", "seen")) // scoped too
```

**Options for `orm.Open` / `orm.Connect`:**

```go
db := orm.Open(primary, dialect.Postgres(),
	orm.WithLogger(slog.Default(), 200*time.Millisecond), // log queries; warn when slower than 200ms
	orm.WithHook(func(ctx context.Context, e orm.Event) { // metrics/tracing: e.SQL, e.Args, e.Duration, e.Err
		queryDuration.Observe(e.Duration.Seconds())
	}),
	orm.WithReplicas(replica1, replica2), // reads round-robin over replicas
	orm.WithQueryTimeout(5*time.Second),  // no statement runs longer than this
)

u, err := users.Find(orm.UsePrimary(ctx), q.Eq("id", id)) // read your own write from the primary

db.WithConn(ctx, func(ctx context.Context) error { // pin one connection (SET, temp tables, ...)
	...
})
```

With replicas, reads go to replicas and writes go to the primary. Anything inside a
transaction uses the primary.

**Query timeout.** One stuck query (a lock wait, a missing index) holds a connection until it
finishes; enough of them exhaust the pool and the whole app stops. `WithQueryTimeout(d)` gives
each statement at most `d`, including reading its rows:

- an earlier deadline on `ctx` wins, so a request with 1s left is never given 5s;
- it is per statement: a transaction may run longer, as long as each statement is quick
  (`InsertAll` applies it per batch);
- a timed-out call returns an error matching `context.DeadlineExceeded`, and the connection
  goes back to the pool;
- raw SQL through `db.Query` / `db.Exec` is covered too. Without the option nothing changes
  and nothing is allocated.

---

## Plugins

A plugin is turned on with one line when you open the database. Your queries don't change:

```go
db := orm.Open(sqlDB, dialect.Postgres(), myplugin.Plugin())
```

`WithHook` and `WithLogger` are built on the same mechanism.

### Multi-tenancy (`tenant`)

Many customers (tenants) share the same tables, and each row names its owner in a tenant
column. The `tenant` plugin scopes every query to the tenant in the request's `ctx`:

```go
import "github.com/mhdaslam790/kiln/tenant"

db := orm.Open(sqlDB, dialect.Postgres(), tenant.Plugin("tenant_id"))

// In the middleware that authenticates the request:
ctx = tenant.With(ctx, claims.OrgID) // string, int, UUID type: whatever the column holds

users.FindAll(ctx)                  // SELECT ... WHERE users.tenant_id = $1
users.Insert(ctx, &u)               // sets u.TenantID
users.FindAll(context.Background()) // error: tenant.ErrNoTenant
users.FindAll(tenant.Bypass(ctx))   // admin tools, background jobs: all tenants
id, ok := tenant.From(ctx)          // read it back
```

- Applies to every model with the column; other tables are untouched and need no tenant.
- Filters selects, counts, pagination, preloads, updates and deletes (including `UpdateAny`
  and `DeleteAny`).
- Fills the column on insert. A record that names a different tenant, an update that moves a
  record, or `q.Set("tenant_id", ...)` returns `tenant.ErrCrossTenant`.
- **Fails safe:** without a tenant in the `ctx`, queries on tenant tables return
  `tenant.ErrNoTenant` instead of running unfiltered. Under `Bypass`, inserts must set the
  column themselves.
- Cached results are kept apart per tenant, because the filter is part of the cache key.
- Works with `kilntest`: `tenant.With(kilntest.Tx(t, db), id)`.
- **Not filtered:** raw SQL (`db.Query`, `db.Exec`, `q.SQL`), joined tables (only the queried
  table) and many-to-many join tables. Scope those yourself.
- Cost: the same as writing the filter by hand, plus about 0.15 µs per query. Add an index
  that starts with the tenant column on each tenant table; with `"tenant_column": "tenant_id"`
  in `kiln.json`, `kiln lint` reports tables that lack one.

**Second line of defence: PostgreSQL row-level security (optional).** The plugin filters
the queries kiln builds. Raw SQL, or a bug in code that skips the plugin, can still reach
other tenants' rows. With row-level security, PostgreSQL itself refuses them. Add it when
you need it, in two steps:

```bash
kiln tenant rls          # writes a migration for every table with tenant_column
kiln migrate up
```

```go
db := orm.Open(sqlDB, dialect.Postgres(), tenant.Plugin("tenant_id", tenant.WithRLS()))
```

The migration is plain SQL in your project, so you can read and change it:

```go
s.Exec(`ALTER TABLE "tasks" ENABLE ROW LEVEL SECURITY`)
s.Exec(`ALTER TABLE "tasks" FORCE ROW LEVEL SECURITY`)
s.Exec(`CREATE POLICY kiln_tenant_isolation ON "tasks" USING (...) WITH CHECK (...)`)
```

- Each transaction tells PostgreSQL its tenant (`set_config('app.tenant_id', ..., true)`,
  local to the transaction, so a pooled connection never carries it into the next request).
- Tenant work must run inside `db.Transaction`, typically one per request in your
  middleware. A query on a tenant table outside one returns `tenant.ErrNoTransaction`. Writes
  that kiln already wraps in its own transaction work, since that transaction gets the
  tenant too.
- No tenant means no rows: forgetting it fails safe.
- Connect the app as an ordinary role. Superusers and roles with `BYPASSRLS` skip the
  policies.
- **Cross-tenant work** (`tenant.Bypass`: admin tools, jobs) uses a second connection as a
  role with `BYPASSRLS`. The policies have no bypass switch, because any SQL the app runs
  could flip one. `Bypass` on the app's connection returns `tenant.ErrBypassRole`.
  ```go
  db := orm.Open(appSQL, dialect.Postgres(), tenant.Plugin("tenant_id", tenant.WithRLS()))
  admin := orm.Open(adminSQL, dialect.Postgres(), tenant.Plugin("tenant_id")) // BYPASSRLS role
  n, err := orm.For[Task](admin).Count(tenant.Bypass(ctx))
  ```
- **What it protects against:** queries that forget the tenant (raw SQL, a missed code path).
  Not against SQL that sets `app.tenant_id` to another tenant's id itself, such as injected
  SQL that knows or guesses that id: keep using parameters for values.
- New tenant tables later: `kiln tenant rls invoices`. Writing the migration by hand:
  `tenant.RLSPolicy(table, column)` returns the same statements.
- Cost: one `set_config` per transaction, only with `WithRLS`. The plugin's own filter keeps
  queries on the tenant index.

**Adding tenants to an existing app.** If your tables already have an organisation column,
use it: `tenant.Plugin("org_id")`. Otherwise, roll it out in three deploys so the app keeps
working throughout:

1. **Migration:** create `tenants`, insert your existing customer, and add the column with
   that customer as the default. Existing rows and rows from the old code get it:
   ```go
   s.AddColumn("tasks", "tenant_id", schema.BigInt, schema.Required(), schema.Default(1))
   ```
   Add the indexes in a separate migration with `schema.Concurrently()` on PostgreSQL.
2. **Code:** add `TenantID` to the models, `tenant.Plugin(...)` at startup and `tenant.With`
   in your middleware. Run your tests: every path that has no tenant yet (login, admin pages,
   jobs) fails with `ErrNoTenant`, so the failures are your to-do list. Fix each with
   `tenant.With` or `tenant.Bypass`.
3. **Migration:** remove the default so new rows must name their tenant:
   `s.ChangeColumn("tasks", "tenant_id", schema.BigInt, schema.Required())`. Make unique
   constraints per tenant where needed, e.g. `(tenant_id, email)` instead of `(email)`.

### Tracing and metrics (`kilnotel`, `kilnprom`)

Separate modules, so the core keeps zero dependencies:

```bash
go get github.com/mhdaslam790/kiln/kilnotel   # OpenTelemetry tracing
go get github.com/mhdaslam790/kiln/kilnprom   # Prometheus metrics
```

```go
db := orm.Open(sqlDB, dialect.Postgres(),
	kilnotel.Plugin(),                            // one span per statement
	kilnprom.Plugin(prometheus.DefaultRegisterer), // query and pool metrics
)
```

**`kilnotel`**: each statement becomes a span under the request's span from `ctx`, named
`select users`, with `db.system.name`, `db.collection.name`, `db.operation.name` and
`db.query.text`. The query text keeps its placeholders; argument values are not recorded
unless you opt in with `kilnotel.WithArgs()`. Cache hits show up with `kiln.cached=true`.
`WithTracerProvider(tp)` picks a provider other than the global one.

**`kilnprom`**:

| Metric | Labels | Use it for |
|---|---|---|
| `kiln_query_duration_seconds` (histogram) | `db`, `table`, `action`, `outcome` (`ok` / `error` / `cached`) | latency per table, query rate, error rate, cache hit ratio |
| `go_sql_*` (the standard `database/sql` collector) | `db_name` | pool exhaustion: connections in use, idle, wait count and time |

Labels never contain SQL or values, so the number of series stays small. With several
databases, give each a name: `kilnprom.Plugin(reg, kilnprom.WithDBName("analytics"))`.
`kilnprom.Plugin` panics on a registration conflict, like `promauto`; `kilnprom.New` returns
the error instead.

Both are observers: they never change queries. When their hook isn't there, nothing runs.

Your own hooks get the same information: `orm.Event` carries `Table` and `Action`
(`orm.ActionRaw` for raw SQL) next to `SQL`, `Duration`, `Err`, `Cached` and `InTx`.

### Validation (`kilnvalidate`)

A separate module: `go get github.com/mhdaslam790/kiln/kilnvalidate`. It checks
[validator](https://github.com/go-playground/validator) tags before every insert and update,
so invalid records never reach the database:

```go
db := orm.Open(sqlDB, dialect.Postgres(), kilnvalidate.Plugin())

type User struct {
	ID    int64
	Email string `validate:"required,email"`
	Age   int    `validate:"gte=0,lte=130"`
}

err := users.Insert(ctx, &User{Email: "nope", Age: 200})
// kilnvalidate: insert users: email must be a valid email address; age must be at most 130

var verr *kilnvalidate.Error
if errors.As(err, &verr) {
	for _, f := range verr.Fields { // every invalid field at once
		fmt.Println(f.Column, f.Tag, f.Message()) // e.g. for a 422 response
	}
}
```

- `errors.Is(err, kilnvalidate.ErrInvalid)` matches any validation failure.
- Inserts (including each `InsertAll` record) and full updates check the whole record.
  `Update(ctx, &u, "Email")` checks only the fields it writes (including fields of embedded
  structs).
- Validation runs after the other plugins, whatever their order in `orm.Open`, so a
  `validate:"required"` tenant id filled in by `tenant.Plugin` passes.
- Bring your own validator, with custom rules or translations:
  `kilnvalidate.Plugin(kilnvalidate.WithValidator(v))`. `FieldError.Err` is the validator's
  own error, for `Translate`.
- `UpdateAny` sets values, not records, so it isn't checked.

### Audit log (`audit`)

A separate module that records who changed what and when, in the same transaction as the
change. Add it when you need it:

```bash
kiln audit init                              # writes the audit_log table migration
kiln migrate up
go get github.com/mhdaslam790/kiln/audit
```

```go
db := orm.Open(sqlDB, dialect.Postgres(), audit.Plugin(
	audit.Actor(func(ctx context.Context) string { return userIDFrom(ctx) }),
	audit.Redact("password_hash"), // recorded as "[redacted]"
))
```

Every `Insert`, `InsertAll` record, `Update` and `Delete` adds one row:

| table_name | record_id | action | actor | changes |
|---|---|---|---|---|
| users | 42 | insert | u_7 | `{"email": "ann@x.io", "age": 30}` |
| users | 42 | update | u_9 | `{"age": [30, 31]}` |
| users | 42 | delete | u_7 | `{"email": "ann@x.io", "age": 31}` |

- **Atomic:** if the change rolls back, so does its entry. If writing the entry fails, the
  change fails too.
- Updates record only the columns that changed, as `[old, new]`; an update that changes
  nothing records nothing. Before an update the plugin reads the current row once.
- `audit.Skip("sessions")` leaves tables out; `audit.Table("change_log")` writes elsewhere.
- Not recorded: `UpdateAny` / `DeleteAny` (they change rows kiln never sees as records), raw
  SQL and `Restore`.
- **Multi-tenant apps:** `audit.Tenant("tenant_id")` records each entry's tenant (the
  record's own, else the context's), so each tenant can see only its own history. With
  `tenant_column` in `kiln.json`, `kiln audit init` adds the column, and `kiln tenant rls`
  then covers the audit table too.
- The migration is yours: use `JSONB` for `changes` on PostgreSQL, or partition by time.

### Encrypted columns (`kilncrypt`)

A separate module (`go get github.com/mhdaslam790/kiln/kilncrypt`, standard library crypto
only) that encrypts columns in your app, so the database, backups, replicas and the query
cache only ever hold ciphertext:

```go
keys, err := kilncrypt.NewKeys(indexKey, kilncrypt.Key{ID: 1, Secret: key1}) // 32-byte keys
email, err := keys.Index("email", "email_index", strings.ToLower)          // optional: search
db := orm.Open(sqlDB, dialect.Postgres(), kilncrypt.Plugin(keys, email))

type Patient struct {
	ID         int64
	Email      kilncrypt.String // text column, AES-256-GCM
	EmailIndex *string          // blind index column (text, 32 chars; index it)
}

p := Patient{Email: kilncrypt.NewString("ann@clinic.io")}
patients.Insert(ctx, &p)                              // stores "kc2:..." ciphertext
found, err := patients.Find(ctx, email.Eq("Ann@Clinic.io")) // exact match, no decryption
plain, err := found.Email.Open(keys)                  // decrypt only where you need it
```

- **Searching encrypted data:** the blind index stores an HMAC of the normalized value (with
  its own key) next to the ciphertext. `email.Eq(v)` / `email.In(vs...)` compare hashes, so
  lookups use a normal database index. Exact matches only: no `LIKE`, ranges or sorting on
  encrypted columns. Equal values have equal hashes, so don't index low-variety columns
  (booleans, countries).
- **Safe by default:** loaded values stay encrypted until `Open`; they print, log and marshal
  to JSON as `"[encrypted]"`. Without the plugin, writing a `NewString` fails with
  `ErrNotSealed` instead of storing plaintext. `UpdateAny(ctx, where, q.Set("email",
  kilncrypt.NewString(v)))` is sealed for you; for raw SQL use `keys.SealColumn(v, table, column)`.
- **Bound to its column:** each value is authenticated together with its table and column, so a
  value copied into another column or table (by someone with write access to the database)
  fails to decrypt with `ErrDecrypt`. It is not bound to its row: swapping values between two
  rows of the same column is not detected. Values scanned with raw SQL don't know their column:
  `Open` returns `ErrUnbound`; use `s.OpenColumn(keys, "patients", "email")`.
- **Renaming** a table or a column with encrypted values needs a re-encryption: read with
  `OpenColumn` under the old name, write under the new one. `kiln gen` warns about it.
- **Key rotation:** make the new key current and keep the old one for reading, then run
  `kilncrypt.Rotate[Patient](ctx, db, keys)` per model. It re-encrypts in pages, is safe to
  re-run, and afterwards the old key can go. Values re-encrypt on any update too.
- A partial `Update(ctx, &p, "Email")` must also write `"EmailIndex"`; kiln returns an error
  rather than leave a stale index.

### PostgreSQL arrays and enums (`kilnpg`)

A separate, dependency-free module (`go get github.com/mhdaslam790/kiln/kilnpg`). It works
with pgx and lib/pq alike.

```go
type Mood string

type Post struct {
	ID   int64
	Tags kilnpg.Array[string] `schema:"sqltype:TEXT[]"` // nil = NULL, empty = '{}'
	Mood Mood                 `schema:"sqltype:mood"`
}

posts.FindAll(ctx, kilnpg.Has("tags", "go"))                // 'go' = ANY(tags)
posts.FindAll(ctx, kilnpg.Contains("tags", "go", "sql"))    // has both (@>)
posts.FindAll(ctx, kilnpg.Overlaps("tags", "go", "rust"))   // has either (&&)
posts.FindAll(ctx, kilnpg.ContainedBy("tags", "go", "sql")) // nothing else (<@)
```

Enum types in migrations:

```go
Up: func(s *schema.Schema) {
	kilnpg.CreateEnum(s, "mood", "happy", "sad")
	s.CreateTable("posts", func(t *schema.Table) {
		t.ID("id")
		t.Text("tags", schema.SQLType("TEXT[]"))
		t.String("mood", schema.Required(), kilnpg.EnumType("mood"))
	})
},
Down: func(s *schema.Schema) {
	s.DropTable("posts")
	kilnpg.DropEnum(s, "mood")
},
```

`kiln diff` and `kiln erd` read these enum steps, so they don't count as raw SQL.
A type name with spaces or capitals needs its quotes in the model tag, as `kiln pull` writes
it: `schema:"sqltype:\"order status\""` (`EnumType` adds them for you in migrations).
`kilnpg.AddEnumValue(s, "mood", "calm")` adds a value. Use the new value in a later
migration, because PostgreSQL only allows it once the transaction that added it has committed.
`Array` holds `string`, `bool`, integer and float elements (or named types of them). It can't
hold NULL elements and returns an error when it reads one. Index array columns you filter on
with GIN: `CREATE INDEX ON posts USING gin (tags)`.

### Masking personal data (`kilnmask`)

A separate, dependency-free module (`go get github.com/mhdaslam790/kiln/kilnmask`) for what
leaves your service: API responses, logs, exports. Tag fields once, mask a copy where needed:

```go
type User struct {
	ID    int64
	Name  string  `mask:"first:1"` // "A****"
	Email string  `mask:"email"`   // "a****@acme.io"
	Phone *string `mask:"last:4"`  // "****4567"
	Token string  `mask:"full"`    // "****"
}

json.NewEncoder(w).Encode(kilnmask.Copy(u))  // u itself is untouched
slog.Info("signup", "user", kilnmask.Log(u)) // masked only if the line is logged
```

- Rules: `full`, `email`, `first:N`, `last:N`, plus your own with
  `kilnmask.WithRule("iban", fn)`. An unknown rule hides the whole value (fails closed).
- The hidden part is always `****`, so the value's length doesn't leak.
- Nested structs, pointers, slices, maps and `any` fields are masked too; nothing is copied
  that doesn't need to be. Cyclic data (a child pointing back to its parent) is copied safely.
- A `mask` tag on a field that isn't a string (`int64`, `[]byte`, a struct) clears it to its
  zero value: no rule can partly show it, so it is hidden completely.
- Works on any struct, not only kiln models. Pair it with `kilncrypt` for data that must also
  be encrypted at rest.

### Circuit breaker (`kilnbreaker`)

A separate module (`go get github.com/mhdaslam790/kiln/kilnbreaker`, no dependencies). When
the database is down or hanging, queries fail at once instead of each waiting for its
timeout, so requests don't pile up, the pool isn't exhausted, and the database isn't flooded
when it comes back:

```go
breaker := kilnbreaker.New(
	kilnbreaker.Failures(5, 10*time.Second), // open after 5 failures within 10s
	kilnbreaker.Cooldown(30*time.Second),    // then fail fast for 30s, then try one query
	kilnbreaker.OnChange(func(from, to kilnbreaker.State) { slog.Warn("db breaker", "state", to) }),
)
db := orm.Open(sqlDB, dialect.Postgres(), orm.Use(breaker), orm.WithQueryTimeout(2*time.Second))

if errors.Is(err, kilnbreaker.ErrOpen) {
	http.Error(w, "try again shortly", http.StatusServiceUnavailable)
}
// health check: breaker.State() == kilnbreaker.Closed
```

- Guards every round trip: model queries, raw SQL, writes and transaction starts. Cache hits
  are still served while it is open.
- Counts only infrastructure failures: refused or lost connections, timeouts, "too many
  connections", server starting up or shutting down. `ErrNotFound`, constraint violations,
  SQL errors and queries the caller cancelled never trip it. Customize with `IsFailure`.
- Pair it with `WithQueryTimeout`: a hanging database then trips it too.
- **Set connect timeouts in your DSN.** The breaker only counts queries that return. The SQL
  Server driver ignores the context while opening a connection and has no login timeout by
  default, so against a hung server it waits forever: add
  `dial timeout=5&connection timeout=5` to SQL Server DSNs. (PostgreSQL and MySQL drivers honour
  the context; `connect_timeout` / `timeout` add a bound of their own.)
- Costs about 2ns per query while closed, with no allocations. One breaker per `orm.DB`
  (replicas included).

### Read your writes (`kilnrw`)

With `WithReplicas`, a user who saves and reloads can see old data because the replica is a
few milliseconds behind. `kilnrw` (its own module, no dependencies) sends *that user's* reads
to the primary for a short window after they write; everyone else keeps using replicas:

```go
rw := kilnrw.New(
	kilnrw.Key(func(ctx context.Context) string { return sessionID(ctx) }), // across requests
	kilnrw.Window(2*time.Second), // longer than your normal replica lag
)
db := orm.Open(primary, dialect.Postgres(), orm.WithReplicas(replica), orm.Use(rw))
handler = kilnrw.Middleware(handler) // within a request
```

- **Within a request:** `Middleware` (or `kilnrw.Track(ctx)` in jobs) makes reads after a write
  in the same request go to the primary.
- **Across requests** (save → redirect → reload): `Key` identifies the user or session; writes
  are remembered in memory, expiring after the window, with no background goroutine. With
  several app instances, pass a shared store (`kilnrw.WithStore`, e.g. backed by Redis).
- Every write counts: inserts, updates, deletes, `UpdateAny`, raw `Exec`, transactions.
  `orm.UsePrimary(ctx)` still forces the primary by hand.

### Sharding (`kilnshard`)

When one database can't hold or serve everything, split customers across several. Each shard
is an ordinary `*orm.DB` (with its own plugins, replicas, breaker); `kilnshard` (its own
module, no dependencies) only picks which one:

```go
shards, err := kilnshard.New(map[string]*orm.DB{"shard-1": db1, "shard-2": db2})

ctx = kilnshard.With(ctx, customerID)               // in your middleware
users, err := kilnshard.For[User](ctx, shards)      // the repo on that customer's shard
u, err := users.FindByID(ctx, id)

err = shards.Each(ctx, 4, func(ctx context.Context, name string, db *orm.DB) error {
	return nightlyCleanup(ctx, db) // every shard, 4 at a time; errors name the shard
})
applied, err := shards.Migrate(ctx, 4, migrations) // in your migration runner
```

- **Placement:** by default rendezvous hashing over the shard names: even spread, and adding
  a shard moves only ~1/N of the keys (to the new shard). Or decide yourself with
  `kilnshard.Lookup(func(ctx, key) (shard, error))`, e.g. a directory table or region rules.
  Shard by tenant with `kilnshard.Key(...)` reading `tenant.From(ctx)`.
- **Not possible across shards:** transactions, joins, single-statement queries over all
  shards (use `Each`). Moving keys between shards is up to you: copy, then change `Lookup`.
- An empty key (`kilnshard.With(ctx, "")`, e.g. an anonymous request) is `ErrNoKey`, like a
  missing one, rather than sending all such data to one shard.
- Picking a shard costs ~60ns, no allocations.

### Writing a plugin

A plugin has a `Name` and implements any of these hooks. They run in the order the plugins
were given, except that a plugin with a `ChecksOnly()` method (`orm.CheckPlugin`, e.g.
validation) runs its `BeforeWrite` after all the others, so it sees the values they fill in.
Plugins that record times use `db.Now()`, which follows `orm.WithClock`. An error from a
`Before` hook stops the operation before any SQL runs, and the caller gets that error
unchanged.

| Hook | Called | Can |
|---|---|---|
| `BeforeQuery(ctx, *orm.QueryInfo) error` | before every select, count, update and delete on a model table | add filters (`info.Where`), inspect or add the columns an update sets (`info.Set`) |
| `BeforeWrite(ctx, *orm.WriteInfo) error` | for each record inserted or updated, after the model's own `Before` hooks | read and set fields (`w.Get`, `w.Set`) |
| `AfterQuery(ctx, orm.Event)` | after every statement and cache hit | observe (logging, tracing, metrics) |
| `AfterWrite(ctx, *orm.WriteInfo) error` | after each record is inserted, updated or deleted, inside the same transaction | record or react to the change (audit); `w.SetValue` / `w.Value` carry data from `BeforeWrite` |
| `AfterRead(ctx, *orm.ReadInfo) error` | after records of a model are loaded (finds, pages, preloads, cached results; not raw SQL) | adjust loaded fields (`r.Field(i, column)` returns a pointer) |
| `AfterBegin(ctx, *sql.Tx) error` | when a top-level transaction starts | per-transaction session settings (row-level security) |
| `Allow(ctx) error` / `Done(ctx, err)` | around every statement and transaction start | decide whether to reach the database at all (circuit breaker) |
| `Wrote(ctx)` / `ReadPrimary(ctx) bool` | after each successful write / before each replica read | route reads to the primary (read-your-writes) |

For example, a plugin that hides rows of archived projects:

```go
type hideArchived struct{}

func (hideArchived) Name() string { return "hide-archived" }

func (hideArchived) BeforeQuery(ctx context.Context, info *orm.QueryInfo) error {
	if info.Action == orm.ActionSelect && info.Model != nil {
		if _, ok := info.Model.Column("archived"); ok {
			info.Where(q.Eq(info.Table+".archived", false))
		}
	}
	return nil
}

db := orm.Open(sqlDB, dialect.Postgres(), orm.Use(hideArchived{}))
```

A write hook sees each record. This one stamps who made the change, on every model with an
`updated_by` column:

```go
type stampUser struct{}

func (stampUser) Name() string { return "stamp-user" }

func (stampUser) BeforeWrite(ctx context.Context, w *orm.WriteInfo) error {
	if _, ok := w.Model.Column("updated_by"); !ok {
		return nil
	}
	if w.Action == orm.ActionUpdate && w.Fields != nil && !slices.Contains(w.Fields, "updated_by") {
		return nil // a partial update that doesn't write the column
	}
	return w.Set("updated_by", userIDFrom(ctx))
}

db := orm.Open(sqlDB, dialect.Postgres(), orm.Use(stampUser{}), kilnvalidate.Plugin())
// kilnvalidate is a CheckPlugin: it validates after stampUser has filled the column.
```

`info.Action` is `ActionSelect`, `ActionCount`, `ActionUpdate` or `ActionDelete` (soft deletes
included). `info.Model` describes the table's model, or is `nil` for a table no model maps to.
Plugin packages usually return `orm.Use(...)` from a function, so users write
`myplugin.Plugin()`.

Plugins don't change raw SQL (`db.Query`, `db.Exec`, `q.SQL`) or many-to-many join tables;
`AfterQuery` still sees them. With no plugins installed there is no extra cost.

---

## Caching

kiln has two caches. **Both are off by default**. You turn each one on when you open
the database.

| | Prepared statement cache | Query result cache |
|---|---|---|
| Turn on | `orm.WithStmtCache(64)` | `orm.WithCache(...)` **and** `orm.Cached(ctx, ttl)` per query |
| What it saves | The database parsing and planning the same SQL again | The whole trip to the database |
| Results | Always fresh | Served from the cache until a write or the TTL invalidates them |
| Helps most | MySQL / MariaDB (one round trip instead of three) | Hot, repeated reads: lookups, lists, counts |

### Query result cache

**1. Turn it on** when connecting. The in-memory store is the default:

```go
db, err := orm.Connect(ctx, "pgx", dsn, dialect.Postgres(),
	orm.WithCache(), // in-memory LRU: 10,000 entries / 64 MiB
)
```

Or choose and tune the store:

```go
import "github.com/mhdaslam790/kiln/cache"

orm.WithCache(
	orm.CacheStore(cache.NewMemory(cache.MaxEntries(50_000), cache.MaxBytes(256<<20))),
	orm.CacheTTL(5*time.Minute), // default expiry (1 minute if not set)
)
```

To share the cache between several app instances, use **Redis**:

```sh
go get github.com/mhdaslam790/kiln/cache/rediscache github.com/redis/go-redis/v9
```

```go
import (
	"github.com/redis/go-redis/v9"
	"github.com/mhdaslam790/kiln/cache/rediscache"
)

rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
db, err := orm.Connect(ctx, "pgx", dsn, dialect.Postgres(),
	orm.WithCache(
		orm.CacheStore(rediscache.New(rdb)), // single node, sentinel or cluster client
		orm.CachePrefix("myapp:"),           // namespace keys (default "kiln:")
		orm.CacheOnError(func(ctx context.Context, err error) { log.Println("cache:", err) }),
	))
```

**2. Mark the reads you want cached** with `orm.Cached`. Reads without it always go to
the database:

```go
ctx := orm.Cached(ctx, 10*time.Minute) // 0 = the default TTL

u, err := users.FindByID(ctx, 42)                     // cached
list, err := users.FindAll(ctx, q.Eq("active", true)) // cached separately (other SQL/args)
n, err := users.Count(ctx)
page, err := users.Paginate(ctx, 1, 20)
err = users.Preload(ctx, &u, "Posts")                 // preloads are cached too

u, err = users.FindByID(orm.NoCache(ctx), 42)         // skip the cache for this call
```

The same flag works for `Find`, `FindAll`, `FindByID`, `Count`, `Exists`, `Paginate`,
`Preload`, `Select` and `Query`.

**How invalidation works.** Each cached entry records which tables it read: the main
table, joined tables and sub-query tables. Every table has a version. Any write through
the ORM gives the table a new version, which makes every entry that read it stale. This
covers `Insert`, `InsertAll`, `Update`, `Save`, `Delete`, `Restore`, `UpdateAny`,
`DeleteAny`, `Link`, `Unlink` and cascades. So after
`users.Update(ctx, &u)`, the next cached read of `users` goes to the database. Cached
`posts` queries stay valid.

- **Transactions:** reads inside `db.Transaction` always go to the database. Writes
  invalidate only **after commit**, so other requests never cache rows that might still
  be rolled back. A rollback invalidates nothing.
- **Writes the ORM can't see:** raw `db.Exec`, other services and manual SQL. Call
  `db.InvalidateTables(ctx, "users")`, or rely on the TTL.
- **Raw SQL reads** are cached only when you name their tables:
  `db.Query(orm.Cached(ctx, 0, "users", "orders"), &rows, "SELECT ...")`. Name extra
  tables in the same way when a `q.Expr` fragment reads another table.
- **Many identical misses at once** share a single database query.
- **Store failures never fail a query.** If Redis is down, reads fall back to the
  database and `CacheOnError` is called.
- **Hooks see cache hits:** `orm.Event.Cached` is true, and `WithLogger` logs `cached=true`.

> **Memory or Redis?** The memory store is the fastest (about 4–7 µs per hit), but each
> app instance has its own copy. A write on instance A does not clear instance B's
> entries; they become correct once their TTL expires. Running more than one instance?
> Use Redis, or keep TTLs short.

> **After migrations or deploys** that change columns, Redis may still hold results in the
> old shape until they expire. Change `CachePrefix` (for example, include your schema
> version), or flush the prefix.

Values that can't be cached safely are simply not cached. That covers query arguments
that are pointers or structs, and driver values other than integers, floats, booleans,
strings, bytes, times and NULL. The query still runs normally.

### Prepared statement cache

```go
db, err := orm.Connect(ctx, "mysql", dsn, dialect.MySQL(), orm.WithStmtCache(64))
```

This keeps up to 64 prepared statements per connection pool and reuses them. Nothing
else changes, and results are never cached.

- It is used on the pool only. Inside transactions and `WithConn` blocks, statements run
  as usual.
- Raw SQL containing `;` is never prepared.
- If a statement fails after a schema change, it is dropped and prepared again next time.
- Each connection keeps its own copy of a statement, so keep the size modest. MySQL limits
  prepared statements server-wide (`max_prepared_stmt_count`).
- PostgreSQL's `pgx` driver already caches statements itself, so expect little gain there.

---

## Migrations

A migration is an `Up` and a `Down` function that record operations with the DSL:

```go
func init() {
	Set.Add(20261123120000, "create_orders",
		func(s *schema.Schema) {
			s.CreateTable("orders", func(t *schema.Table) {
				t.ID("id")
				t.References("user_id", "users", schema.Required(), schema.OnDelete("CASCADE"))
				t.String("status", schema.Required(), schema.Limit(20), schema.Default("new"))
				t.Decimal("total", schema.Precision(10), schema.Scale(2))
				t.JSON("meta")
				t.Timestamps()
				t.SoftDelete()
				t.Unique("user_id", "status")
			})
			s.CreateIndex("orders", "", []string{"created_at"}, schema.PartialWhere("deleted_at IS NULL"))

			// Data migration in Go, in the same transaction
			s.Do(func(ctx context.Context, tx schema.Executor) error {
				_, err := tx.ExecContext(ctx, "UPDATE users SET plan = 'free' WHERE plan IS NULL")
				return err
			})
		},
		func(s *schema.Schema) { s.DropTable("orders") },
	)
}
```

| Operations | |
|---|---|
| Tables | `CreateTable`, `CreateTableIfNotExists`, `AlterTable`, `RenameTable`, `DropTable`, `DropTableIfExists` |
| Columns | `AddColumn`, `ChangeColumn`, `RenameColumn`, `DropColumn` |
| Indexes / keys | `CreateIndex`, `CreateUniqueIndex`, `DropIndex`, `DropIndexIfExists`, `AddForeignKey`, `DropForeignKey` |
| Escape hatches | `Exec(sql, args...)`, `Do(func)` |

`ChangeColumn` describes the whole new column, the same on every database: options you leave
out are removed. `ChangeColumn("t", "c", schema.Int, schema.Required())` drops an existing
default; pass `schema.Default(...)` to keep one.

Column types: `ID String Text SmallInt Int BigInt Float Decimal Bool DateTime Date Time
Bytes JSON UUID`. Column options: `Required Unique Limit Precision Scale Default Primary
Unsigned`, plus `SQLType("CITEXT")` for a database-specific type. Index options:
`UniqueIndex`, `PartialWhere`, `IndexIfNotExists`. Foreign key options: `OnDelete`,
`OnUpdate`, `FKName`.

**Runner behaviour**

- Versions are timestamps, and migrations apply in version order.
- Each migration runs in its own transaction, together with its `schema_migrations` row.
  MySQL commits DDL implicitly, so a failed MySQL migration can be half-applied.
- A database lock stops two processes from migrating at once.
- `migrate status` marks migrations recorded in the database but missing from code.
  `down` refuses to roll those back.
- Each applied migration's row records its source file and a checksum (SHA-256 of the SQL
  its Up produces). If an applied migration is edited later, `migrate status` marks it
  `CHANGED SINCE APPLIED` and `up` logs a warning. Edits inside `s.Do` functions aren't detected.
  Older `schema_migrations` tables get the two columns on the next `up` or `down`.
- From code: `migrate.New(db, migrations.Set.List()).Up(ctx)`, e.g. on app start-up.
  Options: `WithTable`, `WithLog`, `WithoutLock`.

---

## Auto-generated migrations

You can edit model structs directly and let kiln write the migration:

```bash
kiln diff                  # writes database/migrations/<ts>_auto.go
kiln diff add_slugs        # ...named add_slugs
kiln diff --dry-run        # print only
kiln diff --db             # compare with the live database instead of the migrations
kiln pull                  # the reverse: models from an existing database
```

On PostgreSQL, `kiln pull` turns array columns into `kilnpg.Array[T]` fields and each enum
type into a Go string type with one constant per value (`OrderStatusPaid OrderStatus =
"paid"`), in `enum_<name>.go`. Arrays of types with no Go equivalent, such as `box[]`, stay
plain strings.

### Schema diagram

```bash
kiln erd                       # print a Mermaid diagram of the schema the migrations build
kiln erd --out docs/schema.md  # write it into Markdown; GitHub and GitLab draw it
kiln erd users posts           # only these tables
kiln erd --db                  # from the live database instead
```

```mermaid
erDiagram
    users {
        bigint id PK
        string email UK
        text bio "null"
    }
    posts {
        bigint id PK
        bigint user_id FK
        bigint editor_id FK "null"
        string_200 title
    }
    users ||--o{ posts : "user_id"
    users |o--o{ posts : "editor_id"
```

Each table lists its columns with `PK`/`FK`/`UK`, `"null"` for nullable ones and the values of
PostgreSQL enum columns (`"new | paid | shipped"`). Each foreign
key is a relationship labelled with its column: one-to-many, one-to-one when the key is
unique, and optional when it is nullable. A many-to-many join table shows as a table with two
keys. No database is needed: like `kiln diff`, it replays your migrations. Steps written as raw
SQL or Go code can't be read that way, and `kiln erd` says so; use `--db` for those.

### What the models control

For each table that has a model, the model decides:

- which **columns** exist (new fields are added, removed fields are dropped)
- each column's **type**, from the Go type, which a `schema` tag can override
- **nullability**: pointer, `sql.Null*`, slices and maps are nullable; anything else is `NOT NULL`

Everything else is described with a `schema` tag. It is compared only when you declare it, so
details you wrote by hand in migrations are never "corrected" away.

```go
type Post struct {
	ID     int64   `db:"id,primary"`
	Slug   string  `db:"slug" schema:"limit:80,unique"`
	Body   string  `db:"body" schema:"type:text"`
	Price  float64 `db:"price" schema:"type:decimal,precision:10,scale:2"`
	UserID int64   `db:"user_id" schema:"references:users,ondelete:CASCADE"`
	Status string  `db:"status" schema:"index,default:draft"`
	Code   string  `db:"code" schema:"uniqueindex:uniq_posts_code_tenant"`
	Tenant int64   `db:"tenant" schema:"uniqueindex:uniq_posts_code_tenant"` // composite
	Title  *string `db:"title"`                                              // nullable
	Cache  string  `db:"cache" schema:"-"`                                   // not managed by diff
}
```

| Tag key | Meaning |
|---|---|
| `type:<t>` | `text`, `decimal`, `date`, `time`, `uuid`, `json`, `smallint`, … |
| `sqltype:<SQL>` | Exact database type |
| `null` / `notnull` | Override nullability from the Go type |
| `limit:N`, `precision:P`, `scale:S` | Sizes |
| `default:V` | Column default |
| `unique` | Unique index on this column |
| `index` / `index:name` | Plain index; columns sharing a name form a composite index |
| `uniqueindex` / `uniqueindex:name` | Same, unique |
| `references:table[.col]` | Foreign key (also implied by a `belongs_to` association) |
| `ondelete:ACTION` | `CASCADE`, `SET NULL`, `RESTRICT`, … |
| `-` | Ignore this field for schema purposes |

### Safety rules

- Tables without a model are **never dropped**, and indexes the models don't mention are
  left alone.
- A new `NOT NULL` column gets a zero default, so existing rows are fine. The diff prints a
  note when it does this.
- If a column disappears while a similar one appears, the diff prints a **rename hint**.
  Replace the drop/add pair with `s.RenameColumn(...)` to keep the data.
- Every generated `Down` restores the previous schema, including dropped columns.
- The CLI generators write matching tags, so `kiln diff` right after `create`/`add-column`
  reports "schema is up to date". Running `diff` after `pull` also reports no changes.
- `pull` reads columns, types, nullability, lengths, keys, indexes and foreign keys, but not
  column defaults (their SQL differs per database). Add `schema:"default:..."` by hand where
  you want the model to own one.

**Always read a generated migration before applying it.** It is an ordinary Go file, so you
can edit it. `kiln diff` also runs the [safety checks](#migration-safety-checks) on the
migration it writes and prints anything risky.

---

## Migration safety checks

A migration that works on your laptop can still break production: the table has millions of
rows, the old version of the app is still serving traffic during the deploy, or a lock blocks
every write for minutes. `kiln lint` reads your migrations and points these out **before**
they run, with a safer way to do each one.

```bash
kiln lint                      # migrations not yet applied (DATABASE_URL set, e.g. in .env),
                               # otherwise the newest migration
kiln lint --all --strict       # every migration; fail on warnings too (good for CI)
```

```text
20260301120000_drop_legacy
  WARN  drop-column  users.legacy_score
        drops the column and its data; the app version still running during the deploy breaks if it uses it
        fix: first deploy code that no longer reads or writes it, then drop it in a later release
        (reviewed? add s.AllowUnsafe("why", "drop-column") to the migration)

checked 1 pending migration(s): 0 error(s), 1 warning(s), 0 info
```

Each migration is checked against the schema built by the migrations before it, so changes to
a table created in the **same** migration are never reported (it is still empty).

| Check | Level | What it catches |
|---|---|---|
| `not-null-without-default` | error | Adding a `NOT NULL` column with no default to an existing table: fails as soon as it has rows |
| `concurrent-index-mixed` | error | A `Concurrently()` index together with other changes (PostgreSQL): those changes are no longer atomic |
| `allow-unsafe-no-reason` | error | `s.AllowUnsafe` without a reason |
| `drop-table`, `drop-column` | warning | Data loss, and the running app version breaks if it still uses them |
| `rename-table`, `rename-column` | warning | The running app version breaks. Safer: add, backfill, switch, drop |
| `change-column-type` | warning | Rewrites the table under a lock; may lose data if the type narrows |
| `set-not-null` | warning | Fails if any row is still `NULL`. The fix shows the backfill |
| `unique-on-existing` | warning | A unique index fails over duplicates. The fix shows the query that finds them |
| `index-locks-writes` | warning | PostgreSQL: `CREATE INDEX` blocks writes while it builds. Use `schema.Concurrently()` |
| `foreign-key-on-existing` | info | Every existing row is checked while locks are held |
| `raw-sql` | info | `s.Exec` / `s.Do` steps cannot be checked |
| `irreversible` | info | No `Down` function |
| `tenant-column-without-index` | warning | With `tenant_column` in `kiln.json` (or `--tenant-column`): a table with the tenant column but no index starting with it, so every tenant query scans all tenants' rows. Checks the schema after all migrations |

Errors make `kiln lint` exit non-zero; with `--strict`, warnings do too.

**Reviewed it and it's fine?** Say so in the migration. The reason is required, so the
decision is recorded in the code for whoever reads it next:

```go
set.Add(20260301120000, "drop_legacy", func(s *schema.Schema) {
	s.AllowUnsafe("legacy_score unused since v2.3", "drop-column")
	s.DropColumn("users", "legacy_score")
}, func(s *schema.Schema) {
	s.AddColumn("users", "legacy_score", schema.Int)
})
```

Without check names, `AllowUnsafe` silences every check in that migration.

**Indexes without downtime (PostgreSQL).** `schema.Concurrently()` builds the index without
blocking writes. kiln runs that migration outside a transaction automatically, because
PostgreSQL refuses `CREATE INDEX CONCURRENTLY` inside one. Keep the index alone in its
migration; other databases ignore the option.

```go
s.CreateIndex("orders", "", []string{"customer_id"}, schema.Concurrently())
```

In Go code, the same checks are available as `migrate.Lint(kind, migrations, filter)` and
`migrator.LintPending(ctx)`.

**In CI:**

```yaml
- run: go run github.com/mhdaslam790/kiln@latest lint --all --strict
```

---

## Seeds

```bash
kiln new-seed admin_user     # database/seeds/<ts>_admin_user.go
kiln seed                    # run all, in file order
kiln seed admin_user         # run only this one
```

```go
func init() {
	Set.Add("admin_user", func(ctx context.Context, db *orm.DB) error {
		return orm.For[domain.User](db).Insert(ctx,
			&domain.User{Name: "Admin", Email: "admin@example.com"},
			orm.OnConflictDoNothing("email")) // seeders should be safe to re-run
	})
}
```

Each seeder runs in its own transaction. From code: `seeds.Set.Run(ctx, db, names...)`.

---

## Testing your code

### Unit tests with mocks

Have your services depend on the interfaces instead of concrete types:

```go
type Signup struct {
	Users orm.Repository[User] // *orm.Repo[User] satisfies it
	Tx    orm.Transactor       // *orm.DB satisfies it
}
```

Then use `ormtest` in tests, with no database:

```go
func TestSignup(t *testing.T) {
	users := ormtest.New[User](t)
	users.ExpectExists(q.Eq("email", "a@b.c")).Return(false)
	users.ExpectInsert().
		Match(func(u *User) bool { return u.Email == "a@b.c" }).
		Run(func(u *User) { u.ID = 42 }) // simulate the database

	u, err := Signup{Users: users, Tx: ormtest.Tx{}}.Register(ctx, "a@b.c")
	// ...
} // unmet expectations fail the test automatically
```

There is an `Expect…` method for every repository method. Expectations can be refined with
`Return(v)`, `Error(err)`, `NotFound()`, `Run(fn)`, `RunAll(fn)`, `Match(fn)`, `Times(n)`,
`Maybe()` and `Always()`. Pass `ormtest.Any` to accept any query. When a call matches no
expectation, the failure message shows the SQL the call would have run.

### Integration tests with `kilntest`

`kilntest` runs each test in a transaction that is rolled back when the test ends. Tests
never see each other's rows and need no cleanup. Factories create records with defaults:

```go
import "github.com/mhdaslam790/kiln/kilntest"

var (
	db    *orm.DB
	users *kilntest.Factory[domain.User]
)

func TestMain(m *testing.M) {
	db = openTestDB() // your *orm.DB for tests
	users = kilntest.NewFactory(db, func(n int) domain.User {
		return domain.User{Name: "User", Email: fmt.Sprintf("u%d@test.dev", n)}
	})
	os.Exit(m.Run())
}

func TestPromote(t *testing.T) {
	kilntest.Migrate(t, db, migrations.Set.List()) // applies pending ones; cheap when none
	ctx := kilntest.Tx(t, db)                      // rolled back when the test ends

	u := users.Create(t, ctx)                                               // defaults, ID filled in
	admin := users.Create(t, ctx, func(u *domain.User) { u.Role = "admin" }) // override fields
	list := users.CreateMany(t, ctx, 10)                                      // one batched insert
	draft := users.Build()                                                    // not inserted

	err := svc.Promote(ctx, u.ID) // pass ctx to the code under test
	...
}
```

- `n` counts up from 1 in each factory, so unique columns stay unique. Share one factory
  across tests (as above) so parallel tests never insert the same values.
- Insert hooks run as usual. A failed `Create` fails the test.
- Code under test that calls `db.Transaction` keeps working: inside a test it becomes a
  savepoint, so an error still rolls back only that block.

Things to know:

- **Use the test's `ctx` everywhere.** A call made with a new `context.Background()` runs
  outside the transaction. It can't see the test's rows and its writes are not rolled back.
  With SQLite and `SetMaxOpenConns(1)` such a call waits forever for the only connection.
- **Parallel tests** work on PostgreSQL, MySQL and SQL Server. SQLite allows one writer at a
  time, so keep SQLite tests sequential.
- **MySQL:** schema changes (`CREATE`, `ALTER`, ...) commit the transaction implicitly, so run
  migrations with `kilntest.Migrate`, not inside a test transaction.

A SQLite database in a temp dir is fast and needs no setup:

```go
sqldb, _ := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db")+"?_pragma=foreign_keys(1)")
db := orm.Open(sqldb, dialect.SQLite())
```

---

## Database notes

| | PostgreSQL | MySQL / MariaDB | SQLite | SQL Server |
|---|---|---|---|---|
| Generated keys | `RETURNING` | `LastInsertId` | `RETURNING` | `OUTPUT INSERTED` |
| Upsert (`OnConflict…`) | ✓ | ✓ | ✓ | ✗ (use `MERGE` via raw SQL) |
| Transactional DDL | ✓ | ✗ (implicit commit) | ✓ | ✓ |
| Migration lock | advisory lock | `GET_LOCK` | not needed (single writer) | `sp_getapplock` |
| Unsupported `ALTER TABLE` | — | — | table rebuild (automatic) | — |
| JSON column | `JSONB` | `JSON` | `TEXT` | `NVARCHAR(MAX)` |

Tested live against PostgreSQL 18, MariaDB 13 and SQLite. SQL Server is covered by
SQL-generation tests, and by the integration suite when `MSSQL_DSN` is set.

---

## Troubleshooting

| Problem | Fix |
|---|---|
| Scan error into `time.Time` on MySQL | Add `parseTime=true` to the DSN |
| Foreign keys not enforced on SQLite | Add `_pragma=foreign_keys(1)` to the DSN |
| `DATABASE_URL is not set` | Add `DATABASE_URL=...` to `.env` in the project root, export it, or pass `--dsn` |
| `go.mod not found` | Run kiln inside your module, after `go mod init` |
| `go.mod is missing packages kiln needs` | Run the `go get` command it prints (the library and your driver) |
| `applied but missing from code` | A migration file was deleted after being applied. Restore it, or delete its row from `schema_migrations`. |
| `edited after it was applied` / `CHANGED SINCE APPLIED` | An applied migration's Up changed. The database still has the old version: put the change in a new migration (`kiln diff`) and revert the edit. If it's the latest migration and you want the new version, use `kiln migrate redo`. |
| A migration failed half-way on MySQL | MySQL can't roll back DDL. Fix the schema by hand, then re-run. |
| `diff` keeps suggesting a change | Declare the attribute in the `schema` tag (e.g. `limit:120`), or use `schema:"-"` |

---

## Compared with other Go libraries

An honest overview. Every tool here is good, and they make different trade-offs.

| | GORM | ent | bun | rel | **kiln** |
|---|---|---|---|---|---|
| Models | plain structs | generated from a schema DSL | plain structs | plain structs | plain structs |
| Compile-time checked columns | with the separate `gorm/gen` | ✓ generated predicates | ✗ strings | ✗ strings | ✓ `kiln gen` |
| Migration files generated from model changes | ✗ (AutoMigrate, no files) | ✓ through Atlas (needs a dev database) | ✗ hand-written | ✗ hand-written | ✓ without a database, or against the live one |
| Generators that edit your structs (`add-column`, ...) | ✗ | ✗ | ✗ | ✗ | ✓ |
| Built-in query cache with automatic invalidation | ✗ (plugins) | ✗ (separate package) | ✗ | ✗ | ✓ memory or Redis |
| Migration safety checks (lint) | ✗ | ✓ through Atlas | ✗ | ✗ | ✓ `kiln lint` |
| Typed mocks for unit tests | ✗ (sqlmock) | ✗ (real SQLite) | ✗ | `reltest` (untyped) | ✓ `ormtest` |
| Rolled-back transaction per test + factories | ✗ | ✗ (`enttest` opens and migrates) | fixtures (`dbfixture`) | ✗ | ✓ `kilntest` |
| Batched multi-row inserts | ✓ `CreateInBatches` | ✓ `CreateBulk` | ✓ | ✓ `InsertAll` | ✓ `InsertAll`, ids correct on all four databases |
| Extension points | callbacks, plugins | hooks, interceptors | hooks | ✗ | one-line plugins, free when unused |
| Allocation limits enforced in CI | ✗ | ✗ | ✗ | ✗ | ✓ |
| Multi-tenancy that fails safe | ✗ (scopes by hand) | by hand (privacy rules, interceptors) | ✗ | ✗ | ✓ `tenant.Plugin` |
| SQL Server | ✓ | ✗ | ✓ | ✓ | ✓ (newest, least battle-tested) |
| Maturity and ecosystem | very large | large | medium | small | **new** |

**Choose something else when:**

- you want the biggest ecosystem, plugins and Stack Overflow answers: **GORM**;
- your schema is a large graph and you are happy with generated model code: **ent**;
- you prefer writing SQL and want minimal abstraction: **bun**, or **sqlc** for SQL-first code generation;
- you need the most mileage in production today: anything above. kiln is new.

**Choose kiln when** you want one tool for the whole loop: plain structs, compile-checked
queries, migrations written for you from model changes, generators, seeds, caching, mocks and
real-database tests, with a dependency-free core.

**Why Go developers tend to like it:**

- **No magic you can't see.** Everything runs through `database/sql`; every statement can be
  logged; anything automatic (soft delete, plugins) is opt-in and shows up in that SQL.
- **Types instead of strings and `interface{}`.** `Repo[T]` returns `T`; columns can be checked
  at compile time; mocks are typed too.
- **Your structs stay yours.** No embedded base model, no generated model code you can't edit.
- **Performance is measured, not promised.** The overhead per query is published below, and
  CI fails when a change makes common operations allocate more.
- **Small surface, idiomatic errors.** `ctx` first, sentinel errors, savepoints for nested
  transactions, raw SQL whenever you need it.
- **Migrations are the strongest part.** Generated from your structs, linted for risky
  changes, locked across processes, and the same on all four databases.

### In detail: go-rel/rel

kiln's repository API was inspired by rel:

| | rel | kiln |
|---|---|---|
| Typed generic repository (`Repo[T]`) | ✗ (`interface{}`) | ✓ |
| Lifecycle hooks | ✗ | ✓ (After-hook failures roll back) |
| Repository scopes (multi-tenancy) | ✗ | ✓ |
| Offset pagination with totals, batch iteration | ✗ | ✓ `Paginate`, `Each` |
| Cursor (keyset) pagination, both directions | ✗ | ✓ |
| Upsert | partial | ✓ PostgreSQL / MySQL / SQLite |
| Savepoints for nested transactions | ✗ | ✓ |
| Composite primary keys | ✗ | ✓ |
| Many-to-many (preload, link/unlink, cascade) | ✗ | ✓ |
| Change tracking, cascade insert | ✓ | ✓ |
| Mocks for unit tests | `reltest` (untyped) | ✓ `ormtest` (generic) |
| Compile-time checked columns | ✗ | ✓ `kiln gen` |
| Read replicas | ✓ | ✓ (+ `UsePrimary`, `WithConn`) |
| Migration lock across processes | ✗ | ✓ |
| Dry-run SQL, status, redo | partial | ✓ |
| Model + migration generator that edits models | ✗ | ✓ |
| Auto-generated migrations from model changes | ✗ | ✓ (from history or live DB) |
| Models from an existing database | ✗ | ✓ `kiln pull` |
| SQLite table rebuilds for unsupported ALTERs | ✗ | ✓ |
| Seeders | ✗ | ✓ |
| Portable constraint errors | ✓ | ✓ |
| Driver-free core | adapter per DB | ✓ one dialect package |
| Query result cache with automatic invalidation | ✗ | ✓ memory or Redis, opt-in per query |
| Prepared statement cache | ✗ | ✓ opt-in |

---

## Performance

kiln's own cost, measured against an instant in-memory driver so the database's time doesn't
hide it (`go test -run '^$' -bench . ./orm`), next to hand-written `database/sql` code:

| Operation (Apple M-series) | kiln | Hand-written | kiln's extra cost |
|---|---|---|---|
| `FindByID` | 0.9 µs, 21 allocs | 0.33 µs, 9 allocs | ~0.6 µs |
| `FindAll`, 100 rows | 18 µs, 323 allocs | 15 µs, 315 allocs | ~30 ns per row |
| `Insert` | 1.2 µs, 28 allocs | 0.39 µs, 13 allocs | ~0.8 µs |
| `Update` | 0.85 µs, 15 allocs | 0.19 µs, 5 allocs | ~0.7 µs |

A round trip to a real database takes 200–2,000 µs, so kiln adds well under 1% to a typical
query. On SQLite (`cd integration && go test -run '^$' -bench . -benchmem`), loading 1,000
rows takes the same time as hand-written code and allocates less (15.8k vs 16.8k).

How it stays fast, with nothing to configure:

- the column plan for each model is built once and cached, and row readers are pooled;
- plain fields (`string`, `int`, `int64`, `float64`, `bool`, `time.Time`) are written straight
  into the struct without temporary values; `NULL` still becomes the zero value;
- SQL is built in a pre-sized buffer, and quoted identifiers are cached;
- `InsertAll` sends rows in batches inside one transaction, sized for each database, and still
  gives every record the id the database stored for it.

**Batched inserts.** `InsertAll` needs no options; it picks the fastest form per database.
Inserting 1,000 rows (local servers, Apple M-series):

| Database | How `InsertAll` sends rows | One by one (in a transaction) | `InsertAll` | Faster |
|---|---|---|---|---|
| PostgreSQL | multi-row `INSERT ... RETURNING` | 35–44 ms | 6–8 ms | 5–6× |
| MySQL / MariaDB | multi-row `INSERT`, ids from `LAST_INSERT_ID()` | 53–82 ms | 5–6 ms | 10–13× |
| SQLite | small multi-row `INSERT ... RETURNING` (~256 values each) | 17 ms | 5.2 ms | 3.3× |
| SQL Server | `MERGE ... OUTPUT`, up to 1,000 rows each | — | — | fewer round trips |

Inserts with `Cascade()` or an `OnConflict…` option (on auto-increment ids), and records whose ids
you set yourself on SQL Server, go row by row so every id stays correct.
Before/after insert hooks still run for every record.

**Plugins and unused features cost nothing.** With no plugins installed the numbers above are
unchanged. With a plugin that implements every hook, `FindByID` takes about 0.1 µs and one
allocation more, and `InsertAll` of 100 rows one allocation more (not one per row).
`orm/alloc_guard_test.go` fixes the allocation count of common operations, and CI fails when a
change raises it.

**Why reflection rather than generated scanning code?** With the optimisations above, the
remaining gap is a few hundred nanoseconds per query, less than any network round trip.
Generated code would make models harder to work with for a gain you would rarely measure.
If you need those last microseconds, the result cache removes the database trip entirely.

**Caching**: `FindByID` on local servers, in µs per call. Run it with
`PG_DSN=... MYSQL_DSN=... REDIS_ADDR=... go test -run '^$' -bench Caching -benchmem`:

| | No cache | `WithStmtCache` | `WithCache()` memory | `WithCache` Redis |
|---|---|---|---|---|
| SQLite | 10 | 10 | 4–6 | 29 |
| PostgreSQL | 31–37 | 31–36 | 4–6 | 30 |
| MariaDB | 58–88 | 31–42 | 4–7 | 34 |

A Redis hit costs a single round trip: the table versions and the entry are fetched together.
Redis gains the most on expensive queries (joins, aggregates, large lists) or when the
database is remote.

## Developing kiln

```
cmd/        CLI (cobra)             internal/gen   generators, diff/pull runners
q/          query AST               internal/meta  struct reflection + naming
schema/     migration DSL + State   dialect/       SQL compilers per database
orm/        runtime                 migrate/       runner, diff, inspect, render
seed/       seeders                 ormtest/       mocks
kilntest/   test tx + factories     tenant/        multi-tenancy plugin
cache/      result cache stores     cache/rediscache  Redis store (separate Go module)
integration/  end-to-end tests (separate Go module)
```

```bash
make test        # vet + unit tests (race detector) + SQLite end-to-end (no services needed)
make lint        # gofmt + staticcheck, as in CI

# Against real servers, using local installs or `make db-up` (Docker):
make test-all \
  PG_DSN="postgres://$USER@localhost:5432/kiln_test?sslmode=disable" \
  MYSQL_DSN="root@tcp(localhost:3306)/kiln_test?parseTime=true" \
  MSSQL_DSN= REDIS_ADDR=localhost:6379

# The whole suite again with the prepared statement cache on:
cd integration && KILN_TEST_STMT_CACHE=1 go test -count=1 -p 1 ./...
```

Use an empty database for the integration tests, because they create and drop their own
tables. A database whose `*_DSN` is empty is skipped, and so are the Redis
cache tests when `REDIS_ADDR` is empty.

---

## License

[MIT](LICENSE)
