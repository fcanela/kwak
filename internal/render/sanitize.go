// Package render turns other agents' messages into the text a model reads (spec 6.1, I8). It is
// pure: no I/O.
package render

import (
	"strings"
	"unicode"
)

// Sanitize unifies line breaks, removes ANSI escapes, turns tabs into 4 spaces, and drops every
// rune that is a control other than "\n", in Cf, Co or Cs, a noncharacter or a variation selector.
// It never escapes tags; Delivery does that for message bodies.
func Sanitize(s string) string {
	s = unifyBreaks(s)
	s = stripANSI(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case dropped(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unifyBreaks(s string) string {
	if !strings.ContainsAny(s, "\r\u0085\u2028\u2029") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			b.WriteByte('\n')
		case '\u0085', '\u2028', '\u2029':
			b.WriteByte('\n')
		default:
			b.WriteRune(rs[i])
		}
	}
	return b.String()
}

// stripANSI removes CSI sequences (ESC [ ... final), terminated OSC sequences (ESC ] ... BEL or
// ST) and two-character escapes, in their 7-bit and 8-bit forms. Stray controls are dropped later.
func stripANSI(s string) string {
	if !strings.ContainsAny(s, "\x1b\u009b\u009d") {
		return s
	}
	rs := []rune(s)
	n := len(rs)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < n; i++ {
		r := rs[i]
		esc := r == 0x1b && i+1 < n
		switch {
		case r == 0x9b || esc && rs[i+1] == '[':
			j := i + 1
			if r == 0x1b {
				j++
			}
			for j < n && rs[j] >= 0x30 && rs[j] <= 0x3f {
				j++
			}
			for j < n && rs[j] >= 0x20 && rs[j] <= 0x2f {
				j++
			}
			if j < n && rs[j] >= 0x40 && rs[j] <= 0x7e {
				j++
			}
			i = j - 1
		case r == 0x9d || esc && rs[i+1] == ']':
			j := i + 1
			if r == 0x1b {
				j++
			}
			end := -1
			for ; j < n; j++ {
				if rs[j] == 0x07 || rs[j] == 0x9c {
					end = j
					break
				}
				if rs[j] == 0x1b && j+1 < n && rs[j+1] == '\\' {
					end = j + 1
					break
				}
			}
			if end >= 0 {
				i = end
			} else if r == 0x1b {
				i++
			}
		case esc && rs[i+1] >= 0x40 && rs[i+1] <= 0x7e:
			i++
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// dropped reports whether Sanitize removes r. "\n" and "\t" are handled before.
func dropped(r rune) bool {
	switch {
	case unicode.IsControl(r):
		return true
	case unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp):
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF:
		return true
	case r >= 0xE0000 && r <= 0xE007F:
		return true
	case noncharacter(r):
		return true
	}
	return false
}

func noncharacter(r rune) bool {
	return (r >= 0xFDD0 && r <= 0xFDEF) || (r&0xFFFE == 0xFFFE && r <= unicode.MaxRune)
}
