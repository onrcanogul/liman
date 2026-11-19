package textdiff

import "testing"

func TestLines(t *testing.T) {
	tests := []struct {
		name    string
		old     string
		updated string
		context int
		want    string
	}{
		{name: "identical", old: "a\nb\n", updated: "a\nb\n", context: -1, want: ""},
		{name: "new file", old: "", updated: "a\nb\n", context: -1, want: "+ a\n+ b\n"},
		{name: "removed file", old: "a\nb\n", updated: "", context: -1, want: "- a\n- b\n"},
		{
			name:    "one line changed",
			old:     "upstream x {\n    server 127.0.0.1:1;\n}\n",
			updated: "upstream x {\n    server 127.0.0.1:2;\n}\n",
			context: -1,
			want:    "  upstream x {\n-     server 127.0.0.1:1;\n+     server 127.0.0.1:2;\n  }\n",
		},
		{name: "line added in the middle", old: "a\nc\n", updated: "a\nb\nc\n", context: -1, want: "  a\n+ b\n  c\n"},
		{name: "line removed at the end", old: "a\nb\n", updated: "a\n", context: -1, want: "  a\n- b\n"},
		{
			name:    "context hides distant lines",
			old:     "1\n2\n3\n4\n5\n6\n7\n8\n9\n",
			updated: "1\n2\n3\n4\nX\n6\n7\n8\n9\n",
			context: 1,
			want:    "  ...\n  4\n- 5\n+ X\n  6\n  ...\n",
		},
		{
			name:    "zero context shows only changes",
			old:     "1\n2\n3\n",
			updated: "1\nX\n3\n",
			context: 0,
			want:    "  ...\n- 2\n+ X\n  ...\n",
		},
		{
			name:    "two nearby changes share their context",
			old:     "1\n2\n3\n4\n5\n",
			updated: "A\n2\n3\n4\nB\n",
			context: 1,
			want:    "- 1\n+ A\n  2\n  ...\n  4\n- 5\n+ B\n",
		},
		{
			name:    "context larger than the file shows everything",
			old:     "1\n2\n",
			updated: "1\nX\n",
			context: 10,
			want:    "  1\n- 2\n+ X\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Lines([]byte(tt.old), []byte(tt.updated), tt.context); got != tt.want {
				t.Errorf("Lines() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
