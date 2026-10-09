package interp

import (
	"fmt"
	"strconv"
	"strings"
)

// The parser follows the grammar of the Resid compiler's parser
// (compiler/graph.resid): precedence climbing over binary operators, a
// `?`/`else` postfix layer, binding heads `Type name =`, and casts
// recognised syntactically from builtin type names.

type ParseError struct {
	Path string
	Pos  int
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("%s:%d: %s", e.Path, e.Pos, e.Msg) }

type parser struct {
	s *Src
}

func (p *parser) fail(pos int, msg string, a ...any) {
	panic(&ParseError{p.s.Path, pos, fmt.Sprintf(msg, a...)})
}

func (p *parser) lex(pos int) Tok { return p.s.Lex(pos) }

func (p *parser) expect(pos int, text string) int {
	t := p.lex(pos)
	if t.Text != text {
		p.fail(t.Pos, "expected %s, found %q", text, t.Text)
	}
	return t.Pos
}

// ─── Types ─────────────────────────────────────────────────────────────

// parseType returns the type at pos and the position after it, or nil.
func (p *parser) parseType(pos int) (*Type, int) {
	t := p.lex(pos)
	if t.Kind != "ident" {
		return nil, pos
	}
	t2 := p.lex(t.Pos)
	if t2.Text == "(" {
		var args []*Type
		at := t2.Pos
		for {
			a := p.lex(at)
			var ty *Type
			var end int
			if a.Kind == "int" {
				n, _ := strconv.Atoi(a.Text)
				ty, end = &Type{IsLit: true, N: n}, a.Pos
			} else {
				ty, end = p.parseType(at)
				if ty == nil {
					return nil, pos
				}
			}
			args = append(args, ty)
			sep := p.lex(end)
			if sep.Text == ")" {
				at = sep.Pos
				break
			}
			if sep.Text != "," {
				return nil, pos
			}
			at = sep.Pos
		}
		ty := &Type{Name: t.Text, Args: args}
		if (t.Text == "Int" || t.Text == "UInt" || t.Text == "Float" || t.Text == "Dec") && len(args) == 1 && args[0].IsLit {
			ty.N = args[0].N
		}
		if nx := p.lex(at); nx.Text == "closure" {
			return p.closureTail(ty, nx.Pos)
		}
		return ty, at
	}
	if t2.Text == "closure" {
		return p.closureTail(&Type{Name: t.Text}, t2.Pos)
	}
	return &Type{Name: t.Text}, t.Pos
}

func (p *parser) closureTail(ret *Type, pos int) (*Type, int) {
	o := p.lex(pos)
	if o.Text != "(" {
		return nil, pos
	}
	ty := &Type{Name: "closure", Closure: true, Args: []*Type{ret}}
	at := o.Pos
	if e := p.lex(at); e.Text == ")" {
		return ty, e.Pos
	}
	for {
		a, end := p.parseType(at)
		if a == nil {
			return nil, pos
		}
		ty.Args = append(ty.Args, a)
		sep := p.lex(end)
		if sep.Text == ")" {
			return ty, sep.Pos
		}
		if sep.Text != "," {
			return nil, pos
		}
		at = sep.Pos
	}
}

func isKnownType(t *Type) bool {
	switch t.Name {
	case "Int", "ISize", "USize", "UInt", "Float", "Dec", "Bool", "Str", "Bytes", "List", "Vec":
		return !t.Closure
	}
	return false
}

// ─── Expressions ───────────────────────────────────────────────────────

func prec(op string) int {
	switch op {
	case "..", "..=":
		return 1
	case "||":
		return 3
	case "&&":
		return 4
	case "|":
		return 5
	case "^":
		return 6
	case "&":
		return 7
	case "==", "!=":
		return 8
	case "<", "<=", ">", ">=":
		return 9
	case "<<", ">>":
		return 10
	case "+", "-":
		return 11
	case "*", "/", "%":
		return 12
	}
	return 0
}

func (p *parser) expr(pos int) (Expr, int) {
	u, end := p.unary(pos)
	return p.bin(u, end, 1)
}

