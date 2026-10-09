package interp

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The text-formatting primitives of an f-string hole's spec (spec §14),
// written from their definition in runtime/rt/fmt.resid: a spec is one
// packed word, and each primitive is an operation on text.

const (
	fmGroup      = 1
	fmPlus       = 2
	fmSpace      = 4
	fmUpper      = 8
	fmLower      = 16
	fmDecimal    = 32
	fmScientific = 64
	fmPercent    = 128

	fmLeft   = 0
	fmRight  = 1
	fmCenter = 2
	fmSign   = 3
)

func fmWidth(s int64) int   { return int(uint64(s) & 255) }
func fmAlign(s int64) int   { return int((uint64(s) >> 8) & 15) }
func fmBase(s int64) int    { return int((uint64(s) >> 12) & 31) }
func fmFlags(s int64) int64 { return int64((uint64(s) >> 17) & 262143) }
func fmFill(s int64) rune   { return rune((uint64(s) >> 43) & 2097151) }
func fmPrec(s int64) int {
	p := int((uint64(s) >> 35) & 255)
	if p == 0 {
		return -1
	}
	return p - 1
}

func fmtGroup(s string) string {
	st := 0
	if s != "" && (s[0] == '-' || s[0] == '+') {
		st = 1
	}
	nd := 0
	for i := st; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		nd++
	}
	if nd < 4 {
		return s
	}
	var b strings.Builder
	b.WriteString(s[:st])
	left := nd
	for i := st; i < len(s); i++ {
		c := s[i]
		b.WriteByte(c)
		if c >= '0' && c <= '9' && left > 0 {
			left--
			if left > 0 && left%3 == 0 {
				b.WriteByte(',')
			}
		}
	}
	return b.String()
}

func fmtCase(s string, spec int64) string {
	f := fmFlags(spec)
	switch {
	case f&fmUpper != 0:
		return asciiMap(s, 'a', 'z', -32)
	case f&fmLower != 0:
		return asciiMap(s, 'A', 'Z', 32)
	}
	return s
}

func fmtSign(s string, spec int64) string {
	want := fmFlags(spec) & (fmPlus | fmSpace)
	if want == 0 || (s != "" && s[0] == '-') {
		return s
	}
	if want&fmPlus != 0 {
		return "+" + s
	}
	return " " + s
}

func fmtPad(s string, spec int64) string {
	w, al := fmWidth(spec), fmAlign(spec)
	fill := fmFill(spec)
	if fill == 0 {
		fill = ' '
	}
	n := utf8.RuneCountInString(s)
	pad := w - n
	if pad <= 0 {
		return s
	}
	lpad, rpad := pad, pad
	if al == fmLeft {
		lpad = 0
	}
	if al == fmRight || al == fmSign {
		rpad = 0
	}
	if al == fmCenter {
		lpad, rpad = pad/2, pad-pad/2
	}
	f := string(fill)
	if al == fmSign && n > 0 && (s[0] == '-' || s[0] == '+') {
		return s[:1] + strings.Repeat(f, lpad) + s[1:] + strings.Repeat(f, rpad)
	}
	return strings.Repeat(f, lpad) + s + strings.Repeat(f, rpad)
}

func fmtRadix(s string, spec int64) string {
	base := fmBase(spec)
	if base == 10 {
		return s
	}
	st := 0
	if s != "" && (s[0] == '-' || s[0] == '+') {
		st = 1
	}
	v, ok := new(big.Int).SetString(s[st:], 10)
	if !ok {
		return s
	}
	t := v.Text(base)
	if fmFlags(spec)&fmUpper != 0 {
		t = strings.ToUpper(t)
	}
	return s[:st] + t
}

