package schema

import (
	"reflect"
	"testing"
)

func TestReplayEnums(t *testing.T) {
	st, err := Replay(
		func(s *Schema) {
			s.Exec(`CREATE TYPE "kiln mood" AS ENUM ('happy', 'it''s ok')`)
			s.Exec("create type Status as enum ()")
			s.Exec(`ALTER TYPE "kiln mood" ADD VALUE IF NOT EXISTS 'sad'`)
			s.Exec(`ALTER TYPE "kiln mood" ADD VALUE IF NOT EXISTS 'sad'`) // no duplicate
			s.Exec("ALTER TYPE status ADD VALUE 'new';")
		},
		func(s *Schema) {
			s.Exec("CREATE TYPE gone AS ENUM ('x')")
			s.Exec(`DROP TYPE IF EXISTS "gone"`)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"kiln mood": {"happy", "it's ok", "sad"}, "status": {"new"}}
	if !reflect.DeepEqual(st.Enums, want) || st.Opaque != 0 {
		t.Errorf("enums = %#v, opaque %d", st.Enums, st.Opaque)
	}
	st, _ = Replay(func(s *Schema) {
		s.Exec("CREATE TYPE pair AS (a int, b int)")        // composite type
		s.Exec("ALTER TYPE other ADD VALUE 'x'")            // enum made elsewhere
		s.Exec("CREATE TYPE m AS ENUM ($1)", "x")           // has arguments
		s.Exec("CREATE TYPE m AS ENUM ('a'); DROP TABLE x") // more than one statement
		s.Exec("ALTER TYPE m ADD VALUE 'b' BEFORE 'a'")
	})
	if st.Opaque != 5 || len(st.Enums) != 0 {
		t.Errorf("unrecognised statements: enums %v, opaque %d", st.Enums, st.Opaque)
	}
}