// tryTern: `? a : b` arms after `?`.
func (p *parser) tryTern(pos int) (a, b Expr, end int, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, is := r.(*ParseError); !is {
				panic(r)
			}
			ok = false
		}
	}()
	a, e1 := p.expr(pos)
	c := p.lex(e1)
	if c.Text != ":" {
		return nil, nil, 0, false
	}
	b, e2 := p.expr(c.Pos)
	return a, b, e2, true
}

func (p *parser) bin(lhs Expr, pos int, minPrec int) (Expr, int) {
	for {
		t := p.lex(pos)
		op := t.Text
		if op == "?" {
			a, b, end, ok := p.tryTern(t.Pos)
			if ok && minPrec > 1 {
				return lhs, pos
			}
			if ok {
				return &ETern{lhs, a, b}, end
			}
			lhs, pos = &ETry{lhs}, t.Pos
			continue
		}
		if op == "else" {
			o := p.lex(t.Pos)
			if o.Text != "{" {
				p.fail(o.Pos, "expected { after else")
			}
			d, end := p.bexpr(o.Pos)
			end = p.expect(end, "}")
			lhs, pos = &EOrElse{lhs, d}, end
			continue
		}
		pr := prec(op)
		if pr == 0 || pr < minPrec {
			return lhs, pos
		}
		after := p.lex(t.Pos)
		if op == ".." && after.Text == "]" {
			return &ERangeFrom{lhs}, t.Pos
		}
		rhs, rend := p.unary(t.Pos)
		next := pr + 1
		if op == ".." || op == "..=" {
			next = pr
		}
		rhs, rend = p.bin(rhs, rend, next)
		if op == ".." || op == "..=" {
			lhs = &ERange{lhs, rhs, op == "..="}
		} else {
			lhs = &EBin{op, lhs, rhs}
		}
		pos = rend
	}
}

func (p *parser) isRtPrefix(t Tok) bool {
	if t.Text != "rt" {
		return false
	}
	nx := p.lex(t.Pos)
	switch nx.Kind {
	case "ident", "int", "float", "dec", "str", "char", "raw", "bytes", "fstring":
		return true
	}
	return nx.Text == "(" || nx.Text == "[" || nx.Text == "!" || nx.Text == "-"
}

func (p *parser) castHead(pos int) (*Type, int, bool) {
	o := p.lex(pos)
	if o.Text != "(" {
		return nil, 0, false
	}
	ty, end := p.parseType(o.Pos)
	if ty == nil {
		return nil, 0, false
	}
	c := p.lex(end)
	if c.Text != ")" || !isKnownType(ty) {
		return nil, 0, false
	}
	return ty, c.Pos, true
}

func (p *parser) unary(pos int) (Expr, int) {
	t := p.lex(pos)
	if p.isRtPrefix(t) {
		return p.unary(t.Pos)
	}
	if ty, end, ok := p.castHead(pos); ok {
		x, e := p.unary(end)
		return &ECast{ty, x}, e
	}
	switch t.Text {
	case "-", "+", "~", "!":
		x, e := p.unary(t.Pos)
		return &EUn{t.Text, x}, e
	}
	x, e := p.primary(pos)
	return p.postfix(x, e)
}

func (p *parser) args(pos int, close string) ([]Expr, int) {
	var out []Expr
	t := p.lex(pos)
	if t.Text == close {
		return out, t.Pos
	}
	at := pos
	for {
		a := p.lex(at)
		eq := p.lex(a.Pos)
		var x Expr
		var end int
		if a.Kind == "ident" && eq.Text == "=" {
			v, e := p.expr(eq.Pos)
			x, end = &ENamed{a.Text, v}, e
		} else {
			x, end = p.expr(at)
		}
		out = append(out, x)
		sep := p.lex(end)
		if sep.Text == "," {
			at = sep.Pos
			if nx := p.lex(at); nx.Text == close {
				return out, nx.Pos
			}
			continue
		}
		if sep.Text == close {
			return out, sep.Pos
		}
		p.fail(sep.Pos, "expected , or %s, found %q", close, sep.Text)
	}
}

