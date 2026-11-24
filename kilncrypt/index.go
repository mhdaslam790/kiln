package kilncrypt

import (
	"encoding/hex"
	"errors"

	"github.com/mhdaslam790/kiln/q"
)

// Index is a blind index: it stores an HMAC of an encrypted column's value
// in another column, so the encrypted column can be searched by exact
// match without decrypting it. Create it with Keys.Index and pass it to
// Plugin, which keeps the index column up to date on every write.
type Index struct {
	column, indexColumn string
	key                 []byte
	normalize           func(string) string
}

// Index returns the blind index of column (a String field) stored in
// indexColumn (a string or *string field, a text column; index it in the
// database for fast lookups). normalize, if not nil, is applied before
// hashing, e.g. strings.ToLower to make lookups case-insensitive. Changing
// it later needs a re-index: existing rows keep their old index values.
//
// Each index column gets its own key, derived from the index key, so equal
// values in two different columns don't have equal indexes.
func (k *Keys) Index(column, indexColumn string, normalize func(string) string) (*Index, error) {
	if k.index == nil {
		return nil, errors.New("kilncrypt: NewKeys was given no index key")
	}
	if column == "" || indexColumn == "" || column == indexColumn {
		return nil, errors.New("kilncrypt: Index needs two different columns")
	}
	return &Index{column: column, indexColumn: indexColumn, key: mac(k.index, "kilncrypt index "+indexColumn), normalize: normalize}, nil
}

// Of returns the index value of v, as stored in the index column.
func (i *Index) Of(v string) string {
	if i.normalize != nil {
		v = i.normalize(v)
	}
	return hex.EncodeToString(mac(i.key, v)[:16])
}

// Eq returns a filter matching rows whose column equals v.
func (i *Index) Eq(v string) q.Filter { return q.Eq(i.indexColumn, i.Of(v)) }

// In returns a filter matching rows whose column equals any of vs.
func (i *Index) In(vs ...string) q.Filter {
	out := make([]any, len(vs))
	for n, v := range vs {
		out[n] = i.Of(v)
	}
	return q.In(i.indexColumn, out...)
}
