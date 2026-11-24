package hrnexec

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/cristiangirlea/adanext-ada/internal/terminal"
)

const progressLimit = 4 << 10

type progressWriter struct {
	out     io.Writer
	secrets []progressSecret
	line    []byte
	afterCR bool
}

// Prefix lengths let a credential span any number of flushes without buffering it.
type progressSecret struct {
	value   string
	prefix  []int
	matched int
}

func newProgressWriter(out io.Writer, env []string) *progressWriter {
	w := &progressWriter{out: out, line: make([]byte, 0, progressLimit)}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if value != "" && (key == "HRN_TENANT_TOKEN" || key == "HRN_SERVER_URL") {
			s := progressSecret{value: value, prefix: make([]int, len(value))}
			for i := 1; i < len(value); i++ {
				s.prefix[i] = s.advance(s.prefix[i-1], value[i])
			}
			w.secrets = append(w.secrets, s)
		}
	}
	return w
}

func (s *progressSecret) advance(n int, b byte) int {
	for n > 0 && s.value[n] != b {
		n = s.prefix[n-1]
	}
	if s.value[n] == b {
		n++
	}
	return n
}

func (w *progressWriter) Write(p []byte) (int, error) {
	size := len(p)
	for len(p) > 0 {
		n := min(len(p), progressLimit-len(w.line))
		end := bytes.IndexAny(p[:n], "\r\n")
		if end >= 0 {
			n = end + 1
		}
		w.line = append(w.line, p[:n]...)
		p = p[n:]
		if end >= 0 || len(w.line) == progressLimit {
			if err := w.emit(end < 0); err != nil {
				return size - len(p), err
			}
		}
	}
	return size, nil
}

func (w *progressWriter) flush() error { return w.emit(false) }

func (w *progressWriter) emit(forced bool) error {
	data := w.line
	line := w.redact(data, forced)
	if forced && line != "[redacted]" {
		// Leave an incomplete rune for the next flush so terminal escaping sees it whole.
		last := len(data) - 1
		for last > 0 && len(data)-last < utf8.UTFMax && !utf8.RuneStart(data[last]) {
			last--
		}
		if !utf8.FullRune(data[last:]) {
			data = data[:last]
			line = line[:last]
		}
	}
	// A CRLF pair represents one update even when its bytes arrive in separate writes.
	afterCR := strings.HasSuffix(line, "\r")
	if w.afterCR {
		line = strings.TrimPrefix(line, "\n")
	}
	w.afterCR = afterCR
	if strings.HasSuffix(line, "\n") || afterCR {
		line = terminal.Field(line[:len(line)-1]) + "\n"
	} else {
		line = terminal.Field(line)
	}
	_, err := io.WriteString(w.out, line)
	w.line = append(w.line[:0], w.line[len(data):]...)
	return err
}

func (w *progressWriter) redact(data []byte, forced bool) string {
	var changes []int
	continued := false
	hide := func(start, end int) {
		if changes == nil {
			changes = make([]int, len(data)+1)
		}
		if start < 0 {
			continued = true
		}
		changes[max(start, 0)]++
		changes[end]--
	}
	for i := range w.secrets {
		s := &w.secrets[i]
		for j, b := range data {
			s.matched = s.advance(s.matched, b)
			if s.matched == len(s.value) {
				hide(j+1-s.matched, j+1)
				s.matched = s.prefix[s.matched-1]
			}
		}
		if s.matched > 0 && len(data) > 0 {
			hide(len(data)-s.matched, len(data))
		}
	}

	if changes == nil {
		return string(data)
	}
	if forced {
		return "[redacted]"
	}
	var text strings.Builder
	covered := 0
	for i, b := range data {
		previous := covered
		covered += changes[i]
		if covered == 0 {
			text.WriteByte(b)
		} else if previous == 0 && (i != 0 || !continued) {
			text.WriteString("[redacted]")
		}
	}
	return text.String()
}
