package internal

import "testing"

func TestExtractTokenFromInput(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"裸JWT", "eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.abc.def", "eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.abc.def"},
		{"token=前缀", "token=eyJabc.def.ghi", "eyJabc.def.ghi"},
		{"email----password----token", "mdonovan127@panlix.cloud----Aase4L6MdLg9!----eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.body.sig", "eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.body.sig"},
		{"多段----取最后JWT", "a----b----c----eyJxxx.yyy.zzz", "eyJxxx.yyy.zzz"},
		{"含前后空格", "  eyJtest.payload.sig  ", "eyJtest.payload.sig"},
		{"----分隔但最后不是JWT", "a----b----notjwt", "a----b----notjwt"},
		{"空字符串", "", ""},
		{"只有空格", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTokenFromInput(c.input)
			if got != c.want {
				t.Fatalf("input=%q\ngot =%q\nwant=%q", c.input, got, c.want)
			}
		})
	}
}