func (p *parser) postfix(base Expr, pos int) (Expr, int) {
	for {
		t := p.lex(pos)
		switch t.Text {
		case ".":
			f := p.lex(t.Pos)
			o := p.lex(f.Pos)
			if o.Text == "(" {
				as, e := p.args(o.Pos, ")")
				base, pos = &EMCall{base, f.Text, as, tstart(f)}, e
			} else {
				base, pos = &EField{base, f.Text}, f.Pos
			}
		case "[":
			t2 := p.lex(t.Pos)
			if t2.Text == ".." {
				t3 := p.lex(t2.Pos)
				if t3.Text == "]" {
					base, pos = &ESlice{X: base}, t3.Pos
					continue
				}
				hi, e := p.expr(t2.Pos)
				e = p.expect(e, "]")
				base, pos = &ESlice{X: base, Hi: hi}, e
				continue
			}
			ix, e := p.expr(t.Pos)
			e = p.expect(e, "]")
			switch r := ix.(type) {
			case *ERange:
				base = &ESlice{X: base, Lo: r.Lo, Hi: r.Hi, Incl: r.Incl}
			case *ERangeFrom:
				base = &ESlice{X: base, Lo: r.Lo}
			default:
				base = &EIndex{base, ix}
			}
			pos = e
		case "(":
			as, e := p.args(t.Pos, ")")
			base, pos = &ECallV{base, as}, e
		default:
			return base, pos
		}
	}
}

func (p *parser) primary(pos int) (Expr, int) {
	t := p.lex(pos)
	sp := tstart(t)
	switch t.Kind {
	case "int", "float", "dec", "str", "raw", "bytes", "char":
		return &ELit{t.Kind, t.Text, sp}, t.Pos
	case "fstring":
		return p.fstring(t), t.Pos
	}
	switch t.Text {
	case "true", "false":
		return &ELit{"bool", t.Text, sp}, t.Pos
	case "spawn":
		if o := p.lex(t.Pos); o.Text == "(" {
			cend := p.closeParen(o.Pos)
			b, e := p.block(cend)
			return &ESpawn{strings.TrimSpace(p.s.slice(o.Pos, cend-1)), b}, e
		}
	case "if":
		return p.ifx(t.Pos)
	case "match":
		return p.match(t.Pos)
	case "lambda":
		return p.lambda(t.Pos)
	case "(":
		x, e := p.expr(t.Pos)
		return x, p.expect(e, ")")
	case "[":
		as, e := p.args(t.Pos, "]")
		return &EList{as}, e
	case "{":
		return p.braceLit(t.Pos)
	}
	if t.Kind == "ident" {
		t2 := p.lex(t.Pos)
		if t2.Text == "(" {
			as, e := p.args(t2.Pos, ")")
			return &ECall{t.Text, as, sp}, e
		}
		if t2.Text == "{" && p.isStructLit(t2.Pos) {
			return p.structLit(t.Text, t2.Pos)
		}
		return &ERef{t.Text, sp}, t.Pos
	}
	p.fail(t.Pos, "unexpected token %q", t.Text)
	return nil, 0
}

func (p *parser) isStructLit(pos int) bool {
	t := p.lex(pos)
	return t.Text == "}" || t.Text == "."
}

func (p *parser) structLit(name string, pos int) (Expr, int) {
	st := &EStruct{Name: name}
	at := pos
	for {
		t := p.lex(at)
		if t.Text == "}" {
			return st, t.Pos
		}
		if t.Text != "." {
			p.fail(t.Pos, "expected . in struct literal")
		}
		f := p.lex(t.Pos)
		eq := p.lex(f.Pos)
		if eq.Text != "=" {
			p.fail(eq.Pos, "expected = in struct literal")
		}
		v, e := p.expr(eq.Pos)
		st.Fields = append(st.Fields, f.Text)
		st.Vals = append(st.Vals, v)
		sep := p.lex(e)
		if sep.Text == "}" {
			return st, sep.Pos
		}
		if sep.Text != "," {
			p.fail(sep.Pos, "expected , or } in struct literal")
		}
		at = sep.Pos
	}
}

