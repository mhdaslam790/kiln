package cmd

import (
	"fmt"
	"os"

	"github.com/mhdaslam790/kiln/internal/gen"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "kiln",
	Short: "Generate models and migrations for the kiln ORM",
	Long: `kiln generates models and versioned migrations for the kiln ORM
(PostgreSQL, MySQL, SQLite, SQL Server) and runs them.

Field syntax:  name:type[:modifier...]
  types:      string text int bigint float decimal bool datetime date time
              bytes json uuid references
  modifiers:  null unique index required limit=N default=V precision=P scale=S
  example:    kiln create Post title:string:limit=200 body:text user:references`,
	SilenceUsage:  true,
	SilenceErrors: true, // Execute prints the error once
}

// Execute runs the CLI.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func project() (*gen.Project, error) { return gen.Load(".") }

func run(fn func(p *gen.Project, args []string) error) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		p, err := project()
		if err != nil {
			return err
		}
		return fn(p, args)
	}
}

func init() {
	// init
	var cfg gen.Config
	var force bool
	var layout string
	var initCmd *cobra.Command
	initCmd = &cobra.Command{
		Use:   "init",
		Short: "Set up kiln.json, the migrations/seeds packages and the migrate runner",
		Long: `Set up kiln.json, the migrations and seeds packages and the migrate runner.
Re-run with --force to regenerate the runner; flags you omit keep their
current kiln.json values.`,
		Args: cobra.NoArgs,
		RunE: run(func(p *gen.Project, _ []string) error {
			// Unset flags keep the existing configuration.
			fl := initCmd.Flags()
			keep := func(name string, dst *string, cur string) {
				if !fl.Changed(name) && cur != "" {
					*dst = cur
				}
			}
			keep("dialect", &cfg.Dialect, p.Config.Dialect)
			keep("migrations", &cfg.Migrations, p.Config.Migrations)
			keep("models", &cfg.Models, p.Config.Models)
			keep("runner", &cfg.Runner, p.Config.Runner)
			keep("seeds", &cfg.Seeds, p.Config.Seeds)
			if layout == "flat" && !fl.Changed("models") {
				cfg.Models = gen.FlatConfig().Models
			}
			if layout != "flat" {
				keep("repositories", &cfg.Repositories, p.Config.Repositories)
				keep("usecases", &cfg.Usecases, p.Config.Usecases)
			} else {
				cfg.Repositories, cfg.Usecases = "", ""
			}
			return p.Init(cfg, force)
		}),
	}
	def := gen.DefaultConfig()
	initCmd.Flags().StringVarP(&cfg.Dialect, "dialect", "d", def.Dialect, "postgres | mysql | sqlite | mssql")
	initCmd.Flags().StringVar(&cfg.Migrations, "migrations", def.Migrations, "migrations package directory")
	initCmd.Flags().StringVar(&cfg.Models, "models", def.Models, "models package directory")
	initCmd.Flags().StringVar(&cfg.Runner, "runner", def.Runner, "directory of the generated migrate command")
	initCmd.Flags().StringVar(&cfg.Seeds, "seeds", def.Seeds, "seeds package directory")
	initCmd.Flags().StringVar(&cfg.Repositories, "repositories", def.Repositories, "where kiln scaffold writes repository implementations")
	initCmd.Flags().StringVar(&cfg.Usecases, "usecases", def.Usecases, "where kiln scaffold writes usecases")
	initCmd.Flags().StringVar(&layout, "layout", "clean", "clean (internal/domain, database/) | flat (models/, database/)")
	initCmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")

	// new
	newCmd := &cobra.Command{
		Use:     "new <name>",
		Short:   "Create an empty migration",
		Example: "  kiln new backfill_user_slugs",
		Args:    cobra.ExactArgs(1),
		RunE:    run(func(p *gen.Project, a []string) error { return p.New(a[0]) }),
	}

	// create
	var co gen.CreateOptions
	createCmd := &cobra.Command{
		Use:     "create <Model> [field:type...]",
		Aliases: []string{"c", "model"},
		Short:   "Create a model and its create-table migration",
		Example: "  kiln create User name:string email:string:unique age:int:null\n  kiln create Post title:string body:text user:references --soft-delete",
		Args:    cobra.MinimumNArgs(1),
		RunE:    run(func(p *gen.Project, a []string) error { return p.Create(a[0], a[1:], co) }),
	}
	createCmd.Flags().BoolVar(&co.NoTimestamps, "no-timestamps", false, "omit created_at / updated_at")
	createCmd.Flags().BoolVar(&co.SoftDelete, "soft-delete", false, "add deleted_at for soft deletes")
	createCmd.Flags().StringVar(&co.Table, "table", "", "table name (default: snake_case plural of the model)")

	addColumnCmd := &cobra.Command{
		Use:     "add-column <Model> <field:type>...",
		Aliases: []string{"add"},
		Short:   "Add columns to a model and generate the migration",
		Example: "  kiln add-column User bio:text:null score:int:default=0",
		Args:    cobra.MinimumNArgs(2),
		RunE:    run(func(p *gen.Project, a []string) error { return p.AddColumns(a[0], a[1:]) }),
	}

	dropColumnCmd := &cobra.Command{
		Use:     "drop-column <Model> <column>...",
		Aliases: []string{"remove"},
		Short:   "Remove columns from a model and generate the migration",
		Args:    cobra.MinimumNArgs(2),
		RunE:    run(func(p *gen.Project, a []string) error { return p.DropColumns(a[0], a[1:]) }),
	}

	renameColumnCmd := &cobra.Command{
		Use:   "rename-column <Model> <old> <new>",
		Short: "Rename a column in a model and generate the migration",
		Args:  cobra.ExactArgs(3),
		RunE:  run(func(p *gen.Project, a []string) error { return p.RenameColumn(a[0], a[1], a[2]) }),
	}

	var unique bool
	addIndexCmd := &cobra.Command{
		Use:     "add-index <Model|table> <column>...",
		Short:   "Generate a create-index migration",
		Example: "  kiln add-index User email --unique\n  kiln add-index Post user_id created_at",
		Args:    cobra.MinimumNArgs(2),
		RunE:    run(func(p *gen.Project, a []string) error { return p.AddIndex(a[0], a[1:], unique) }),
	}
	addIndexCmd.Flags().BoolVarP(&unique, "unique", "u", false, "unique index")

	var dropUnique bool
	dropIndexCmd := &cobra.Command{
		Use:   "drop-index <Model|table> <column>...",
		Short: "Generate a drop-index migration",
		Args:  cobra.MinimumNArgs(2),
		RunE:  run(func(p *gen.Project, a []string) error { return p.DropIndex(a[0], a[1:], dropUnique) }),
	}
	dropIndexCmd.Flags().BoolVarP(&dropUnique, "unique", "u", false, "the index is unique")

	var dsn string
	migrateCmd := &cobra.Command{
		Use:   "migrate [up|down [n]|redo|status|sql]",
		Short: "Run migrations (go run of the generated runner)",
		Long: `Runs the project's migrate command (created by kiln init).
The connection string is read from DATABASE_URL or --dsn.

  up         apply all pending migrations (default)
  down [n]   roll back the last n migrations (default 1)
  redo       roll back and re-apply the latest migration
  status     list migrations and whether they are applied
  sql        print the SQL of pending migrations without running it`,
		Args: cobra.MaximumNArgs(2),
		RunE: run(func(p *gen.Project, a []string) error { return p.Migrate(a, dsn) }),
	}
	migrateCmd.Flags().StringVar(&dsn, "dsn", "", "database URL (overrides DATABASE_URL)")

	// diff
	var do gen.DiffOptions
	diffCmd := &cobra.Command{
		Use:     "diff [migration_name]",
		Aliases: []string{"auto", "generate"},
		Short:   "Generate a migration from the differences between models and schema",
		Long: `Compares the model structs with the schema produced by the existing
migrations (replayed in memory, no database needed) and writes a migration
with the differences. With --db, compares against the live database instead.

Models are authoritative for tables that have a model: their columns, types
and nullability, plus whatever their schema tags declare (type, limit,
unique, index, references, default...). Tables without a model and indexes
the models don't mention are left alone.`,
		Example: "  kiln diff add_profiles\n  kiln diff --dry-run\n  kiln diff --db",
		Args:    cobra.MaximumNArgs(1),
		RunE: run(func(p *gen.Project, a []string) error {
			if len(a) == 1 {
				do.Name = a[0]
			}
			return p.Diff(do)
		}),
	}
	diffCmd.Flags().BoolVar(&do.DB, "db", false, "compare with the live database (DATABASE_URL)")
	diffCmd.Flags().StringVar(&do.DSN, "dsn", "", "database URL for --db (overrides DATABASE_URL)")
	diffCmd.Flags().BoolVarP(&do.DryRun, "dry-run", "n", false, "print the migration instead of writing it")

	// gen
	var gopts gen.GenOptions
	genCmd := &cobra.Command{
		Use:     "gen",
		Aliases: []string{"cols"},
		Short:   "Generate typed columns for the models (kiln_cols.go)",
		Long: `Writes kiln_cols.go into the models package with one typed column per
field, so column names and value types are checked by the compiler:

  users.FindAll(ctx, models.UserCols.Email.Eq("a@b.c"), models.UserCols.Age.Gte(18))

Once the file exists, create/add-column/drop-column/rename-column refresh it
automatically. Run it again after editing models by hand, or add
//go:generate kiln gen to a models file. --check fails if the file is stale
(useful in CI).`,
		Example: "  kiln gen\n  kiln gen --check",
		Args:    cobra.NoArgs,
		RunE:    run(func(p *gen.Project, a []string) error { return p.Gen(gopts) }),
	}
	genCmd.Flags().BoolVar(&gopts.Check, "check", false, "fail if kiln_cols.go is out of date instead of writing it")

	// scaffold
	var so gen.ScaffoldOptions
	scaffoldCmd := &cobra.Command{
		Use:   "scaffold <Model>",
		Short: "Write a repository interface, its kiln implementation and a usecase for a model",
		Long: `Writes clean-architecture layers for an existing model, once:

  internal/domain/user_repository.go      UserRepository interface (the port)
  internal/repository/user_repository.go  kiln implementation
  internal/usecase/user_usecase.go        usecase depending only on the interface

They are a starting point: trim the interface to what the usecase needs and
add specific methods (FindByEmail...). Use it for models with business
logic; simple models can use orm.For[T](db) directly. Existing files are
never overwritten without --force.`,
		Example: "  kiln scaffold User",
		Args:    cobra.ExactArgs(1),
		RunE:    run(func(p *gen.Project, a []string) error { return p.Scaffold(a[0], so) }),
	}
	scaffoldCmd.Flags().BoolVar(&so.Force, "force", false, "overwrite existing layer files")

	// lint
	var lo gen.LintOptions
	lintCmd := &cobra.Command{
		Use:   "lint",
		Short: "Check migrations for changes that are risky in production",
		Long: `Checks migrations for operations that can break production: data loss
(dropping tables or columns), statements that fail once tables hold rows
(NOT NULL without a default, unique indexes over duplicates), long write
locks (CREATE INDEX on PostgreSQL), and renames that break the app version
still running during a deploy.

With DATABASE_URL (or --dsn) it checks the migrations not yet applied;
without one, the newest migration. --all checks every migration.

With a tenant column (kiln.json "tenant_column", or --tenant-column) it
also checks that every table with that column has an index starting with
it; without one, each tenant query scans every tenant's rows.

Errors fail the command; --strict fails on warnings too (useful in CI).
After reviewing a finding, silence it in the migration with
  s.AllowUnsafe("why it is safe", "check-name")`,
		Example: "  kiln lint\n  kiln lint --all --strict\n  kiln lint --dsn $DATABASE_URL\n  kiln lint --tenant-column tenant_id",
		Args:    cobra.NoArgs,
		RunE:    run(func(p *gen.Project, a []string) error { return p.Lint(lo) }),
	}
	lintCmd.Flags().BoolVar(&lo.All, "all", false, "check every migration")
	lintCmd.Flags().BoolVar(&lo.Strict, "strict", false, "fail on warnings too")
	lintCmd.Flags().StringVar(&lo.DSN, "dsn", "", "database URL (overrides DATABASE_URL)")
	lintCmd.Flags().StringVar(&lo.TenantColumn, "tenant-column", "", `also check that tables with this column have an index starting with it (default: kiln.json "tenant_column")`)

	// pull
	var po gen.PullOptions
	pullCmd := &cobra.Command{
		Use:     "pull [table...]",
		Aliases: []string{"introspect"},
		Short:   "Generate models from an existing database",
		Long: `Inspects the database (DATABASE_URL or --dsn) and writes a model for every
table that has none yet. Pass table names to limit it; --force overwrites.
The generated schema tags make 'kiln diff --db' report no changes.`,
		RunE: run(func(p *gen.Project, a []string) error {
			po.Tables = a
			return p.Pull(po)
		}),
	}
	pullCmd.Flags().StringVar(&po.DSN, "dsn", "", "database URL (overrides DATABASE_URL)")
	pullCmd.Flags().BoolVar(&po.Force, "force", false, "overwrite existing model files")

	// seeds
	newSeedCmd := &cobra.Command{
		Use:   "new-seed <name>",
		Short: "Create a data seeder",
		Args:  cobra.ExactArgs(1),
		RunE:  run(func(p *gen.Project, a []string) error { return p.NewSeed(a[0]) }),
	}
	var seedDSN string
	seedCmd := &cobra.Command{
		Use:   "seed [name...]",
		Short: "Run all seeders, or the named ones",
		RunE: run(func(p *gen.Project, a []string) error {
			return p.Migrate(append([]string{"seed"}, a...), seedDSN)
		}),
	}
	seedCmd.Flags().StringVar(&seedDSN, "dsn", "", "database URL (overrides DATABASE_URL)")

	rootCmd.AddCommand(initCmd, newCmd, createCmd, addColumnCmd, dropColumnCmd, renameColumnCmd, genCmd,
		addIndexCmd, dropIndexCmd, migrateCmd, diffCmd, lintCmd, scaffoldCmd, pullCmd, newSeedCmd, seedCmd)
}
