package dialect

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

var all = map[Kind]Dialect{
	KindPostgres: Postgres(), KindMySQL: MySQL(), KindSQLite: SQLite(), KindMSSQL: MSSQL(),
}

type want map[Kind]string

func check(t *testing.T, name string, got map[Kind]string, w want) {
	t.Helper()
	for k, exp := range w {
		if got[k] != exp {
			t.Errorf("%s [%s]\n got: %s\nwant: %s", name, k, got[k], exp)
		}
	}
}

func selectAll(t *testing.T, qq q.Query) (map[Kind]string, map[Kind][]any) {
	t.Helper()
	sqls, args := map[Kind]string{}, map[Kind][]any{}
	for k, d := range all {
		st, err := d.Select(qq)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		sqls[k], args[k] = st.SQL, st.Args
	}
	return sqls, args
}

func TestSelect(t *testing.T) {
	qq := q.From("users").
		Select("id", "name").
		Where(q.Eq("active", true), q.Or(q.Gt("age", 18), q.Null("age"))).
		Sort("-created_at", "id").
		Limit(10).Offset(20)
	got, args := selectAll(t, qq)
	check(t, "select", got, want{
		KindPostgres: `SELECT "id", "name" FROM "users" WHERE ("active" = $1 AND ("age" > $2 OR "age" IS NULL)) ORDER BY "created_at" DESC, "id" ASC LIMIT 10 OFFSET 20`,
		KindMySQL:    "SELECT `id`, `name` FROM `users` WHERE (`active` = ? AND (`age` > ? OR `age` IS NULL)) ORDER BY `created_at` DESC, `id` ASC LIMIT 10 OFFSET 20",
		KindSQLite:   `SELECT "id", "name" FROM "users" WHERE ("active" = ? AND ("age" > ? OR "age" IS NULL)) ORDER BY "created_at" DESC, "id" ASC LIMIT 10 OFFSET 20`,
		KindMSSQL:    `SELECT [id], [name] FROM [users] WHERE ([active] = @p1 AND ([age] > @p2 OR [age] IS NULL)) ORDER BY [created_at] DESC, [id] ASC OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY`,
	})
	if !reflect.DeepEqual(args[KindPostgres], []any{true, 18}) {
		t.Errorf("args = %v", args[KindPostgres])
	}
}

func TestSelectLimitOnlyAndLock(t *testing.T) {
	got, _ := selectAll(t, q.From("jobs").Where(q.Eq("state", "new")).Limit(1).ForUpdate())
	check(t, "lock", got, want{
		KindPostgres: `SELECT * FROM "jobs" WHERE "state" = $1 LIMIT 1 FOR UPDATE`,
		KindMySQL:    "SELECT * FROM `jobs` WHERE `state` = ? LIMIT 1 FOR UPDATE",
		KindSQLite:   `SELECT * FROM "jobs" WHERE "state" = ? LIMIT 1`,
		KindMSSQL:    `SELECT TOP (1) * FROM [jobs] WITH (UPDLOCK, ROWLOCK) WHERE [state] = @p1`,
	})
}

func TestFiltersAndJoins(t *testing.T) {
	qq := q.From("posts").
		Select("posts.*", "COUNT(c.id) AS comments").
		LeftJoin("comments c", "c.post_id = posts.id AND c.flagged = ?", false).
		Where(
			q.In("posts.status", "draft", "live"),
			q.NotIn("posts.id"),
			q.In("posts.author_id", q.From("users").Select("id").Where(q.Eq("banned", false))),
			q.Expr("lower(title) LIKE ?", "%go%"),
			q.Not(q.Eq("posts.deleted", nil)),
			q.Eq("posts.updated_at", q.F("posts.created_at")),
		).
		Group("posts.id").
		Having(q.Gt("COUNT(c.id)", 2))
	got, args := selectAll(t, qq)
	check(t, "joins", got, want{
		KindPostgres: `SELECT "posts".*, COUNT(c.id) AS comments FROM "posts" LEFT JOIN comments c ON c.post_id = posts.id AND c.flagged = $1 WHERE ("posts"."status" IN ($2, $3) AND 1=1 AND "posts"."author_id" IN (SELECT "id" FROM "users" WHERE "banned" = $4) AND lower(title) LIKE $5 AND NOT ("posts"."deleted" IS NULL) AND "posts"."updated_at" = "posts"."created_at") GROUP BY "posts"."id" HAVING COUNT(c.id) > $6`,
	})
	if len(args[KindPostgres]) != 6 {
		t.Errorf("args = %v", args[KindPostgres])
	}
}