func (p *parser) braceLit(pos int) (Expr, int) {
	t := p.lex(pos)
	if t.Text == "}" {
		return &EEmpty{}, t.Pos
	}
	first, e := p.expr(pos)
	nx := p.lex(e)
	if nx.Text == ":" {
		m := &EMap{}
		k := first
		at := nx.Pos
		for {
			v, ve := p.expr(at)
			m.Keys = append(m.Keys, k)
			m.Vals = append(m.Vals, v)
			sep := p.lex(ve)
			if sep.Text == "}" {
				return m, sep.Pos
			}
			if sep.Text != "," {
				p.fail(sep.Pos, "expected , or } in map literal")
			}
			if n2 := p.lex(sep.Pos); n2.Text == "}" {
				return m, n2.Pos
			}
			var ke int
			k, ke = p.expr(sep.Pos)
			at = p.expect(ke, ":")
		}
	}
	if nx.Text == "}" {
		return &ESet{[]Expr{first}}, nx.Pos
	}
	if nx.Text != "," {
		p.fail(nx.Pos, "malformed brace literal")
	}
	rest, end := p.args(nx.Pos, "}")
	return &ESet{append([]Expr{first}, rest...)}, end
}

// The literal text of an f-string segment with {{ and }} decoded.
func flit(text []rune, i, n int) string {
	var b strings.Builder
	for i < n {
		c := text[i]
		if (c == '{' || c == '}') && i+1 < n && text[i+1] == c {
			b.WriteRune(c)
			i += 2
			continue
		}
		b.WriteRune(c)
		i++
	}
	return b.String()
}

