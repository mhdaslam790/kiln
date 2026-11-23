package q

import (
	"reflect"
	"testing"
)

func TestColMatchesStringAPI(t *testing.T) {
	age, name := Col[int]("age"), Col[string]("name")
	cases := []struct {
		name      string
		got, want any
	}{
		{"Eq", age.Eq(3), Eq("age", 3)},
		{"Ne", age.Ne(3), Ne("age", 3)},
		{"Lt", age.Lt(3), Lt("age", 3)},
		{"Lte", age.Lte(3), Lte("age", 3)},
		{"Gt", age.Gt(3), Gt("age", 3)},
		{"Gte", age.Gte(3), Gte("age", 3)},
		{"Between", age.Between(1, 9), Between("age", 1, 9)},
		{"In", age.In(1, 2), In("age", 1, 2)},
		{"NotIn", age.NotIn(1, 2), NotIn("age", 1, 2)},
		{"Like", name.Like("a%"), Like("name", "a%")},
		{"NotLike", name.NotLike("a%"), NotLike("name", "a%")},
		{"IsNull", name.IsNull(), Null("name")},
		{"NotNull", name.NotNull(), NotNull("name")},
		{"EqCol", age.EqCol(Col[int]("min_age")), Eq("age", F("min_age"))},
		{"Set", name.Set("x"), Set("name", "x")},
		{"SetNull", name.SetNull(), Set("name", nil)},
		{"Inc", age.Inc(2), Inc("age", 2)},
		{"Of", age.Of("u").Eq(1), Eq("u.age", 1)},
		{"Names", Names(age, name), []string{"age", "name"}},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, c.got, c.want)
		}
	}
	var a, b Query
	age.Desc().Build(&a)
	Sort("-age").Build(&b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("Desc: %#v != %#v", a, b)
	}
}