func TestEmptyIn(t *testing.T) {
	got, _ := selectAll(t, q.From("t").Where(q.In("id")))
	check(t, "empty in", got, want{KindSQLite: `SELECT * FROM "t" WHERE 1=0`})
}

func TestOffsetWithoutLimit(t *testing.T) {
	got, _ := selectAll(t, q.From("t").Offset(5))
	check(t, "offset", got, want{
		KindPostgres: `SELECT * FROM "t" OFFSET 5`,
		KindMySQL:    "SELECT * FROM `t` LIMIT 18446744073709551615 OFFSET 5",
		KindSQLite:   `SELECT * FROM "t" LIMIT -1 OFFSET 5`,
		KindMSSQL:    `SELECT * FROM [t] ORDER BY (SELECT NULL) OFFSET 5 ROWS`,
	})
}

func TestCount(t *testing.T) {
	got := map[Kind]string{}
	for k, d := range all {
		st, err := d.Count(q.From("users").Where(q.Eq("a", 1)).Sort("id").Limit(3))
		if err != nil {
			t.Fatal(err)
		}
		got[k] = st.SQL
	}
	check(t, "count", got, want{KindPostgres: `SELECT COUNT(*) FROM "users" WHERE "a" = $1`})

	st, _ := Postgres().Count(q.From("users").Group("role"))
	if st.SQL != `SELECT COUNT(*) FROM (SELECT * FROM "users" GROUP BY "role") "t"` {
		t.Errorf("grouped count: %s", st.SQL)
	}
}

