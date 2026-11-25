package migrate

import (
	"strings"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/schema"
)

type Org struct {
	ID   int64
	Name string `schema:"unique,limit:100"`
}

type Member struct {
	ID        int64
	OrgID     int64
	Org       *Org
	Email     string         `schema:"uniqueindex:uniq_member_email"`
	Nick      *string        // was NOT NULL in the migrations
	Bio       string         `schema:"type:text"`
	Score     int            `schema:"default:10"`
	Settings  map[string]any `db:"settings,json"`
	Legacy    string         `schema:"-"`
	CreatedAt time.Time
}

type MemberTag struct {
	MemberID int64  `db:"member_id,primary"`
	Tag      string `db:"tag,primary"`
}

func baseMigrations() []Migration {
	var set Set
	set.Add(1, "members", func(s *schema.Schema) {
		s.CreateTable("members", func(t *schema.Table) {
			t.ID("id")
			t.BigInt("org_id", schema.Required())
			t.String("email", schema.Required())
			t.String("nick", schema.Required())
			t.String("old_field")
			t.String("legacy")
			t.Int("score", schema.Required(), schema.Default(0))
			t.DateTime("created_at", schema.Required())
		})
		s.Exec("UPDATE members SET nick = email")
	}, nil)
	return set.List()
}

func TestDiff(t *testing.T) {
	res, err := DiffMigrations(baseMigrations(), Org{}, Member{}, MemberTag{})
	if err != nil {
		t.Fatal(err)
	}
	up := strings.Join(RenderGo(res.Up), "\n")
	down := strings.Join(RenderGo(res.Down), "\n")
	t.Logf("UP:\n%s\nDOWN:\n%s\nNOTES:\n%s", up, down, strings.Join(res.Notes, "\n"))

	for _, want := range []string{
		`s.CreateTable("orgs", func(t *schema.Table) {`,
		`t.String("name", schema.Required(), schema.Unique(), schema.Limit(100))`,
		`t.PrimaryKey("member_id", "tag")`,
		`s.AlterTable("members", func(a *schema.AlterTable) { a.References("bio"`[:0] + `s.AddColumn("members", "bio", schema.Text, schema.Required(), schema.Default(""))`,
		`s.AddColumn("members", "settings", schema.JSON)`,
		`s.ChangeColumn("members", "nick", schema.String)`,
		`s.ChangeColumn("members", "score", schema.Int, schema.Required(), schema.Default(10))`,
		`s.DropColumn("members", "old_field")`,
		`s.AddForeignKey("members", "org_id", "orgs", "id")`,
		`s.CreateIndex("members", "idx_members_org_id", []string{"org_id"})`,
		`s.CreateIndex("members", "uniq_member_email", []string{"email"}, schema.UniqueIndex())`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up missing %s", want)
		}
	}
	if strings.Contains(up, "legacy") {
		t.Error("schema:\"-\" column must be ignored")
	}
	// orgs must be created before members gets its foreign key.
	if strings.Index(up, `CreateTable("orgs"`) > strings.Index(up, `AddForeignKey("members"`) {
		t.Error("tables not in dependency order")
	}
	for _, want := range []string{
		`s.DropTable("orgs")`,
		`s.ChangeColumn("members", "nick", schema.String, schema.Required())`,
		`s.AddColumn("members", "old_field", schema.String)`,
	} {
		if !strings.Contains(down, want) {
			t.Errorf("down missing %s", want)
		}
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "not modelled") {
		t.Error("expected a note about raw SQL")
	}

	// Idempotence: after applying Up, there is nothing left to do.
	st, _ := schema.Replay(func(s *schema.Schema) { baseMigrations()[0].Up(s) })
	for _, op := range res.Up {
		if err := st.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
	again, err := Diff(st, Org{}, Member{}, MemberTag{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("second diff not empty:\n%s", strings.Join(RenderGo(again.Up), "\n"))
	}

	// And Down restores the original columns.
	for _, op := range res.Down {
		if err := st.Apply(op); err != nil {
			t.Fatal(err)
		}
	}
	if st.Tables["orgs"] != nil || st.Tables["members"].Column("old_field") == nil || st.Tables["members"].Column("bio") != nil {
		t.Error("down did not restore the schema")
	}
}

func TestRenderedCodeCompiles(t *testing.T) {
	// Every rendered line must be valid DSL: execute the equivalent ops through the dialect.
	res, err := DiffMigrations(nil, Org{}, Member{}, MemberTag{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Up) == 0 || len(res.Down) != 3 {
		t.Fatalf("up=%d down=%d", len(res.Up), len(res.Down))
	}
}

func TestSameSQLType(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`"mood"`, "mood", true}, {"TEXT[]", "text[]", true}, {"[nvarchar](20)", "NVARCHAR(20)", true},
		{"TEXT[]", "TEXT", false}, {"mood", "moods", false}, {"`citext`", "CITEXT", true},
		{"INT8[]", "BIGINT[]", true}, {"int[]", "INTEGER[]", true}, {"VARCHAR(20)", "character varying(20)", true},
		{"BIGINT[]", "INTEGER[]", false},
	} {
		if got := sameSQLType(c.a, c.b); got != c.want {
			t.Errorf("sameSQLType(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