func fmtStyle(v float64, spec int64) string {
	flags := fmFlags(spec)
	sci, pct, fix := flags&fmScientific != 0, flags&fmPercent != 0, flags&fmDecimal != 0
	prec := fmPrec(spec)
	var out string
	if prec < 0 && !fix && !sci {
		if pct {
			out = floatShort(v*100) + "%"
		} else {
			out = floatShort(v)
		}
	} else {
		w := v
		if pct {
			w = v * 100
		}
		p := prec
		if p < 0 {
			p = 0
		}
		if sci {
			out = cFormat(w, 'e', p)
		} else {
			out = cFormat(w, 'f', p)
		}
		if pct {
			out += "%"
		}
	}
	want := flags & (fmPlus | fmSpace)
	if want != 0 && !(out != "" && out[0] == '-') {
		if want&fmPlus != 0 {
			out = "+" + out
		} else {
			out = " " + out
		}
	}
	return out
}

// ─── C's float conversions ─────────────────────────────────────────────

func cSpecial(v float64) (string, bool) {
	switch {
	case math.IsNaN(v):
		if math.Signbit(v) {
			return "-nan", true
		}
		return "nan", true
	case math.IsInf(v, 1):
		return "inf", true
	case math.IsInf(v, -1):
		return "-inf", true
	}
	return "", false
}

// cFormat is printf's %.<p>f, %.<p>e or %.<p>g.
func cFormat(v float64, conv byte, p int) string {
	if s, ok := cSpecial(v); ok {
		return s
	}
	switch conv {
	case 'f':
		return strconv.FormatFloat(v, 'f', p, 64)
	case 'e':
		return strconv.FormatFloat(v, 'e', p, 64)
	}
	// %g: P significant digits (0 counts as 1); the exponent X of the
	// %e form picks the style, and trailing zeros go.
	if p == 0 {
		p = 1
	}
	e := strconv.FormatFloat(v, 'e', p-1, 64)
	k := strings.LastIndexByte(e, 'e')
	x, _ := strconv.Atoi(e[k+1:])
	if x < p && x >= -4 {
		return trimZeros(strconv.FormatFloat(v, 'f', p-1-x, 64))
	}
	return trimZeros(e[:k]) + e[k:]
}

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// floatShort: the shortest %.<P>g (P = 1..17) that reads back as v, with
// exponents 1..15 written positionally (runtime/rt/numfmt.resid).
func floatShort(v float64) string {
	if math.IsNaN(v) {
		return "nan"
	}
	var s string
	for p := 1; ; p++ {
		s = cFormat(v, 'g', p)
		back, _ := strconv.ParseFloat(s, 64)
		if p >= 17 || back == v {
			break
		}
	}
	x := gExponent(s)
	if x < 1 || x > 15 {
		return s
	}
	return cFormat(v, 'g', x+1)
}

func gExponent(s string) int {
	k := strings.Index(s, "e+")
	if k < 0 {
		return 0
	}
	x, _ := strconv.Atoi(s[k+2:])
	return x
}

// ─── Case mapping (str_to_upper / str_to_lower) ────────────────────────
// Unicode simple case mapping plus SpecialCasing's unconditional uppercase
// expansions, and Final_Sigma when lowering.

func isCased(r rune) bool { return unicode.ToLower(r) != r || unicode.ToUpper(r) != r }

func isIgnorable(r rune) bool {
	if isCased(r) {
		return false
	}
	return (r >= 768 && r <= 879) || (r >= 1155 && r <= 1161) || (r >= 1425 && r <= 1469) ||
		(r >= 1552 && r <= 1562) || (r >= 1611 && r <= 1631) || (r >= 3633 && r <= 3642) ||
		(r >= 8204 && r <= 8207) || (r >= 65024 && r <= 65039)
}

func toUpper(s string) string {
	var b strings.Builder
	for _, r := range s {
		if x, ok := specialUpper[r]; ok {
			b.WriteString(x)
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

func toLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if r == 931 {
			prev := false
			for j := i - 1; j >= 0; j-- {
				if !isIgnorable(rs[j]) {
					prev = isCased(rs[j])
					break
				}
			}
			next := false
			for j := i + 1; j < len(rs); j++ {
				if !isIgnorable(rs[j]) {
					next = isCased(rs[j])
					break
				}
			}
			if prev && !next {
				b.WriteRune(962)
			} else {
				b.WriteRune(963)
			}
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