func TestRawRebind(t *testing.T) {
	st, err := Postgres().(*sqlDialect).Raw("SELECT '?', ? , ?? FROM t WHERE x = ?", []any{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if st.SQL != "SELECT '?', $1 , ? FROM t WHERE x = $2" {
		t.Errorf("got %s", st.SQL)
	}
	if _, err := MySQL().Raw("a = ? AND b = ?", []any{1}); err == nil {
		t.Error("expected arg count error")
	}
}

func TestInsert(t *testing.T) {
	ins := Insert{Table: "users", Columns: []string{"name", "age"}, Rows: [][]any{{"a", 1}, {"b", 2}}, Returning: "id"}
	got := map[Kind]string{}
	for k, d := range all {
		st, err := d.Insert(ins)
		if err != nil {
			t.Fatal(err)
		}
		got[k] = st.SQL
	}
	check(t, "insert", got, want{
		KindPostgres: `INSERT INTO "users" ("name", "age") VALUES ($1, $2), ($3, $4) RETURNING "id"`,
		KindMySQL:    "INSERT INTO `users` (`name`, `age`) VALUES (?, ?), (?, ?)",
		KindSQLite:   `INSERT INTO "users" ("name", "age") VALUES (?, ?), (?, ?) RETURNING "id"`,
		KindMSSQL:    `MERGE INTO [users] USING (VALUES (@p1, @p2, 0), (@p3, @p4, 1)) AS s ([name], [age], [kiln_pos]) ON 1 = 0 WHEN NOT MATCHED THEN INSERT ([name], [age]) VALUES (s.[name], s.[age]) OUTPUT s.[kiln_pos], INSERTED.[id];`,
	})
}

func TestInsertDefaultValuesAndUpsert(t *testing.T) {
	got := map[Kind]string{}
	for k, d := range all {
		st, _ := d.Insert(Insert{Table: "t", Returning: "id"})
		got[k] = st.SQL
	}
	check(t, "default values", got, want{
		KindPostgres: `INSERT INTO "t" DEFAULT VALUES RETURNING "id"`,
		KindMySQL:    "INSERT INTO `t` () VALUES ()",
		KindMSSQL:    `INSERT INTO [t] OUTPUT INSERTED.[id] DEFAULT VALUES`,
	})

	up := Insert{Table: "t", Columns: []string{"k", "v"}, Rows: [][]any{{1, 2}},
		OnConflict: &q.OnConflict{Columns: []string{"k"}, Update: []string{"v"}}}
	got = map[Kind]string{}
	for _, k := range []Kind{KindPostgres, KindMySQL, KindSQLite} {
		st, err := all[k].Insert(up)
		if err != nil {
			t.Fatal(err)
		}
		got[k] = st.SQL
	}
	check(t, "upsert", got, want{
		KindPostgres: `INSERT INTO "t" ("k", "v") VALUES ($1, $2) ON CONFLICT ("k") DO UPDATE SET "v" = EXCLUDED."v"`,
		KindMySQL:    "INSERT INTO `t` (`k`, `v`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `v` = VALUES(`v`)",
	})
	st, _ := MySQL().Insert(Insert{Table: "t", Columns: []string{"k"}, Rows: [][]any{{1}}, OnConflict: &q.OnConflict{Columns: []string{"k"}, DoNothing: true}})
	if st.SQL != "INSERT INTO `t` (`k`) VALUES (?) ON DUPLICATE KEY UPDATE `k` = `k`" {
		t.Errorf("mysql do nothing: %s", st.SQL)
	}
	if _, err := MSSQL().Insert(up); err == nil {
		t.Error("expected MSSQL upsert error")
	}
}

func TestUpdateDelete(t *testing.T) {
	st, err := Postgres().Update("users", []q.Mutation{q.Set("name", "x"), q.Inc("logins", 1), q.SetExpr("score", "score * ?", 2)}, q.Eq("id", 7))
	if err != nil {
		t.Fatal(err)
	}
	if st.SQL != `UPDATE "users" SET "name" = $1, "logins" = "logins" + $2, "score" = score * $3 WHERE "id" = $4` {
		t.Errorf("update: %s", st.SQL)
	}
	st, _ = MSSQL().Delete("users", q.Eq("id", 7))
	if st.SQL != `DELETE FROM [users] WHERE [id] = @p1` {
		t.Errorf("delete: %s", st.SQL)
	}
}

func ddl(t *testing.T, fn func(s *schema.Schema)) map[Kind]string {
	t.Helper()
	var s schema.Schema
	fn(&s)
	out := map[Kind]string{}
	for k, d := range all {
		var parts []string
		for _, op := range s.Ops {
			stmts, err := d.DDL(op)
			if err != nil {
				parts = append(parts, "ERR: "+err.Error())
				continue
			}
			for _, st := range stmts {
				parts = append(parts, st.SQL)
			}
		}
		out[k] = strings.Join(parts, ";\n")
	}
	return out
}

func TestCreateTable(t *testing.T) {
	got := ddl(t, func(s *schema.Schema) {
		s.CreateTable("posts", func(t *schema.Table) {
			t.ID("id")
			t.String("title", schema.Required(), schema.Limit(120))
			t.Bool("published", schema.Default(false))
			t.Decimal("price", schema.Precision(10), schema.Scale(2))
			t.JSON("meta")
			t.References("user_id", "users", schema.OnDelete("CASCADE"))
			t.Timestamps()
			t.Unique("user_id", "title")
		})
	})
	check(t, "create table", got, want{
		KindPostgres: `CREATE TABLE "posts" ("id" BIGSERIAL NOT NULL PRIMARY KEY, "title" VARCHAR(120) NOT NULL, "published" BOOLEAN DEFAULT FALSE, "price" DECIMAL(10,2), "meta" JSONB, "user_id" BIGINT, "created_at" TIMESTAMPTZ NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, CONSTRAINT "uniq_posts_user_id_title" UNIQUE ("user_id", "title"), CONSTRAINT "fk_posts_user_id" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE CASCADE);
CREATE INDEX "idx_posts_user_id" ON "posts" ("user_id")`,
		KindMySQL: "CREATE TABLE `posts` (`id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, `title` VARCHAR(120) NOT NULL, `published` BOOL DEFAULT FALSE, `price` DECIMAL(10,2), `meta` JSON, `user_id` BIGINT, `created_at` DATETIME NOT NULL, `updated_at` DATETIME NOT NULL, CONSTRAINT `uniq_posts_user_id_title` UNIQUE (`user_id`, `title`), CONSTRAINT `fk_posts_user_id` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE);\nCREATE INDEX `idx_posts_user_id` ON `posts` (`user_id`)",
		KindSQLite: `CREATE TABLE "posts" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "title" VARCHAR(120) NOT NULL, "published" BOOLEAN DEFAULT 0, "price" DECIMAL(10,2), "meta" TEXT, "user_id" BIGINT, "created_at" DATETIME NOT NULL, "updated_at" DATETIME NOT NULL, CONSTRAINT "uniq_posts_user_id_title" UNIQUE ("user_id", "title"), CONSTRAINT "fk_posts_user_id" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE CASCADE);
CREATE INDEX "idx_posts_user_id" ON "posts" ("user_id")`,
		KindMSSQL: `CREATE TABLE [posts] ([id] BIGINT NOT NULL IDENTITY(1,1) PRIMARY KEY, [title] NVARCHAR(120) NOT NULL, [published] BIT DEFAULT 0, [price] DECIMAL(10,2), [meta] NVARCHAR(MAX), [user_id] BIGINT, [created_at] DATETIME2 NOT NULL, [updated_at] DATETIME2 NOT NULL, CONSTRAINT [uniq_posts_user_id_title] UNIQUE ([user_id], [title]), CONSTRAINT [fk_posts_user_id] FOREIGN KEY ([user_id]) REFERENCES [users] ([id]) ON DELETE CASCADE);
CREATE INDEX [idx_posts_user_id] ON [posts] ([user_id])`,
	})
}

func TestCompositeKeyAndIfNotExists(t *testing.T) {
	got := ddl(t, func(s *schema.Schema) {
		s.CreateTableIfNotExists("tags_posts", func(t *schema.Table) {
			t.BigInt("tag_id", schema.Primary())
			t.BigInt("post_id", schema.Primary())
		})
	})
	check(t, "composite", got, want{
		KindPostgres: `CREATE TABLE IF NOT EXISTS "tags_posts" ("tag_id" BIGINT, "post_id" BIGINT, PRIMARY KEY ("tag_id", "post_id"))`,
		KindMSSQL:    `IF OBJECT_ID('tags_posts', 'U') IS NULL CREATE TABLE [tags_posts] ([tag_id] BIGINT, [post_id] BIGINT, PRIMARY KEY ([tag_id], [post_id]))`,
	})
}

func TestAlterOps(t *testing.T) {
	got := ddl(t, func(s *schema.Schema) {
		s.AlterTable("users", func(t *schema.AlterTable) {
			t.String("nickname", schema.Default("anon"))
			t.RenameColumn("name", "full_name")
			t.DropColumn("legacy")
		})
		s.RenameTable("users", "people")
		s.CreateUniqueIndex("people", "", []string{"email"}, schema.IndexIfNotExists())
		s.DropIndex("people", "uniq_people_email")
		s.ChangeColumn("people", "age", schema.BigInt, schema.Required())
	})
	check(t, "alter", got, want{
		KindPostgres: `ALTER TABLE "users" ADD COLUMN "nickname" VARCHAR(255) DEFAULT 'anon';
ALTER TABLE "users" RENAME COLUMN "name" TO "full_name";
ALTER TABLE "users" DROP COLUMN "legacy";
ALTER TABLE "users" RENAME TO "people";
CREATE UNIQUE INDEX IF NOT EXISTS "uniq_people_email" ON "people" ("email");
DROP INDEX "uniq_people_email";
ALTER TABLE "people" ALTER COLUMN "age" TYPE BIGINT, ALTER COLUMN "age" SET NOT NULL`,
		KindMySQL: "ALTER TABLE `users` ADD COLUMN `nickname` VARCHAR(255) DEFAULT 'anon';\nALTER TABLE `users` RENAME COLUMN `name` TO `full_name`;\nALTER TABLE `users` DROP COLUMN `legacy`;\nRENAME TABLE `users` TO `people`;\nCREATE UNIQUE INDEX `uniq_people_email` ON `people` (`email`);\nDROP INDEX `uniq_people_email` ON `people`;\nALTER TABLE `people` MODIFY COLUMN `age` BIGINT NOT NULL",
		KindSQLite: `ALTER TABLE "users" ADD COLUMN "nickname" VARCHAR(255) DEFAULT 'anon';
ALTER TABLE "users" RENAME COLUMN "name" TO "full_name";
ALTER TABLE "users" DROP COLUMN "legacy";
ALTER TABLE "users" RENAME TO "people";
CREATE UNIQUE INDEX IF NOT EXISTS "uniq_people_email" ON "people" ("email");
DROP INDEX "uniq_people_email";
ERR: dialect: SQLite cannot change a column; recreate the table`,
		KindMSSQL: `ALTER TABLE [users] ADD [nickname] NVARCHAR(255) DEFAULT 'anon';
EXEC sp_rename 'users.name', 'full_name', 'COLUMN';
DECLARE @df sysname, @sql nvarchar(max); SELECT @df = dc.name FROM sys.default_constraints dc JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id WHERE dc.parent_object_id = OBJECT_ID('[users]') AND c.name = 'legacy'; IF @df IS NOT NULL BEGIN SET @sql = N'ALTER TABLE [users] DROP CONSTRAINT ' + QUOTENAME(@df); EXEC sp_executesql @sql; END;
ALTER TABLE [users] DROP COLUMN [legacy];
EXEC sp_rename 'users', 'people';
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'uniq_people_email' AND object_id = OBJECT_ID('people')) CREATE UNIQUE INDEX [uniq_people_email] ON [people] ([email]);
DROP INDEX [uniq_people_email] ON [people];
DECLARE @df sysname, @sql nvarchar(max); SELECT @df = dc.name FROM sys.default_constraints dc JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id WHERE dc.parent_object_id = OBJECT_ID('[people]') AND c.name = 'age'; IF @df IS NOT NULL BEGIN SET @sql = N'ALTER TABLE [people] DROP CONSTRAINT ' + QUOTENAME(@df); EXEC sp_executesql @sql; END;
ALTER TABLE [people] ALTER COLUMN [age] BIGINT NOT NULL`,
	})
}

func TestClassify(t *testing.T) {
	cases := []struct {
		d   Dialect
		msg string
		k   ErrorKind
	}{
		{Postgres(), `ERROR: duplicate key value violates unique constraint "x" (SQLSTATE 23505)`, ErrUnique},
		{MySQL(), "Error 1062 (23000): Duplicate entry 'a' for key 'x'", ErrUnique},
		{SQLite(), "constraint failed: UNIQUE constraint failed: users.email (2067)", ErrUnique},
		{SQLite(), "FOREIGN KEY constraint failed", ErrForeignKey},
		{MSSQL(), "mssql: Cannot insert duplicate key row in object", ErrUnique},
		{Postgres(), "connection refused", ErrOther},
	}
	for _, c := range cases {
		if got := c.d.Classify(errString(c.msg)); got != c.k {
			t.Errorf("%s %q: got %v want %v", c.d.Kind(), c.msg, got, c.k)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