func holeEnd(text []rune, i, n, depth int) int {
	for i < n {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

func (p *parser) fstring(t Tok) Expr {
	base := tstart(t)
	text := []rune(t.Text)
	n := len(text) - 1
	fs := &EFStr{}
	i, seg := 2, 2
	for i < n {
		c := text[i]
		if c == '{' && i+1 < n && text[i+1] == '{' {
			i += 2
			continue
		}
		if c != '{' {
			i++
			continue
		}
		e := holeEnd(text, i+1, n, 1)
		if e < 0 {
			p.fail(base+i, "unterminated f-string hole")
		}
		fs.Parts = append(fs.Parts, flit(text, seg, i))
		h, hend := p.expr(base + i + 1)
		cl := p.lex(hend)
		spec := ""
		if cl.Text == ":" {
			spec = string(text[cl.Pos-base : e])
		} else if cl.Pos != base+e+1 {
			p.fail(hend, "f-string hole is not one expression")
		}
		fs.Holes = append(fs.Holes, h)
		fs.Specs = append(fs.Specs, spec)
		i, seg = e+1, e+1
	}
	fs.Parts = append(fs.Parts, flit(text, seg, n))
	return fs
}

func (p *parser) ifx(pos int) (Expr, int) {
	o := p.lex(pos)
	c, e := p.expr(o.Pos)
	e = p.expect(e, ")")
	e = p.expect(e, "{")
	th, e := p.bexpr(e)
	e = p.expect(e, "}")
	kw := p.lex(e)
	if kw.Text != "else" {
		return &EIf{c, th, nil}, e
	}
	nx := p.lex(kw.Pos)
	if nx.Text == "if" {
		el, e2 := p.ifx(nx.Pos)
		return &EIf{c, th, el}, e2
	}
	if nx.Text != "{" {
		p.fail(nx.Pos, "expected { after else")
	}
	el, e2 := p.bexpr(nx.Pos)
	e2 = p.expect(e2, "}")
	return &EIf{c, th, el}, e2
}

func (p *parser) match(pos int) (Expr, int) {
	t := p.lex(pos)
	var sc Expr
	var e int
	t2 := p.lex(t.Pos)
	if t.Text == "(" || t2.Text == "(" {
		sc, e = p.primary(pos)
	} else {
		sc, e = &ERef{t.Text, tstart(t)}, t.Pos
	}
	e = p.expect(e, "{")
	m := &EMatch{X: sc}
	for {
		a := p.lex(e)
		if a.Text == "}" {
			return m, a.Pos
		}
		if a.Kind != "ident" {
			p.fail(a.Pos, "match arm pattern must be a variant name or _")
		}
		arm := Arm{Variant: a.Text}
		at := a.Pos
		if o := p.lex(at); o.Text == "(" {
			at = o.Pos
			for {
				nm := p.lex(at)
				arm.Names = append(arm.Names, nm.Text)
				sep := p.lex(nm.Pos)
				at = sep.Pos
				if sep.Text == ")" {
					break
				}
				if sep.Text != "," {
					p.fail(sep.Pos, "expected , or ) in match arm pattern")
				}
			}
		}
		at = p.expect(at, "=>")
		body, be := p.expr(at)
		arm.Body = body
		m.Arms = append(m.Arms, arm)
		sep := p.lex(be)
		if sep.Text == "}" {
			return m, sep.Pos
		}
		if sep.Text != "," {
			p.fail(sep.Pos, "expected , or } after match arm")
		}
		e = sep.Pos
	}
}

func (p *parser) lambda(pos int) (Expr, int) {
	o := p.lex(pos)
	if o.Text != "(" {
		p.fail(o.Pos, "expected ( after lambda")
	}
	l := &ELambda{}
	at := o.Pos
	if t := p.lex(at); t.Text == ")" {
		at = t.Pos
	} else {
		for {
			nm := p.lex(at)
			l.Params = append(l.Params, nm.Text)
			sep := p.lex(nm.Pos)
			at = sep.Pos
			if sep.Text == ")" {
				break
			}
			if sep.Text != "," {
				p.fail(sep.Pos, "expected , or ) in lambda parameters")
			}
		}
	}
	at = p.expect(at, "{")
	body, e := p.bexpr(at)
	l.Body = body
	return l, p.expect(e, "}")
}

// ─── Block expressions and statements ──────────────────────────────────

// bindHead: `Type name =` at pos; returns the type, name and the position
// after `=`.
func (p *parser) bindHead(pos int) (*Type, string, int, bool) {
	t := p.lex(pos)
	if t.Kind != "ident" {
		return nil, "", 0, false
	}
	if t2 := p.lex(t.Pos); t2.Text == "[" {
		// constraint-typed binding `T[pred] x = e`: the predicate is ignored.
		cl := t2.Pos
		for {
			c := p.lex(cl)
			if c.Kind == "eof" {
				return nil, "", 0, false
			}
			cl = c.Pos
			if c.Text == "]" {
				break
			}
		}
		nm := p.lex(cl)
		eq := p.lex(nm.Pos)
		if nm.Kind != "ident" || eq.Text != "=" {
			return nil, "", 0, false
		}
		return &Type{Name: t.Text}, nm.Text, eq.Pos, true
	}
	ty, end := p.parseType(pos)
	if ty == nil {
		return nil, "", 0, false
	}
	nm := p.lex(end)
	eq := p.lex(nm.Pos)
	if nm.Kind != "ident" || eq.Text != "=" {
		return nil, "", 0, false
	}
	return ty, nm.Text, eq.Pos, true
}

// bexpr: `{ T x = e; ... e [;] }` body after `{`, stopping before `}`.
func (p *parser) bexpr(pos int) (Expr, int) {
	b := &EBlock{}
	for {
		if ty, name, at, ok := p.bindHead(pos); ok {
			x, e := p.expr(at)
			b.Binds = append(b.Binds, &SBind{ty, name, x})
			pos = p.expect(e, ";")
			continue
		}
		x, e := p.expr(pos)
		semi := p.lex(e)
		after := p.lex(semi.Pos)
		if semi.Text == ";" && after.Text == "}" {
			e = semi.Pos
		}
		b.Last = x
		if len(b.Binds) == 0 {
			return x, e
		}
		return b, e
	}
}

func (p *parser) block(pos int) (*SBlock, int) {
	o := p.lex(pos)
	if o.Text != "{" {
		p.fail(o.Pos, "expected {, found %q", o.Text)
	}
	b := &SBlock{}
	at := o.Pos
	for {
		t := p.lex(at)
		if t.Text == "}" {
			return b, t.Pos
		}
		if t.Kind == "eof" {
			p.fail(t.Pos, "unexpected end of input in block")
		}
		st, e := p.stmt(at)
		b.Stmts = append(b.Stmts, st)
		at = e
	}
}

func (p *parser) patHead(pos int) (string, string, int, bool) {
	v := p.lex(pos)
	if v.Text != "Some" && v.Text != "Ok" && v.Text != "Err" {
		return "", "", 0, false
	}
	o := p.lex(v.Pos)
	nm := p.lex(o.Pos)
	c := p.lex(nm.Pos)
	eq := p.lex(c.Pos)
	if o.Text != "(" || nm.Kind != "ident" || c.Text != ")" || eq.Text != "=" {
		return "", "", 0, false
	}
	return v.Text, nm.Text, eq.Pos, true
}

func (p *parser) cond(pos int) (Expr, int) {
	if v, n, at, ok := p.patHead(pos); ok {
		x, e := p.expr(at)
		return &CPat{v, n, x}, e
	}
	return p.expr(pos)
}

func (p *parser) semi(st Stmt, pos int) (Stmt, int) { return st, p.expect(pos, ";") }

func (p *parser) stmt(pos int) (Stmt, int) {
	t := p.lex(pos)
	switch t.Text {
	case "return":
		if e := p.lex(t.Pos); e.Text == ";" {
			return &SReturn{}, e.Pos
		}
		x, e := p.expr(t.Pos)
		return p.semi(&SReturn{x}, e)
	case "@":
		an := p.lex(t.Pos)
		if an.Text == "residual" {
			if ty, name, at, ok := p.bindHead(an.Pos); ok {
				x, e := p.expr(at)
				return p.semi(&SBind{ty, name, x}, e)
			}
		}
	case "break":
		return p.semi(&SBreak{}, t.Pos)
	case "continue":
		return p.semi(&SCont{}, t.Pos)
	case "if":
		o := p.lex(t.Pos)
		if o.Text != "(" {
			p.fail(o.Pos, "expected ( after if")
		}
		c, e := p.cond(o.Pos)
		e = p.expect(e, ")")
		th, e := p.stmt(e)
		if kw := p.lex(e); kw.Text == "else" {
			el, e2 := p.stmt(kw.Pos)
			return &SIf{c, th, el}, e2
		}
		return &SIf{c, th, nil}, e
	case "while":
		o := p.lex(t.Pos)
		c, e := p.cond(o.Pos)
		e = p.expect(e, ")")
		body, e := p.stmt(e)
		return &SWhile{c, body}, e
	case "for":
		o := p.lex(t.Pos)
		ty, e := p.parseType(o.Pos)
		nm := p.lex(e)
		kw := p.lex(nm.Pos)
		if kw.Text != "in" {
			p.fail(kw.Pos, "expected `in` in for statement")
		}
		it, e2 := p.expr(kw.Pos)
		e2 = p.expect(e2, ")")
		body, e3 := p.stmt(e2)
		return &SFor{ty, nm.Text, it, body}, e3
	case "with":
		p.fail(t.Pos, "`with` is not supported by this interpreter")
	case "{":
		return p.block(pos)
	}
	if v, n, at, ok := p.patHead(pos); ok {
		x, e := p.expr(at)
		return p.semi(&SLetPat{v, n, x}, e)
	}
	if t.Kind == "ident" {
		if b := p.lex(t.Pos); b.Text == "{" {
			f := p.lex(b.Pos)
			sep := p.lex(f.Pos)
			if f.Kind == "ident" && (sep.Text == "," || sep.Text == "}") {
				d := &SDestr{TypeName: t.Text}
				at := b.Pos
				for {
					n := p.lex(at)
					d.Names = append(d.Names, n.Text)
					s := p.lex(n.Pos)
					at = s.Pos
					if s.Text == "}" {
						break
					}
				}
				at = p.expect(at, "=")
				x, e := p.expr(at)
				d.X = x
				return p.semi(d, e)
			}
		}
	}
	if t.Text == "_" {
		if eq := p.lex(t.Pos); eq.Text == "=" {
			x, e := p.expr(eq.Pos)
			return p.semi(&SDiscard{x}, e)
		}
	}
	if ty, name, at, ok := p.bindHead(pos); ok {
		x, e := p.expr(at)
		return p.semi(&SBind{ty, name, x}, e)
	}
	x, e := p.expr(pos)
	if p.lex(e).Text == "=" {
		p.fail(e, "cannot assign")
	}
	return p.semi(&SExpr{x}, e)
}

// ─── Declarations ──────────────────────────────────────────────────────

func (p *parser) closeParen(pos int) int {
	depth := 0
	for {
		t := p.lex(pos)
		if t.Kind == "eof" {
			return t.Pos
		}
		if t.Text == "(" {
			depth++
		} else if t.Text == ")" {
			if depth == 0 {
				return t.Pos
			}
			depth--
		}
		pos = t.Pos
	}
}

func (p *parser) prefixEnd(pos int) int {
	for {
		t := p.lex(pos)
		if t.Text == "pub" {
			pos = t.Pos
			continue
		}
		if t.Text != "@" {
			return pos
		}
		an := p.lex(t.Pos)
		lp := p.lex(an.Pos)
		if lp.Text != "(" {
			pos = an.Pos
			continue
		}
		pos = p.closeParen(lp.Pos)
	}
}

func (p *parser) params(pos int) ([]Param, int) {
	var ps []Param
	at := p.expect(pos, "(")
	if t := p.lex(at); t.Text == ")" {
		return ps, t.Pos
	}
	for {
		ty, e := p.parseType(at)
		if ty == nil {
			p.fail(at, "expected a parameter type")
		}
		nm := p.lex(e)
		if nm.Kind != "ident" {
			p.fail(nm.Pos, "expected parameter name")
		}
		par := Param{T: ty, Name: nm.Text}
		sep := p.lex(nm.Pos)
		if sep.Text == "=" {
			d, de := p.expr(sep.Pos)
			par.Default = d
			sep = p.lex(de)
		}
		ps = append(ps, par)
		if sep.Text == ")" {
			return ps, sep.Pos
		}
		if sep.Text != "," {
			p.fail(sep.Pos, "expected , or ) in parameters")
		}
		at = sep.Pos
	}
}

// skipDecl: position after the declaration starting at pos (balanced to
// its closing `}` or `;`).
func (p *parser) skipDecl(pos int) int {
	depth := 0
	for {
		t := p.lex(pos)
		pos = t.Pos
		switch {
		case t.Kind == "eof":
			return pos
		case t.Text == "{" || t.Text == "(" || t.Text == "[":
			depth++
		case t.Text == "}" || t.Text == ")" || t.Text == "]":
			depth--
			if depth == 0 && t.Text == "}" {
				if nx := p.lex(pos); nx.Text == ";" {
					return nx.Pos
				}
				return pos
			}
		case t.Text == ";" && depth == 0:
			return pos
		}
	}
}

func (p *parser) decls(m *Module, pos int, close string) int {
	for {
		t := p.lex(pos)
		if t.Kind == "eof" || t.Text == close {
			return t.Pos
		}
		if t.Text == ";" {
			pos = t.Pos
			continue
		}
		if t.Text == "import" {
			pos = p.lex(t.Pos).Pos
			if nx := p.lex(pos); nx.Text == "as" {
				pos = p.lex(nx.Pos).Pos
			}
			pos = p.expect(pos, ";")
			continue
		}
		pos = p.decl(m, pos)
	}
}

func (p *parser) decl(m *Module, pos int) int {
	sp := p.s.skipAll(pos)
	pe := p.prefixEnd(pos)
	prefix := strings.TrimSpace(p.s.slice(sp, p.s.skipAll(pe)))
	pub := false
	for _, w := range strings.Fields(prefix) {
		if w == "pub" {
			pub = true
		}
	}
	t := p.lex(pe)
	tn := p.lex(t.Pos)
	switch {
	case t.Text == "sandbox":
		lp := p.lex(t.Pos)
		cend := p.closeParen(lp.Pos)
		lb := p.lex(cend)
		return p.decls(m, lb.Pos, "}")
	case t.Text == "type":
		return p.typeDecl(m, t.Pos)
	case t.Text == "test" && tn.Kind == "str":
		_, e := p.block(tn.Pos)
		return e
	case t.Text == "behavior" && tn.Kind == "ident":
		return p.skipDecl(t.Pos)
	}
	// an instance `B(T) = f;`
	if t.Kind == "ident" && tn.Text == "(" {
		cl := p.closeParen(tn.Pos)
		if eq := p.lex(cl); eq.Text == "=" {
			return p.skipDecl(eq.Pos)
		}
	}
	rt, e := p.parseType(pe)
	if rt == nil {
		p.fail(pe, "expected a declaration")
	}
	nm := p.lex(e)
	if nm.Kind != "ident" {
		p.fail(nm.Pos, "expected a function declaration")
	}
	ps, e2 := p.params(nm.Pos)
	body, e3 := p.block(e2)
	fd := &FnDecl{Name: nm.Text, Params: ps, Ret: rt, Body: body, Pub: pub, Annots: prefix, Mod: m}
	fd.Link = strings.Contains(prefix, "@link")
	m.Fns[nm.Text] = fd
	return e3
}

func (p *parser) typeDecl(m *Module, pos int) int {
	nm := p.lex(pos)
	at := nm.Pos
	if g := p.lex(at); g.Text == "(" {
		at = p.closeParen(g.Pos)
	}
	eq := p.lex(at)
	if eq.Text != "=" {
		p.fail(eq.Pos, "expected = in type declaration")
	}
	td := &TypeDecl{Name: nm.Text, Mod: m}
	m.Types[nm.Text] = td
	br := p.lex(eq.Pos)
	if br.Text == "{" {
		td.Kind = "struct"
		f := br.Pos
		for {
			t := p.lex(f)
			if t.Text == "}" {
				return p.expect(t.Pos, ";")
			}
			ty, e := p.parseType(f)
			fn := p.lex(e)
			s := p.lex(fn.Pos)
			if ty == nil || fn.Kind != "ident" || s.Text != ";" {
				p.fail(s.Pos, "expected `Type name;` in struct")
			}
			td.Fields = append(td.Fields, Param{T: ty, Name: fn.Text})
			f = s.Pos
		}
	}
	if vs, end, ok := p.variantList(eq.Pos); ok {
		td.Kind = "sum"
		for i := range vs {
			vs[i].Sum = td
		}
		td.Variants = vs
		return end
	}
	ty, e := p.parseType(eq.Pos)
	if ty == nil {
		p.fail(eq.Pos, "malformed type declaration")
	}
	td.Kind = "alias"
	td.Alias = ty
	return p.skipDecl(e - 0)
}

func (p *parser) variantList(pos int) ([]Variant, int, bool) {
	var vs []Variant
	at := pos
	for {
		n := p.lex(at)
		if n.Kind != "ident" {
			return nil, 0, false
		}
		v := Variant{Name: n.Text}
		at = n.Pos
		if o := p.lex(at); o.Text == "(" {
			at = o.Pos
			for {
				ty, e := p.parseType(at)
				if ty == nil {
					return nil, 0, false
				}
				v.Payloads = append(v.Payloads, ty)
				sep := p.lex(e)
				at = sep.Pos
				if sep.Text == ")" {
					break
				}
				if sep.Text != "," {
					return nil, 0, false
				}
			}
		}
		vs = append(vs, v)
		sep := p.lex(at)
		if sep.Text == "|" {
			at = sep.Pos
			continue
		}
		if sep.Text == ";" && len(vs) > 1 {
			return vs, sep.Pos, true
		}
		return nil, 0, false
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
