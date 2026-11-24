// Package terminal formats untrusted text for display.
package terminal

import (
	"strconv"
	"strings"
	"unicode"
)

func Field(text string) string {
	var clean strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			quoted := strconv.QuoteRune(r)
			clean.WriteString(quoted[1 : len(quoted)-1])
		} else {
			clean.WriteRune(r)
		}
	}
	return clean.String()
}
