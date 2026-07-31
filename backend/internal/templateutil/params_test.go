package templateutil

import "testing"

func TestExpectedBodyParamCount(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{name: "empty", raw: `[]`, want: 0},
		{name: "body without placeholders", raw: `[{"type":"BODY","text":"Olá"}]`, want: 0},
		{name: "body with two", raw: `[{"type":"BODY","text":"Oi {{1}}, às {{2}}"}]`, want: 2},
		{name: "gap uses max index", raw: `[{"type":"BODY","text":"{{1}} e {{3}}"}]`, want: 3},
		{name: "ignores header", raw: `[{"type":"HEADER","text":"{{1}}"},{"type":"BODY","text":"fixo {{1}}"}]`, want: 1},
		{name: "invalid json", raw: `{`, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpectedBodyParamCount([]byte(tc.raw))
			if got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
