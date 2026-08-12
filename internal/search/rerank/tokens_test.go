package rerank

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"ParseHTTPHeader", []string{"parse", "http", "header"}},
		{"validate_user_token", []string{"validate", "user", "token"}},
		{"HTTPHeader", []string{"http", "header"}},
		{"простой текст", []string{"простой", "текст"}},
		// A word ending in consecutive uppercase runes where the last
		// one is multi-byte used to panic with "index out of range [1]
		// with length 1": the lookahead guard compared byte offsets, so
		// []rune(s[i:]) could hold a single rune.
		{"ТЕКСТ", []string{"текст"}},
		{"ПРОСТОЙ ТЕКСТ", []string{"простой", "текст"}},
		{"CAFÉ", []string{"café"}},
		// SCREAMING → Camel split still works across a multi-byte run.
		{"ЖКХStatus", []string{"жкх", "status"}},
	}
	for _, tc := range cases {
		if got := tokenize(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
