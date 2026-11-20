package cli

import (
	"bytes"
	"testing"
)

func TestSafeWriterDropsControlCharactersAndKeepsText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain text", "pulling image\n\tdone\n", "pulling image\n\tdone\n"},
		{"non-ASCII text", "çalışıyor ✓ Größe\n", "çalışıyor ✓ Größe\n"},
		{"colour and cursor movement", "ok\x1b[2K\x1b[1A\x1b[31mFAILED\x1b[0m\n", "ok[2K[1A[31mFAILED[0m\n"},
		{"window title and clipboard", "\x1b]0;root@prod\x07\x1b]52;c;ZXZpbA==\x07x", "]0;root@prod]52;c;ZXZpbA==x"},
		{"carriage return overwrites a line", "deploy failed\rdeploy ok    \n", "deploy faileddeploy ok    \n"},
		{"backspace, bell, NUL, DEL", "a\x08b\x07c\x00d\x7fe", "abcde"},
		{"C1 controls in UTF-8", "a\u009b31mb\u0085c", "a31mbc"},
		{"0xC2 that starts a real character", "© 2026 é", "© 2026 é"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := safe(&buf)
			// One byte per write: sequences must not survive by being split
			// across writes, and characters must not be damaged by it.
			for i := 0; i < len(tt.in); i++ {
				if n, err := w.Write([]byte{tt.in[i]}); n != 1 || err != nil {
					t.Fatalf("Write() = %d, %v", n, err)
				}
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("one byte at a time: got %q, want %q", got, tt.want)
			}

			buf.Reset()
			w = safe(&buf)
			if n, err := w.Write([]byte(tt.in)); n != len(tt.in) || err != nil {
				t.Fatalf("Write() = %d, %v; want %d", n, err, len(tt.in))
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("in one write: got %q, want %q", got, tt.want)
			}
		})
	}
}
