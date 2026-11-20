package cli

import "io"

// safeWriter removes terminal control characters from everything written
// through it.
//
// Much of what the CLI prints was not written by liman: container logs,
// error messages from a registry, the text of a failed health check. A
// terminal obeys escape sequences in whatever it is shown, so such text
// could repaint earlier lines (hide a failure, fake a success), change the
// window title, or on some terminals put text on the clipboard. Line breaks
// and tabs are kept; every other control character is dropped, including
// carriage return, which exists to overwrite the current line.
type safeWriter struct {
	w io.Writer
	// pendingC2 remembers a 0xC2 byte at the end of the previous write. In
	// UTF-8 the control characters U+0080 to U+009F are 0xC2 followed by
	// 0x80 to 0x9F, and the two bytes may arrive in separate writes.
	pendingC2 bool
}

func (s *safeWriter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p)+1)
	for _, b := range p {
		if s.pendingC2 {
			s.pendingC2 = false
			if b >= 0x80 && b <= 0x9f {
				continue // a C1 control character: drop both bytes
			}
			out = append(out, 0xc2)
		}
		switch {
		case b == 0xc2:
			s.pendingC2 = true
		case b == '\n' || b == '\t' || (b >= 0x20 && b != 0x7f):
			out = append(out, b)
		}
	}
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	// The caller's byte count, as io.Writer requires, not the filtered one.
	return len(p), nil
}

// safe wraps w unless it is wrapped already.
func safe(w io.Writer) io.Writer {
	if _, ok := w.(*safeWriter); ok || w == nil {
		return w
	}
	return &safeWriter{w: w}
}
