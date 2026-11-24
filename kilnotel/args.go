package kilnotel

import (
	"fmt"
	"strconv"
)

func itoa(i int) string { return strconv.Itoa(i) }

func argString(a any) string {
	switch v := a.(type) {
	case nil:
		return "NULL"
	case string:
		return v
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(v))
	default:
		return fmt.Sprint(v)
	}
}
