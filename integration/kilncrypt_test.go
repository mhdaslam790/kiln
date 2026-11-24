package integration

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/kilncrypt"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

type Patient struct {
	ID         int64
	Name       string
	Email      kilncrypt.String
	EmailIndex *string
	Diagnosis  kilncrypt.String
}

var patientsTable = migrate.Migration{Version: 20990106000001, Name: "create_patients",
	Up: func(s *schema.Schema) {
		s.CreateTable("patients", func(t *schema.Table) {
			t.ID("id")
			t.String("name", schema.Required())
			t.Text("email")
			t.String("email_index", schema.Limit(32))
			t.Text("diagnosis")
			t.Index([]string{"email_index"})
		})
	},
	Down: func(s *schema.Schema) { s.DropTable("patients") },
}

func cryptKey(id uint32, b byte) kilncrypt.Key {
	return kilncrypt.Key{ID: id, Secret: bytes.Repeat([]byte{b}, kilncrypt.KeySize)}
}

func TestKilncrypt(t *testing.T) {
	ctx := context.Background()
	indexKey := bytes.Repeat([]byte{'i'}, kilncrypt.KeySize)
	keys, err := kilncrypt.NewKeys(indexKey, cryptKey(1, 'a'))
	must(t, err)
	email, err := keys.Index("email", "email_index", strings.ToLower)
	must(t, err)

	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{kilncrypt.Plugin(keys, email)}
	}, func(t *testing.T, db *orm.DB) {
		m := migrate.New(db, append(migrations(), patientsTable))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })
		patients := orm.For[Patient](db)

		ann := Patient{Name: "ann", Email: kilncrypt.NewString("Ann@Clinic.io"), Diagnosis: kilncrypt.NewString("flu")}
		must(t, patients.Insert(ctx, &ann))
		must(t, patients.InsertAll(ctx, []Patient{
			{Name: "bob", Email: kilncrypt.NewString("bob@clinic.io")},
			{Name: "cy"}, // NULLs
		}))

		// The database holds ciphertext only.
		var raw []struct {
			Email     *string `db:"email"`
			Index     *string `db:"email_index"`
			Diagnosis *string `db:"diagnosis"`
		}
		must(t, db.Query(ctx, &raw, "SELECT email, email_index, diagnosis FROM patients ORDER BY id"))
		if raw[0].Email == nil || !strings.HasPrefix(*raw[0].Email, "kc1:") || strings.Contains(*raw[0].Email, "Ann") ||
			raw[0].Diagnosis == nil || strings.Contains(*raw[0].Diagnosis, "flu") || raw[0].Index == nil || len(*raw[0].Index) != 32 {
			t.Fatalf("stored row: %+v", raw[0])
		}
		if raw[2].Email != nil || raw[2].Index != nil || raw[2].Diagnosis != nil {
			t.Fatalf("NULLs stored as %+v", raw[2])
		}

		// Search by blind index (normalized), decrypt on demand.
		got, err := patients.Find(ctx, email.Eq("ANN@clinic.io"))
		must(t, err)
		if got.ID != ann.ID {
			t.Fatalf("found %d, want %d", got.ID, ann.ID)
		}
		if plain, err := got.Email.Open(keys); err != nil || plain != "Ann@Clinic.io" {
			t.Fatalf("Open = %q, %v", plain, err)
		}
		if n, err := patients.Count(ctx, email.In("bob@clinic.io", "nobody@x.io")); err != nil || n != 1 {
			t.Fatalf("In: %d %v", n, err)
		}

		// A full update of a loaded record keeps the ciphertext as it is.
		before := *raw[0].Email
		got.Name = "ann b"
		must(t, patients.Update(ctx, &got))
		must(t, db.Query(ctx, &raw, "SELECT email, email_index, diagnosis FROM patients ORDER BY id"))
		if *raw[0].Email != before {
			t.Fatal("unchanged value was re-encrypted")
		}

		// Changing the value updates the index; a partial update must write both.
		got.Email = kilncrypt.NewString("ann@new.io")
		if err := patients.Update(ctx, &got, "Email"); err == nil || !strings.Contains(err.Error(), "email_index") {
			t.Fatalf("partial update without the index: %v", err)
		}
		must(t, patients.Update(ctx, &got, "Email", "EmailIndex"))
		if _, err := patients.Find(ctx, email.Eq("ann@new.io")); err != nil {
			t.Fatalf("lookup after change: %v", err)
		}
		if _, err := patients.Find(ctx, email.Eq("Ann@Clinic.io")); !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("old value still found: %v", err)
		}

		// UpdateAny needs a sealed value: plaintext is refused, Seal works.
		if _, err := patients.UpdateAny(ctx, q.Eq("name", "cy"), q.Set("diagnosis", kilncrypt.NewString("cold"))); !errors.Is(err, kilncrypt.ErrNotSealed) {
			t.Fatalf("UpdateAny with plaintext: %v", err)
		}
		sealed, err := keys.Seal("cold")
		must(t, err)
		_, err = patients.UpdateAny(ctx, q.Eq("name", "cy"), q.Set("diagnosis", sealed))
		must(t, err)
		cy, err := patients.Find(ctx, q.Eq("name", "cy"))
		must(t, err)
		if d, _ := cy.Diagnosis.Open(keys); d != "cold" || !cy.Email.IsNull() {
			t.Fatalf("cy: %q, email null %v", d, cy.Email.IsNull())
		}

		// Rotation: new current key, old key kept for reading.
		newKeys, err := kilncrypt.NewKeys(indexKey, cryptKey(2, 'b'), cryptKey(1, 'a'))
		must(t, err)
		email2, err := newKeys.Index("email", "email_index", strings.ToLower)
		must(t, err)
		rdb := orm.Open(db.SQL(), db.Dialect(), kilncrypt.Plugin(newKeys, email2))
		n, err := kilncrypt.Rotate[Patient](ctx, rdb, newKeys)
		must(t, err)
		if n != 3 {
			t.Fatalf("rotated %d records, want 3", n)
		}
		if n, err := kilncrypt.Rotate[Patient](ctx, rdb, newKeys); err != nil || n != 0 {
			t.Fatalf("second Rotate: %d %v", n, err)
		}
		onlyNew, err := kilncrypt.NewKeys(indexKey, cryptKey(2, 'b'))
		must(t, err)
		all, err := orm.For[Patient](rdb).FindAll(ctx, email2.Eq("ann@new.io"))
		must(t, err)
		if len(all) != 1 {
			t.Fatalf("index lookup after rotation: %d", len(all))
		}
		if v, err := all[0].Email.Open(onlyNew); err != nil || v != "ann@new.io" {
			t.Fatalf("open without the old key = %q, %v", v, err)
		}
		if _, err := got.Email.Open(onlyNew); err != nil {
			t.Fatalf("in-memory value: %v", err) // still holds its plaintext
		}

		// Without the plugin, plaintext is never written.
		plain := orm.Open(db.SQL(), db.Dialect())
		if err := orm.For[Patient](plain).Insert(ctx, &Patient{Name: "x", Email: kilncrypt.NewString("x")}); !errors.Is(err, kilncrypt.ErrNotSealed) {
			t.Fatalf("insert without plugin: %v", err)
		}
	})
}
