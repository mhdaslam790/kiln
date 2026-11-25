package meta

import (
	"reflect"
	"testing"
	"time"
)

type Base struct {
	ID        int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Author struct {
	Base
	Name      string
	Email     string `db:"email_address"`
	Secret    string `db:"-"`
	DeletedAt *time.Time
	Posts     []Post
	Profile   *Profile
}

type Post struct {
	ID       int
	AuthorID int64
	Author   Author
	Title    string
	Meta     map[string]any `db:"meta,json"`
	Tags     []string
}

type Profile struct {
	ID       uint
	AuthorID int64
	Bio      string
}

type Category struct {
	Code string `db:"code,primary"`
}

func (Category) TableName() string { return "cats" }

func TestModel(t *testing.T) {
	m := MustOf(reflect.TypeOf(Author{}))
	if m.Table != "authors" {
		t.Errorf("table = %s", m.Table)
	}
	if m.PK == nil || m.PK.Column != "id" || !m.PK.Auto {
		t.Fatalf("pk = %+v", m.PK)
	}
	if m.CreatedAt == nil || m.UpdatedAt == nil || m.DeletedAt == nil {
		t.Error("timestamps not detected")
	}
	if m.Field("email_address") == nil || m.Field("Secret") != nil {
		t.Error("tag handling")
	}
	posts, err := m.Assoc("Posts")
	if err != nil {
		t.Fatal(err)
	}
	if posts.Kind != HasMany || posts.OwnerCol != "id" || posts.TargetCol != "author_id" {
		t.Errorf("posts assoc = %+v", posts)
	}
	prof, err := m.Assoc("profile")
	if err != nil {
		t.Fatal(err)
	}
	if prof.Kind != HasOne || !prof.Ptr {
		t.Errorf("profile assoc = %+v", prof)
	}

	pm := MustOf(reflect.TypeOf(Post{}))
	au, err := pm.Assoc("Author")
	if err != nil {
		t.Fatal(err)
	}
	if au.Kind != BelongsTo || au.OwnerCol != "author_id" || au.TargetCol != "id" {
		t.Errorf("author assoc = %+v", au)
	}
	if f := pm.Field("meta"); f == nil || !f.JSON {
		t.Error("json field")
	}
	if pm.Field("tags") == nil {
		t.Error("slice of scalars should be a column")
	}

	cm := MustOf(reflect.TypeOf(Category{}))
	if cm.Table != "cats" || cm.PK.Column != "code" || cm.PK.Auto {
		t.Errorf("category = %+v %+v", cm.Table, cm.PK)
	}
}

func TestNames(t *testing.T) {
	for in, out := range map[string]string{"UserID": "user_id", "HTTPServer": "http_server", "ID": "id", "Name2Go": "name2_go", "createdAt": "created_at"} {
		if got := Snake(in); got != out {
			t.Errorf("Snake(%s) = %s, want %s", in, got, out)
		}
	}
	for in, out := range map[string]string{"user": "users", "category": "categories", "box": "boxes", "day": "days", "person": "people", "blog_post": "blog_posts", "status": "statuses"} {
		if got := Plural(in); got != out {
			t.Errorf("Plural(%s) = %s, want %s", in, got, out)
		}
	}
	for _, w := range []string{"user", "category", "box", "day", "person", "blog_post", "status", "address", "church"} {
		if got := Singular(Plural(w)); got != w {
			t.Errorf("Singular(Plural(%s)) = %s", w, got)
		}
	}
	if Camel("user_id") != "UserID" || Camel("avatar_url") != "AvatarURL" {
		t.Error("Camel")
	}
}

type Tag struct {
	ID   int64
	Name string
}

type Article struct {
	ID      int64
	Tags    []Tag      `m2m:"article_tags"`
	Related []*Article `m2m:"related_articles,article_id,related_id"`
	Bad     []Article  `m2m:"self_join"` // self-reference without explicit columns
}

func TestManyToMany(t *testing.T) {
	m := MustOf(reflect.TypeOf(Article{}))
	a, err := m.Assoc("Tags")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ManyToMany || a.JoinTable != "article_tags" || a.JoinOwnerCol != "article_id" ||
		a.JoinTargetCol != "tag_id" || a.OwnerCol != "id" || a.TargetCol != "id" {
		t.Errorf("tags = %+v", a)
	}
	r, err := m.Assoc("Related")
	if err != nil || r.JoinOwnerCol != "article_id" || r.JoinTargetCol != "related_id" || !r.Ptr {
		t.Errorf("related = %+v, %v", r, err)
	}
	if _, err := m.Assoc("Bad"); err == nil {
		t.Error("self-referencing m2m without join columns should fail")
	}
}

func TestLockField(t *testing.T) {
	type Doc struct {
		ID      int64
		Version uint32 `db:"version,lock"`
	}
	m, err := Of(reflect.TypeOf(Doc{}))
	if err != nil || m.Version == nil || m.Version.Column != "version" {
		t.Fatalf("lock field: %v %+v", err, m)
	}
	type Plain struct{ ID int64 }
	if m, _ := Of(reflect.TypeOf(Plain{})); m.Version != nil {
		t.Fatal("no lock tag must mean no version field")
	}

	type StringLock struct {
		ID int64
		V  string `db:"v,lock"`
	}
	type TwoLocks struct {
		ID int64
		A  int `db:"a,lock"`
		B  int `db:"b,lock"`
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(StringLock{}), reflect.TypeOf(TwoLocks{})} {
		if _, err := Of(typ); err == nil {
			t.Errorf("%s: want an error", typ.Name())
		}
	}
}

func TestCamelValidIdentifiers(t *testing.T) {
	for in, want := range map[string]string{
		"user_id": "UserID", "html_body": "HTMLBody", "3d_model": "X3dModel", "my-col": "MyCol",
		"my col": "MyCol", "ünïcode_name": "ÜnïcodeName", "_x": "X", "x__y": "XY", "$amt": "Amt",
		"1": "X1", "": "X", "a.b": "AB", "post_tags": "PostTags",
	} {
		if got := Camel(in); got != want {
			t.Errorf("Camel(%q) = %q, want %q", in, got, want)
		}
	}
}
