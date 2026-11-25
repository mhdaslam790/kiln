package integration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/kilnpg"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

type Mood string

type PgPost struct {
	ID     int64
	Title  string
	Tags   kilnpg.Array[string] `schema:"sqltype:TEXT[]"`
	Scores kilnpg.Array[int64]  `schema:"sqltype:BIGINT[]"`
	Mood   Mood                 `schema:"sqltype:kiln mood"`
}

func (PgPost) TableName() string { return "pg_posts" }

func TestKilnPG(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		if db.Dialect().Kind() != "postgres" {
			t.Skip("PostgreSQL only")
		}
		ctx := context.Background()
		mig := migrate.Migration{Version: 20990111000001, Name: "kilnpg",
			Up: func(s *schema.Schema) {
				kilnpg.CreateEnum(s, "kiln mood", "happy", "sad", "it's ok")
				s.CreateTable("pg_posts", func(t *schema.Table) {
					t.ID("id")
					t.String("title", schema.Required())
					t.Text("tags", schema.SQLType("TEXT[]"))
					t.Text("scores", schema.SQLType("BIGINT[]"))
					t.String("mood", schema.Required(), kilnpg.EnumType("kiln mood"))
				})
			},
			Down: func(s *schema.Schema) {
				s.DropTable("pg_posts")
				kilnpg.DropEnum(s, "kiln mood")
			},
		}
		m := migrate.New(db, append(migrations(), mig))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })

		posts := orm.For[PgPost](db)
		for _, p := range []PgPost{
			{Title: "a", Tags: kilnpg.Array[string]{"go", "sql"}, Scores: kilnpg.Array[int64]{1, 2}, Mood: "happy"},
			{Title: "b", Tags: kilnpg.Array[string]{"rust", `we"ird, tag`}, Scores: kilnpg.Array[int64]{}, Mood: "it's ok"},
			{Title: "c", Mood: "sad"}, // NULL arrays
		} {
			must(t, posts.Insert(ctx, &p))
		}
		titles := func(fs ...q.Querier) string {
			t.Helper()
			list, err := posts.FindAll(ctx, append(fs, q.Sort("title"))...)
			must(t, err)
			var out []string
			for _, p := range list {
				out = append(out, p.Title)
			}
			return strings.Join(out, ",")
		}
		check := func(name, got, want string) {
			t.Helper()
			if got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		check("Has", titles(kilnpg.Has("tags", "go")), "a")
		check("Has odd", titles(kilnpg.Has("tags", `we"ird, tag`)), "b")
		check("Contains", titles(kilnpg.Contains("tags", "go", "sql")), "a")
		check("Contains miss", titles(kilnpg.Contains("tags", "go", "rust")), "")
		check("Overlaps", titles(kilnpg.Overlaps("tags", "sql", "rust")), "a,b")
		check("ContainedBy", titles(kilnpg.ContainedBy("tags", "go", "sql", "zig")), "a")
		check("int array", titles(kilnpg.Has("scores", int64(2))), "a")
		check("enum Eq", titles(q.Eq("mood", Mood("it's ok"))), "b")

		got, err := posts.Find(ctx, q.Eq("title", "b"))
		must(t, err)
		if !slices.Equal(got.Tags, []string{"rust", `we"ird, tag`}) || got.Scores == nil || len(got.Scores) != 0 || got.Mood != "it's ok" {
			t.Errorf("round trip: %#v", got)
		}
		c, err := posts.Find(ctx, q.Eq("title", "c"))
		must(t, err)
		if c.Tags != nil || c.Scores != nil {
			t.Errorf("NULL arrays: %#v", c)
		}
		// Updating an array writes it back.
		got.Tags = append(got.Tags, "new")
		must(t, posts.Update(ctx, &got, "Tags"))
		check("after update", titles(kilnpg.Has("tags", "new")), "b")

		// kiln diff sees the model and its migration as the same schema, and
		// reads the enum statements (nothing opaque).
		res, err := migrate.DiffMigrations(append(migrations(), mig), PgPost{})
		must(t, err)
		for _, op := range res.Up {
			t.Errorf("diff wants a change: %#v", op)
		}
		for _, n := range res.Notes {
			if strings.Contains(n, "not modelled") {
				t.Errorf("diff note: %s", n)
			}
		}
		// The live database reads back the same: enum values, array types.
		live, err := migrate.Inspect(ctx, db)
		must(t, err)
		if got := live.Enums["kiln mood"]; !slices.Equal(got, []string{"happy", "sad", "it's ok"}) {
			t.Errorf("inspected enum = %q", got)
		}
		lt := live.Tables["pg_posts"]
		for col, want := range map[string]string{"tags": "TEXT[]", "scores": "BIGINT[]", "mood": `"kiln mood"`} {
			if c := lt.Column(col); c == nil || c.SQLType != want {
				t.Errorf("inspected %s = %+v, want SQLType %q", col, c, want)
			}
		}
		res, err = migrate.Diff(&schema.State{Tables: map[string]*schema.TableState{"pg_posts": lt}, Enums: live.Enums}, PgPost{})
		must(t, err)
		for _, op := range res.Up {
			t.Errorf("diff --db wants a change: %#v", op)
		}
		// The SQL diff --db writes for an enum whose name needs quotes runs:
		// the model has a column the database doesn't.
		loose := *lt
		loose.Columns = []schema.Column{*lt.Column("id")}
		res, err = migrate.Diff(&schema.State{Tables: map[string]*schema.TableState{"pg_posts": &loose}, Enums: live.Enums}, PgPostLoose{})
		must(t, err)
		if len(res.Up) != 1 {
			t.Fatalf("diff --db = %#v", res.Up)
		}
		change := migrate.Migration{Version: 20990111000002, Name: "loosen", Up: func(s *schema.Schema) { s.Ops = append(s.Ops, res.Up...) },
			Down: func(s *schema.Schema) { s.Ops = append(s.Ops, res.Down...) }}
		m2 := migrate.New(db, append(migrations(), mig, change))
		_, err = m2.Up(ctx)
		must(t, err)
		_, err = m2.Down(ctx, 1)
		must(t, err)

		if !strings.Contains(migrate.ERD(live, "pg_posts"), `"happy | sad | it's ok"`) {
			t.Errorf("ERD has no enum values:\n%s", migrate.ERD(live, "pg_posts"))
		}

		// The database refuses values outside the enum.
		if err := posts.Insert(ctx, &PgPost{Title: "x", Mood: "angry"}); err == nil {
			t.Error("enum accepted an unknown value")
		}
		if _, err := posts.FindAll(ctx, kilnpg.Has("tags", "go"), q.Eq("nope", 1)); err == nil || errors.Is(err, orm.ErrNotFound) {
			t.Error("bad column not reported")
		}
	})
}

// PgPostLoose is pg_posts with one more enum column, its type quoted in
// the tag as kiln pull writes it.
type PgPostLoose struct {
	ID    int64
	Mood2 *Mood `db:"mood2" schema:"sqltype:\"kiln mood\""`
}

func (PgPostLoose) TableName() string { return "pg_posts" }
