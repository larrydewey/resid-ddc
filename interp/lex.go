package interp

// The lexer follows the Resid compiler's own token rules (compiler/driver.resid
// lex_tok): positions are codepoint indices into the source, and a token is
// re-lexed from any position on demand, so the parser can look ahead freely.

type Tok struct {
	Pos  int // index just past the token
	Text string
	Kind string // ident, int, float, dec, str, raw, bytes, char, fstring, op, eof
}

type Src struct {
	R    []rune
	Path string
}

func (s *Src) at(i int) rune {
	if i < 0 || i >= len(s.R) {
		return 0
	}
	return s.R[i]
}

func isWS(c rune) bool    { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
func isAlpha(c rune) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c >= 128 }
func isDigit(c rune) bool { return c >= '0' && c <= '9' }
func isAlnum(c rune) bool { return isAlpha(c) || isDigit(c) || c == '_' }

func (s *Src) skipAll(i int) int {
	n := len(s.R)
	for {
		for i < n && isWS(s.R[i]) {
			i++
		}
		if i >= n || s.R[i] != '/' {
			return i
		}
		c2 := s.at(i + 1)
		if c2 == '/' {
			i += 2
			for i < n && s.R[i] != '\n' {
				i++
			}
			continue
		}
		if c2 == '*' {
			i += 2
			for i < n && !(s.R[i] == '*' && s.at(i+1) == '/') {
				i++
			}
			if i < n {
				i += 2
			}
			continue
		}
		return i
	}
}

func (s *Src) scanString(i int) int {
	n := len(s.R)
	for i < n {
		c := s.R[i]
		if c == '"' {
			return i + 1
		}
		if c == '\\' {
			i += 2
			continue
		}
		i++
	}
	return i
}

func (s *Src) scanFString(i int) int {
	n := len(s.R)
	depth := 0
	for i < n {
		c := s.R[i]
		switch {
		case c == '\\':
			i += 2
			continue
		case c == '{':
			depth++
		case c == '}':
			depth = 0
		case c == '"' && depth == 0:
			return i + 1
		}
		i++
	}
	return i
}

func (s *Src) scanDigits(i, base int) int {
	n := len(s.R)
	for i < n {
		c := s.R[i]
		ok := false
		switch base {
		case 2:
			ok = c == '0' || c == '1'
		case 8:
			ok = c >= '0' && c <= '7'
		case 10:
			ok = isDigit(c)
		case 16:
			ok = isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		}
		if !ok {
			return i
		}
		i++
	}
	return i
}

func (s *Src) slice(a, b int) string {
	if a < 0 {
		a = 0
	}
	if b > len(s.R) {
		b = len(s.R)
	}
	if a >= b {
		return ""
	}
	return string(s.R[a:b])
}

var ops3 = []string{"..="}
var ops2 = []string{"..", "<<", "<=", ">>", ">=", "==", "=>", "!=", "&&", "||"}

func (s *Src) Lex(pos int) Tok {
	n := len(s.R)
	j := s.skipAll(pos)
	if j >= n {
		return Tok{Pos: j, Kind: "eof"}
	}
	c, c2 := s.R[j], s.at(j+1)
	switch {
	case c == 'f' && c2 == '"':
		e := s.scanFString(j + 2)
		return Tok{e, s.slice(j, e), "fstring"}
	case c == 'r' && c2 == '"':
		e := s.scanString(j + 2)
		return Tok{e, s.slice(j, e), "raw"}
	case c == 'b' && c2 == '"':
		e := s.scanString(j + 2)
		return Tok{e, s.slice(j, e), "bytes"}
	case c == '"':
		e := s.scanString(j + 1)
		return Tok{e, s.slice(j, e), "str"}
	case c == '\'':
		k := j + 3
		if c2 == '\\' {
			k = j + 4
		}
		return Tok{k, s.slice(j, k), "char"}
	case isDigit(c):
		if c == '0' && (c2 == 'x' || c2 == 'b' || c2 == 'o') {
			base := map[rune]int{'x': 16, 'b': 2, 'o': 8}[c2]
			e := s.scanDigits(j+2, base)
			return Tok{e, s.slice(j, e), "int"}
		}
		e := s.scanDigits(j, 10)
		if s.at(e) == '.' && s.at(e+1) != '.' {
			ef := s.scanDigits(e+1, 10)
			if s.at(ef) == 'm' && !isAlnum(s.at(ef+1)) {
				return Tok{ef + 1, s.slice(j, ef+1), "dec"}
			}
			return Tok{ef, s.slice(j, ef), "float"}
		}
		if s.at(e) == 'm' && !isAlnum(s.at(e+1)) {
			return Tok{e + 1, s.slice(j, e+1), "dec"}
		}
		return Tok{e, s.slice(j, e), "int"}
	case isAlpha(c) || c == '_':
		e := j
		for e < n && isAlnum(s.R[e]) {
			e++
		}
		return Tok{e, s.slice(j, e), "ident"}
	case c == '#':
		return Tok{j + 8, "#location", "op"}
	case c == '@':
		return Tok{j + 1, "@", "op"}
	}
	for _, o := range ops3 {
		if s.slice(j, j+3) == o {
			return Tok{j + 3, o, "op"}
		}
	}
	for _, o := range ops2 {
		if s.slice(j, j+2) == o {
			return Tok{j + 2, o, "op"}
		}
	}
	if string(c) != "" && containsRune(".<>=!&|+-*/%~^?:,;(){}[]", c) {
		return Tok{j + 1, string(c), "op"}
	}
	return Tok{j + 1, "", "op"}
}

func containsRune(set string, c rune) bool {
	for _, r := range set {
		if r == c {
			return true
		}
	}
	return false
}

// Start of the token ending at t.Pos.
func tstart(t Tok) int { return t.Pos - len([]rune(t.Text)) }
