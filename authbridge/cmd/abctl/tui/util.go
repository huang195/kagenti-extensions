package tui

import (
	"fmt"
	"strings"
)

// formatCount renders an integer with thousands separators so large
// numbers stay readable in narrow table columns. Handles negatives by
// preserving the sign; zero and single-triplet values are passed through
// unchanged.
func formatCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// nonEmpty returns s if non-empty, otherwise fallback. Used by label
// renderers where we want an em-dash sentinel in place of empty strings.
func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
