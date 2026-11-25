package integration

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/audit"
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
		m := migrate.New(db, append(migrations(), auditTable, patientsTable))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 2) })
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
		if raw[0].Email == nil || !strings.HasPrefix(*raw[0].Email, "kc2:") || strings.Contains(*raw[0].Email, "Ann") ||
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

		// UpdateAny seals q.Set values for their column; the caller's
		// mutations are not changed.
		set := []q.Mutation{q.Set("diagnosis", kilncrypt.NewString("cold"))}
		_, err = patients.UpdateAny(ctx, q.Eq("name", "cy"), set...)
		must(t, err)
		if v, err := set[0].Value.(kilncrypt.String).Value(); v != nil || !errors.Is(err, kilncrypt.ErrNotSealed) {
			t.Fatalf("caller's mutation changed: %v %v", v, err)
		}
		cy, err := patients.Find(ctx, q.Eq("name", "cy"))
		must(t, err)
		if d, err := cy.Diagnosis.Open(keys); d != "cold" || err != nil || !cy.Email.IsNull() {
			t.Fatalf("cy: %q %v, email null %v", d, err, cy.Email.IsNull())
		}
		if _, err := patients.UpdateAny(ctx, q.Eq("name", "cy"), q.Set("name", kilncrypt.NewString("x"))); err == nil {
			t.Fatal("encrypted value set on a plain column")
		}

		// Values are bound to their column: copied into another column
		// with raw SQL, it no longer decrypts.
		_, err = db.Exec(ctx, "UPDATE patients SET diagnosis = email WHERE id = ?", ann.ID)
		must(t, err)
		swapped, err := patients.FindByID(ctx, ann.ID)
		must(t, err)
		if _, err := swapped.Diagnosis.Open(keys); !errors.Is(err, kilncrypt.ErrDecrypt) {
			t.Fatalf("value copied to another column opened: %v", err)
		}
		// Writing the record back stores the bytes unchanged: still refused.
		must(t, patients.Update(ctx, &swapped))
		again, err := patients.FindByID(ctx, ann.ID)
		must(t, err)
		if _, err := again.Diagnosis.Open(keys); !errors.Is(err, kilncrypt.ErrDecrypt) {
			t.Fatalf("copied value opened after a rewrite: %v", err)
		}
		// Re-sealing it (key rotation) checks it first, so it can't be laundered.
		rot, err := kilncrypt.NewKeys(indexKey, cryptKey(9, 'z'), cryptKey(1, 'a'))
		must(t, err)
		if _, err := kilncrypt.Rotate[Patient](ctx, orm.Open(db.SQL(), db.Dialect(), kilncrypt.Plugin(rot)), rot); !errors.Is(err, kilncrypt.ErrDecrypt) {
			t.Fatalf("Rotate re-sealed a copied value: %v", err)
		}
		_, err = db.Exec(ctx, "UPDATE patients SET diagnosis = NULL WHERE id = ?", ann.ID)
		must(t, err)
		// Scanned with raw SQL: Open can't know the column, OpenColumn can.
		var rawEmail []kilncrypt.String
		must(t, db.Query(ctx, &rawEmail, "SELECT email FROM patients WHERE id = ?", ann.ID))
		if _, err := rawEmail[0].Open(keys); !errors.Is(err, kilncrypt.ErrUnbound) {
			t.Fatalf("raw Open: %v", err)
		}
		if v, err := rawEmail[0].OpenColumn(keys, "patients", "email"); err != nil || v != "ann@new.io" {
			t.Fatalf("OpenColumn = %q, %v", v, err)
		}
		// SealColumn for raw SQL writes.
		sc, err := keys.SealColumn("raw flu", "patients", "diagnosis")
		must(t, err)
		_, err = db.Exec(ctx, "UPDATE patients SET diagnosis = ? WHERE name = ?", sc, "bob")
		must(t, err)
		bob, err := patients.Find(ctx, q.Eq("name", "bob"))
		must(t, err)
		if d, err := bob.Diagnosis.Open(keys); err != nil || d != "raw flu" {
			t.Fatalf("SealColumn value: %q %v", d, err)
		}

		// v0.1 values (kc1, not bound to a column) are still read.
		_, err = db.Exec(ctx, "UPDATE patients SET diagnosis = ? WHERE name = ?", sealV1(t, 1, 'a', "old flu"), "bob")
		must(t, err)
		bob, err = patients.Find(ctx, q.Eq("name", "bob"))
		must(t, err)
		if d, err := bob.Diagnosis.Open(keys); err != nil || d != "old flu" {
			t.Fatalf("v0.1 value: %q %v", d, err)
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
		// Rotate rewrote the v0.1 value as kc2: RejectUnbound can be set.
		var bobDiag string
		must(t, db.Query(ctx, &bobDiag, "SELECT diagnosis FROM patients WHERE name = ?", "bob"))
		if !strings.HasPrefix(bobDiag, "kc2:") {
			t.Fatalf("v0.1 value not upgraded: %.8s", bobDiag)
		}
		if n, err := kilncrypt.Rotate[Patient](ctx, rdb, newKeys); err != nil || n != 0 {
			t.Fatalf("second Rotate: %d %v", n, err)
		}
		onlyNew, err := kilncrypt.NewKeys(indexKey, cryptKey(2, 'b'))
		must(t, err)
		onlyNew.RejectUnbound()
		bob, err = orm.For[Patient](rdb).Find(ctx, q.Eq("name", "bob"))
		must(t, err)
		if d, err := bob.Diagnosis.Open(onlyNew); err != nil || d != "old flu" {
			t.Fatalf("upgraded v0.1 value with RejectUnbound: %q %v", d, err)
		}
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

		// Setting NULL clears the index too.
		got.Email = kilncrypt.String{}
		must(t, orm.For[Patient](rdb).Update(ctx, &got, "Email", "EmailIndex"))
		must(t, db.Query(ctx, &raw, "SELECT email, email_index, diagnosis FROM patients ORDER BY id"))
		if raw[0].Email != nil || raw[0].Index != nil {
			t.Fatalf("NULL left %+v", raw[0])
		}

		// Rotate pages through more than one page (500 per page).
		var many []Patient
		for i := 0; i < 620; i++ {
			many = append(many, Patient{Name: "bulk", Diagnosis: kilncrypt.NewString("d")})
		}
		for i := 0; i < len(many); i += 200 { // stay under SQL Server's 2100 parameters
			must(t, patients.InsertAll(ctx, many[i:min(i+200, len(many))])) // old key 1
		}
		if n, err := kilncrypt.Rotate[Patient](ctx, rdb, newKeys); err != nil || n != 620 {
			t.Fatalf("Rotate over pages: %d %v", n, err)
		}
		var stored []string
		must(t, db.Query(ctx, &stored, "SELECT diagnosis FROM patients WHERE diagnosis IS NOT NULL"))
		stale := 0
		for _, v := range stored {
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(v, "kc2:"))
			if err != nil || binary.BigEndian.Uint32(raw) != 2 {
				stale++
			}
		}
		if len(stored) < 620 || stale != 0 {
			t.Fatalf("%d values still under the old key", stale)
		}

		// With audit, the log holds ciphertext, never the plaintext.
		t.Run("audit", func(t *testing.T) {
			adb := orm.Open(db.SQL(), db.Dialect(), kilncrypt.Plugin(newKeys, email2), audit.Plugin())
			ap := orm.For[Patient](adb)
			rec := Patient{Name: "dee", Email: kilncrypt.NewString("dee@secret.io"), Diagnosis: kilncrypt.NewString("measles")}
			must(t, ap.Insert(ctx, &rec))
			rec.Diagnosis = kilncrypt.NewString("mumps")
			must(t, ap.Update(ctx, &rec))
			loaded, err := ap.FindByID(ctx, rec.ID)
			must(t, err)
			loaded.Name = "dee b"
			must(t, ap.Update(ctx, &loaded)) // encrypted fields untouched: not in the diff
			var logs []string
			must(t, db.Query(ctx, &logs, "SELECT changes FROM audit_log ORDER BY id"))
			if len(logs) != 3 {
				t.Fatalf("%d audit rows", len(logs))
			}
			for _, l := range logs {
				if strings.Contains(l, "dee@secret") || strings.Contains(l, "measles") || strings.Contains(l, "mumps") {
					t.Fatalf("plaintext in audit log: %s", l)
				}
			}
			if !strings.Contains(logs[1], `"diagnosis":["kc2:`) || strings.Contains(logs[2], "diagnosis") {
				t.Fatalf("diffs: %s / %s", logs[1], logs[2])
			}
		})

		// Without the plugin, plaintext is never written.
		plain := orm.Open(db.SQL(), db.Dialect())
		if err := orm.For[Patient](plain).Insert(ctx, &Patient{Name: "x", Email: kilncrypt.NewString("x")}); !errors.Is(err, kilncrypt.ErrNotSealed) {
			t.Fatalf("insert without plugin: %v", err)
		}
	})
}

// sealV1 writes a value exactly as kilncrypt v0.1 stored it.
func sealV1(t *testing.T, id uint32, secret byte, plain string) string {
	t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{secret}, kilncrypt.KeySize))
	must(t, err)
	aead, err := cipher.NewGCM(block)
	must(t, err)
	buf := make([]byte, 4+aead.NonceSize())
	binary.BigEndian.PutUint32(buf, id)
	_, err = rand.Read(buf[4:])
	must(t, err)
	out := aead.Seal(buf, buf[4:], []byte(plain), append([]byte("kc1:"), buf[:4]...))
	return "kc1:" + base64.RawURLEncoding.EncodeToString(out)
}
