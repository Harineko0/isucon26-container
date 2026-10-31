package isureal

import "testing"

func TestEscapeLike(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "helena_santos_4199", want: `helena\_santos\_4199`},
		{name: "percent", in: "100%", want: `100\%`},
		{name: "backslash", in: `a\b`, want: `a\\b`},
		{name: "unicode", in: "小林 悠真", want: "小林 悠真"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := escapeLike(tt.in); got != tt.want {
				t.Errorf("escapeLike(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
