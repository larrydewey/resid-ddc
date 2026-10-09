package interp

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The evaluator compiles each function's AST once into Go closures over a
// frame of slots (one per binder), then runs them. A `return f(...)` in
// tail position (directly, or through an if/ternary/block) is a tail call:
// the caller's loop reuses itself instead of growing the Go stack, which
// matters because Resid loops are tail recursion.

type evalFn func(*Frame) Value
type stmtFn func(*Frame) int

const (
	cNorm = iota
	cRet
	cBrk
	cCont
	cTail
)

type Frame struct {
	slots    []Value
	env      []Value // a lambda's captured slots
	ret      Value
	tailFn   *cfunc
	tailArgs []Value // scratch for a tail call's arguments, reused
	fn       *cfunc
}

// The value stack: frames are windows of a segment, pushed and popped in
// call order, so a call allocates nothing. Frame structs are pooled by
// depth for the same reason (no closure keeps a Frame: a lambda copies
// the slots it captures).
type stackSeg struct {
	vals []Value
	next *stackSeg
}

type mark struct {
	seg *stackSeg
	sp  int
}

func (in *Interp) push(n int) ([]Value, mark) {
	m := mark{in.cur, in.sp}
	if in.sp+n > len(in.cur.vals) {
		nx := in.cur.next
		if nx == nil || len(nx.vals) < n {
			nx = &stackSeg{vals: make([]Value, max(1<<16, n))}
			in.cur.next = nx
		}
		in.cur, in.sp = nx, 0
	}
	sl := in.cur.vals[in.sp : in.sp+n : in.sp+n]
	in.sp += n
	return sl, m
}

func (in *Interp) pop(m mark, sl []Value) {
	clear(sl)
	in.cur, in.sp = m.seg, m.sp
}

type cfunc struct {
	name    string
	decl    *FnDecl
	nslots  int
	nparams int
	pco     []func(Value) Value
	rco     func(Value) Value
	body    stmtFn
	lambda  *ELambda
	ready   bool
	usesTry bool
	hasCo   bool
	calls   int64
}

type Interp struct {
	prog    *Program
	fns     map[*FnDecl]*cfunc
	opts    Options
	args    []string
	out     *outBuf
	rawmem  map[int64]byte
	globals map[string]int64
	nextGlb int64
	flags   map[string]bool
	cur     *stackSeg
	sp      int
	frames  []*Frame
	depth   int
}

type tryReturn struct{ v Value }

// ─── Compile context ───────────────────────────────────────────────────

type cctx struct {
	in       *Interp
	mod      *Module
	scopes   []map[string]int
	nslots   int
	parent   *cctx // enclosing function, for a lambda
	captures map[string]int
	usesTry  bool
}

func (c *cctx) push() { c.scopes = append(c.scopes, map[string]int{}) }
func (c *cctx) pop()  { c.scopes = c.scopes[:len(c.scopes)-1] }
func (c *cctx) bind(name string) int {
	s := c.nslots
	c.nslots++
	if name != "_" {
		c.scopes[len(c.scopes)-1][name] = s
	}
	return s
}

func (c *cctx) lookup(name string) (int, bool) {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if s, ok := c.scopes[i][name]; ok {
			return s, true
		}
	}
	return 0, false
}

// lookupOuter: a variable of an enclosing function, captured by a lambda.
func (c *cctx) lookupOuter(name string) (int, bool) {
	if c.parent == nil {
		return 0, false
	}
	if s, ok := c.parent.lookup(name); ok {
		return s, true
	}
	return 0, false
}

func (in *Interp) fnFor(mod *Module, name string) *FnDecl {
	if mod != nil {
		if f, ok := mod.Fns[name]; ok {
			return f
		}
	}
	return in.prog.Pub[name]
}

func (in *Interp) cfuncOf(d *FnDecl) *cfunc {
	if cf, ok := in.fns[d]; ok {
		return cf
	}
	cf := &cfunc{name: d.Name, decl: d}
	in.fns[d] = cf
	return cf
}

func (in *Interp) ensure(cf *cfunc) {
	if cf.ready {
		return
	}
	cf.ready = true
	d := cf.decl
	c := &cctx{in: in, mod: d.Mod}
	c.push()
	for _, p := range d.Params {
		c.bind(p.Name)
		cf.pco = append(cf.pco, coercer(in, p.T))
	}
	cf.nparams = len(d.Params)
	for _, co := range cf.pco {
		if co != nil {
			cf.hasCo = true
		}
	}
	cf.rco = coercer(in, d.Ret)
	cf.body = c.block(d.Body, true)
	cf.nslots = c.nslots
	cf.usesTry = c.usesTry
}

// ─── Calling ───────────────────────────────────────────────────────────

func (in *Interp) call(cf *cfunc, args []Value, env []Value) Value {
	in.ensure(cf)
	if len(args) != cf.nparams {
		abort(fmt.Sprintf("%s expects %d arguments, got %d", cf.name, cf.nparams, len(args)))
	}
	slots, m := in.push(cf.nslots)
	copy(slots, args)
	return in.invoke(cf, slots, m, env)
}

// invoke runs cf in the frame window slots (pushed at m), its arguments
// already in place, and pops the window.
func (in *Interp) invoke(cf *cfunc, slots []Value, m mark, env []Value) Value {
	in.depth++
	for in.depth >= len(in.frames) {
		in.frames = append(in.frames, &Frame{})
	}
	fr := in.frames[in.depth]
	for {
		if cf.hasCo {
			for i, co := range cf.pco {
				if co != nil {
					slots[i] = co(slots[i])
				}
			}
		}
		fr.slots, fr.env, fr.ret, fr.fn = slots, env, nil, cf
		cf.calls++
		var ctl int
		if !cf.usesTry {
			ctl = cf.body(fr)
		} else {
			ctl = in.runTry(cf, fr)
		}
		if ctl == cTail {
			// Reuse this window for the callee.
			ncf := fr.tailFn
			in.ensure(ncf)
			in.pop(m, slots)
			slots, m = in.push(ncf.nslots)
			copy(slots, fr.tailArgs)
			clear(fr.tailArgs)
			cf, env = ncf, nil
			continue
		}
		r := fr.ret
		fr.slots, fr.env, fr.ret = nil, nil, nil
		in.pop(m, slots)
		in.depth--
		if cf.rco != nil {
			return cf.rco(r)
		}
		return r
	}
}

func (in *Interp) runTry(cf *cfunc, fr *Frame) (ctl int) {
	depth, cur, sp := in.depth, in.cur, in.sp
	{
		func() {
			defer func() {
				if r := recover(); r != nil {
					if tr, ok := r.(*tryReturn); ok {
						in.depth, in.cur, in.sp = depth, cur, sp
						fr.ret = tr.v
						ctl = cRet
						return
					}
					panic(r)
				}
			}()
			ctl = cf.body(fr)
		}()
	}
	return ctl
}

// ─── Statements ────────────────────────────────────────────────────────

func (c *cctx) block(b *SBlock, fnBody bool) stmtFn {
	c.push()
	defer c.pop()
	fs := make([]stmtFn, 0, len(b.Stmts))
	for _, s := range b.Stmts {
		fs = append(fs, c.stmt(s))
	}
	if len(fs) == 1 {
		return fs[0]
	}
	return func(fr *Frame) int {
		for _, f := range fs {
			if r := f(fr); r != cNorm {
				return r
			}
		}
		return cNorm
	}
}

func (c *cctx) stmt(s Stmt) stmtFn {
	switch st := s.(type) {
	case *SReturn:
		if st.X == nil {
			return func(fr *Frame) int { fr.ret = nil; return cRet }
		}
		return c.tail(st.X)
	case *SBind:
		x := c.expr(st.X)
		co := coercer(c.in, st.T)
		slot := c.bind(st.Name)
		if co != nil {
			return func(fr *Frame) int { fr.slots[slot] = co(x(fr)); return cNorm }
		}
		return func(fr *Frame) int { fr.slots[slot] = x(fr); return cNorm }
	case *SDiscard:
		x := c.expr(st.X)
		return func(fr *Frame) int { x(fr); return cNorm }
	case *SExpr:
		x := c.expr(st.X)
		return func(fr *Frame) int { x(fr); return cNorm }
	case *SBlock:
		return c.block(st, false)
	case *SBreak:
		return func(*Frame) int { return cBrk }
	case *SCont:
		return func(*Frame) int { return cCont }
	case *SIf:
		c.push()
		cond, bindSlot, isPat := c.condition(st.C)
		th := c.stmt(st.Then)
		c.pop()
		var el stmtFn
		if st.Else != nil {
			el = c.stmt(st.Else)
		}
		return func(fr *Frame) int {
			v := cond(fr)
			ok := false
			if isPat {
				if pv, matched := v.(*Var); matched && pv != nil {
					ok = true
					if bindSlot >= 0 {
						fr.slots[bindSlot] = pv.P[0]
					}
				}
			} else {
				ok = v.(bool)
			}
			if ok {
				return th(fr)
			}
			if el != nil {
				return el(fr)
			}
			return cNorm
		}
	case *SWhile:
		c.push()
		cond, bindSlot, isPat := c.condition(st.C)
		body := c.stmt(st.Body)
		c.pop()
		return func(fr *Frame) int {
			for {
				v := cond(fr)
				if isPat {
					pv, matched := v.(*Var)
					if !matched || pv == nil {
						return cNorm
					}
					if bindSlot >= 0 {
						fr.slots[bindSlot] = pv.P[0]
					}
				} else if !v.(bool) {
					return cNorm
				}
				switch r := body(fr); r {
				case cBrk:
					return cNorm
				case cRet, cTail:
					return r
				}
			}
		}
	case *SFor:
		iter := c.expr(st.Iter)
		c.push()
		slot := c.bind(st.Name)
		co := coercer(c.in, st.T)
		body := c.stmt(st.Body)
		c.pop()
		run := func(fr *Frame, v Value) (int, bool) {
			if co != nil {
				v = co(v)
			}
			fr.slots[slot] = v
			switch r := body(fr); r {
			case cBrk:
				return cNorm, true
			case cRet, cTail:
				return r, true
			}
			return cNorm, false
		}
		if rg, ok := st.Iter.(*ERange); ok {
			lo, hi := c.expr(rg.Lo), c.expr(rg.Hi)
			incl := rg.Incl
			return func(fr *Frame) int {
				a, b := lo(fr).(int64), hi(fr).(int64)
				if incl {
					b++
				}
				for i := a; i < b; i++ {
					if r, stop := run(fr, i); stop {
						return r
					}
				}
				return cNorm
			}
		}
		return func(fr *Frame) int {
			for _, v := range iterValues(iter(fr)) {
				if r, stop := run(fr, v); stop {
					return r
				}
			}
			return cNorm
		}
	case *SLetPat:
		x := c.expr(st.X)
		slot := c.bind(st.Name)
		want := st.Variant
		return func(fr *Frame) int {
			v, ok := x(fr).(*Var)
			if !ok || v.V.Name != want {
				abort("pattern " + want + "(...) did not match")
			}
			fr.slots[slot] = v.P[0]
			return cNorm
		}
	case *SDestr:
		x := c.expr(st.X)
		td := c.in.prog.Types[st.TypeName]
		if td == nil {
			panic(fmt.Sprintf("unknown type %s in destructuring", st.TypeName))
		}
		var idx, slots []int
		for _, n := range st.Names {
			idx = append(idx, fieldIndex(td, n))
			slots = append(slots, c.bind(n))
		}
		return func(fr *Frame) int {
			r := x(fr).(*Rec)
			for i, k := range idx {
				fr.slots[slots[i]] = r.F[k]
			}
			return cNorm
		}
	}
	panic(fmt.Sprintf("statement %T not supported", s))
}

// condition compiles an if/while condition; a `V(n) = e` pattern binds n.
func (c *cctx) condition(e Expr) (evalFn, int, bool) {
	if p, ok := e.(*CPat); ok {
		x := c.expr(p.X)
		slot := c.bind(p.Name)
		want := p.Variant
		return func(fr *Frame) Value {
			v, ok := x(fr).(*Var)
			if !ok || v.V.Name != want {
				return (*Var)(nil)
			}
			return v
		}, slot, true
	}
	return c.expr(e), -1, false
}

// tail compiles `return e` with e in tail position.
func (c *cctx) tail(e Expr) stmtFn {
	switch x := e.(type) {
	case *ECall:
		if _, local := c.lookup(x.Name); !local {
			if d := c.in.fnFor(c.mod, x.Name); d != nil && !d.Link {
				cf := c.in.cfuncOf(d)
				args := c.callArgs(d, x.Args)
				return func(fr *Frame) int {
					if cap(fr.tailArgs) < len(args) {
						fr.tailArgs = make([]Value, len(args), max(8, len(args)))
					}
					av := fr.tailArgs[:len(args)]
					for i, a := range args {
						av[i] = a(fr)
					}
					fr.tailFn, fr.tailArgs = cf, av
					return cTail
				}
			}
		}
	case *EIf:
		if x.Else != nil {
			cond := c.expr(x.C)
			th, el := c.tail(x.Then), c.tail(x.Else)
			return func(fr *Frame) int {
				if cond(fr).(bool) {
					return th(fr)
				}
				return el(fr)
			}
		}
	case *ETern:
		cond := c.expr(x.C)
		th, el := c.tail(x.A), c.tail(x.B)
		return func(fr *Frame) int {
			if cond(fr).(bool) {
				return th(fr)
			}
			return el(fr)
		}
	case *EBlock:
		c.push()
		defer c.pop()
		var binds []stmtFn
		for _, b := range x.Binds {
			binds = append(binds, c.stmt(b))
		}
		last := c.tail(x.Last)
		return func(fr *Frame) int {
			for _, b := range binds {
				b(fr)
			}
			return last(fr)
		}
	}
	v := c.expr(e)
	return func(fr *Frame) int { fr.ret = v(fr); return cRet }
}

// callArgs orders a call's arguments by the callee's parameters: named
// arguments go to their slot and missing trailing ones take defaults.
func (c *cctx) callArgs(d *FnDecl, args []Expr) []evalFn {
	named := false
	for _, a := range args {
		if _, ok := a.(*ENamed); ok {
			named = true
		}
	}
	if !named && len(args) >= len(d.Params) {
		out := make([]evalFn, len(args))
		for i, a := range args {
			out[i] = c.expr(a)
		}
		return out
	}
	slots := make([]Expr, len(d.Params))
	pos := 0
	for _, a := range args {
		if n, ok := a.(*ENamed); ok {
			for i, p := range d.Params {
				if p.Name == n.Name {
					slots[i] = n.X
				}
			}
			continue
		}
		if pos < len(slots) {
			slots[pos] = a
		}
		pos++
	}
	out := make([]evalFn, len(slots))
	for i, s := range slots {
		if s == nil {
			if d.Params[i].Default == nil {
				panic(fmt.Sprintf("call to %s is missing argument %s", d.Name, d.Params[i].Name))
			}
			dc := &cctx{in: c.in, mod: d.Mod}
			dc.push()
			out[i] = dc.expr(d.Params[i].Default)
			continue
		}
		out[i] = c.expr(s)
	}
	return out
}

// ─── Expressions ───────────────────────────────────────────────────────

func (c *cctx) exprs(es []Expr) []evalFn {
	out := make([]evalFn, len(es))
	for i, e := range es {
		out[i] = c.expr(e)
	}
	return out
}

func evalAll(fs []evalFn, fr *Frame) []Value {
	out := make([]Value, len(fs))
	for i, f := range fs {
		out[i] = f(fr)
	}
	return out
}

func (c *cctx) expr(e Expr) evalFn {
	switch x := e.(type) {
	case *ELit:
		v := litValue(x)
		return func(*Frame) Value { return v }
	case *EFStr:
		return c.fstr(x)
	case *ERef:
		return c.ref(x)
	case *ENamed:
		return c.expr(x.X)
	case *ECall:
		return c.callExpr(x)
	case *ECallV:
		fn := c.expr(x.Fn)
		args := c.exprs(x.Args)
		return func(fr *Frame) Value { return c.in.apply(fn(fr), evalAll(args, fr)) }
	case *EMCall:
		return c.mcall(x)
	case *EField:
		obj := c.expr(x.X)
		name := x.Name
		return func(fr *Frame) Value { return getField(obj(fr), name) }
	case *EIndex:
		obj, ix := c.expr(x.X), c.expr(x.I)
		return func(fr *Frame) Value { return index(obj(fr), ix(fr)) }
	case *ESlice:
		obj := c.expr(x.X)
		var lo, hi evalFn
		if x.Lo != nil {
			lo = c.expr(x.Lo)
		}
		if x.Hi != nil {
			hi = c.expr(x.Hi)
		}
		incl := x.Incl
		return func(fr *Frame) Value {
			o := obj(fr)
			a, b := int64(0), int64(-1)
			if lo != nil {
				a = lo(fr).(int64)
			}
			if hi != nil {
				b = hi(fr).(int64)
				if incl {
					b++
				}
			}
			return sliceOf(o, a, b, hi != nil)
		}
	case *ERange:
		lo, hi := c.expr(x.Lo), c.expr(x.Hi)
		incl := x.Incl
		return func(fr *Frame) Value {
			a, b := lo(fr).(int64), hi(fr).(int64)
			if incl {
				b++
			}
			var vs []Value
			for i := a; i < b; i++ {
				vs = append(vs, i)
			}
			return NewList(vs)
		}
	case *EBin:
		return c.bin(x)
	case *EUn:
		v := c.expr(x.X)
		switch x.Op {
		case "!":
			return func(fr *Frame) Value { return !v(fr).(bool) }
		case "-":
			return func(fr *Frame) Value {
				a := v(fr)
				if f, ok := a.(float64); ok {
					return -f
				}
				return intNeg(a)
			}
		case "~":
			return func(fr *Frame) Value { return intNot(v(fr)) }
		case "+":
			return v
		}
	case *ECast:
		return c.cast(x)
	case *EIf:
		cond, th := c.expr(x.C), c.expr(x.Then)
		var el evalFn
		if x.Else != nil {
			el = c.expr(x.Else)
		}
		return func(fr *Frame) Value {
			if cond(fr).(bool) {
				return th(fr)
			}
			if el != nil {
				return el(fr)
			}
			return nil
		}
	case *ETern:
		cond, a, b := c.expr(x.C), c.expr(x.A), c.expr(x.B)
		return func(fr *Frame) Value {
			if cond(fr).(bool) {
				return a(fr)
			}
			return b(fr)
		}
	case *ETry:
		c.usesTry = true
		v := c.expr(x.X)
		return func(fr *Frame) Value {
			r := v(fr).(*Var)
			if r.V == vSome || r.V == vOk {
				return r.P[0]
			}
			panic(&tryReturn{r})
		}
	case *EOrElse:
		v, d := c.expr(x.X), c.expr(x.D)
		return func(fr *Frame) Value {
			r, ok := v(fr).(*Var)
			if ok && (r.V == vSome || r.V == vOk) {
				return r.P[0]
			}
			return d(fr)
		}
	case *EMatch:
		return c.match(x)
	case *ELambda:
		return c.lambda(x)
	case *EStruct:
		return c.structLit(x)
	case *EList:
		es := c.exprs(x.Elems)
		return func(fr *Frame) Value { return NewList(evalAll(es, fr)) }
	case *EMap:
		ks, vs := c.exprs(x.Keys), c.exprs(x.Vals)
		return func(fr *Frame) Value {
			m := NewMap()
			for i := range ks {
				m = m.Insert(ks[i](fr), vs[i](fr))
			}
			return m
		}
	case *ESet:
		es := c.exprs(x.Elems)
		return func(fr *Frame) Value {
			m := NewMap()
			for _, f := range es {
				m = m.Insert(f(fr), true)
			}
			return m
		}
	case *EEmpty:
		return func(*Frame) Value { return NewMap() }
	case *EBlock:
		c.push()
		defer c.pop()
		var binds []stmtFn
		for _, b := range x.Binds {
			binds = append(binds, c.stmt(b))
		}
		last := c.expr(x.Last)
		return func(fr *Frame) Value {
			for _, b := range binds {
				b(fr)
			}
			return last(fr)
		}
	}
	panic(fmt.Sprintf("expression %T not supported", e))
}

func (c *cctx) ref(x *ERef) evalFn {
	if s, ok := c.lookup(x.Name); ok {
		return func(fr *Frame) Value { return fr.slots[s] }
	}
	if s, ok := c.lookupOuter(x.Name); ok {
		return func(fr *Frame) Value { return fr.env[s] }
	}
	switch x.Name {
	case "None":
		return func(*Frame) Value { return noneV }
	case "args", "filesystem", "process", "environment", "clock", "network", "terminal":
		v := &Provider{x.Name}
		return func(*Frame) Value { return v }
	}
	if v, ok := c.in.prog.Vars[x.Name]; ok && len(v.Payloads) == 0 {
		val := &Var{V: v}
		return func(*Frame) Value { return val }
	}
	panic(fmt.Sprintf("%s: unknown name %s", c.mod.Path, x.Name))
}

type Provider struct{ Name string }

func (c *cctx) callExpr(x *ECall) evalFn {
	if s, ok := c.lookup(x.Name); ok {
		args := c.exprs(x.Args)
		return func(fr *Frame) Value { return c.in.apply(fr.slots[s], evalAll(args, fr)) }
	}
	if s, ok := c.lookupOuter(x.Name); ok {
		args := c.exprs(x.Args)
		return func(fr *Frame) Value { return c.in.apply(fr.env[s], evalAll(args, fr)) }
	}
	if d := c.in.fnFor(c.mod, x.Name); d != nil && !d.Link {
		cf := c.in.cfuncOf(d)
		args := c.callArgs(d, x.Args)
		in := c.in
		return func(fr *Frame) Value {
			in.ensure(cf)
			slots, m := in.push(cf.nslots)
			for i, a := range args {
				slots[i] = a(fr)
			}
			return in.invoke(cf, slots, m, nil)
		}
	}
	switch x.Name {
	case "Some":
		a := c.expr(x.Args[0])
		return func(fr *Frame) Value { return Some(a(fr)) }
	case "Ok":
		a := c.expr(x.Args[0])
		return func(fr *Frame) Value { return &Var{V: vOk, P: []Value{a(fr)}} }
	case "Err":
		a := c.expr(x.Args[0])
		return func(fr *Frame) Value { return &Var{V: vErr, P: []Value{a(fr)}} }
	}
	if v, ok := c.in.prog.Vars[x.Name]; ok {
		args := c.exprs(x.Args)
		var cos []func(Value) Value
		for _, t := range v.Payloads {
			cos = append(cos, coercer(c.in, t))
		}
		return func(fr *Frame) Value {
			p := evalAll(args, fr)
			for i := range p {
				if i < len(cos) && cos[i] != nil {
					p[i] = cos[i](p[i])
				}
			}
			return &Var{V: v, P: p}
		}
	}
	if x.Name == "ListBuf" {
		return func(*Frame) Value { return &ListBufV{} }
	}
	if x.Name == "StrBuf" {
		return func(*Frame) Value { return &StrBufV{} }
	}
	b := c.in.builtin(x.Name, len(x.Args))
	if b == nil {
		panic(fmt.Sprintf("%s:%d: unknown function %s", c.mod.Path, x.Pos, x.Name))
	}
	// Named arguments to a builtin (sort's `using`) are passed positionally.
	args := c.exprs(x.Args)
	return func(fr *Frame) Value { return b(evalAll(args, fr)) }
}

func (c *cctx) lambda(x *ELambda) evalFn {
	sub := &cctx{in: c.in, mod: c.mod, parent: c}
	sub.push()
	for _, p := range x.Params {
		sub.bind(p)
	}
	body := sub.tail(x.Body)
	cf := &cfunc{name: "lambda", nparams: len(x.Params), body: body, ready: true, usesTry: sub.usesTry}
	cf.nslots = 0
	return func(fr *Frame) Value {
		cf.nslots = sub.nslots
		env := make([]Value, len(fr.slots))
		copy(env, fr.slots)
		return &Closure{Fn: cf, Env: env}
	}
}

func (in *Interp) apply(f Value, args []Value) Value {
	cl, ok := f.(*Closure)
	if !ok {
		abort("calling a value that is not a function")
	}
	if cl.Native != nil {
		return cl.Native(args)
	}
	return in.call(cl.Fn, args, cl.Env)
}

func (c *cctx) structLit(x *EStruct) evalFn {
	td := c.in.prog.Types[x.Name]
	if td == nil || td.Kind != "struct" {
		panic(fmt.Sprintf("%s: unknown record type %s", c.mod.Path, x.Name))
	}
	idx := make([]int, len(x.Fields))
	cos := make([]func(Value) Value, len(x.Fields))
	for i, f := range x.Fields {
		idx[i] = fieldIndex(td, f)
		cos[i] = coercer(c.in, td.Fields[idx[i]].T)
	}
	vals := c.exprs(x.Vals)
	n := len(td.Fields)
	return func(fr *Frame) Value {
		r := &Rec{T: td, F: make([]Value, n)}
		for i, v := range vals {
			x := v(fr)
			if cos[i] != nil {
				x = cos[i](x)
			}
			r.F[idx[i]] = x
		}
		return r
	}
}

func fieldIndex(td *TypeDecl, name string) int {
	for i, f := range td.Fields {
		if f.Name == name {
			return i
		}
	}
	panic(fmt.Sprintf("type %s has no field %s", td.Name, name))
}

func getField(v Value, name string) Value {
	switch r := v.(type) {
	case *Rec:
		for i, f := range r.T.Fields {
			if f.Name == name {
				return r.F[i]
			}
		}
		abort("no field " + name + " in " + r.T.Name)
	case *Var:
		if name == "message" && len(r.P) == 1 {
			return r.P[0]
		}
	}
	abort(fmt.Sprintf("field %s of a %T", name, v))
	return nil
}

func (c *cctx) match(x *EMatch) evalFn {
	sc := c.expr(x.X)
	type carm struct {
		name  string
		slots []int
		body  evalFn
	}
	var arms []carm
	for _, a := range x.Arms {
		c.push()
		ca := carm{name: a.Variant}
		for _, n := range a.Names {
			ca.slots = append(ca.slots, c.bind(n))
		}
		ca.body = c.expr(a.Body)
		c.pop()
		arms = append(arms, ca)
	}
	return func(fr *Frame) Value {
		v := sc(fr).(*Var)
		for _, a := range arms {
			if a.name == "_" || a.name == v.V.Name {
				for i, s := range a.slots {
					fr.slots[s] = v.P[i]
				}
				return a.body(fr)
			}
		}
		abort("no match arm for " + v.V.Name)
		return nil
	}
}

// ─── Operators ─────────────────────────────────────────────────────────

func (c *cctx) bin(x *EBin) evalFn {
	l, r := c.expr(x.L), c.expr(x.R)
	switch x.Op {
	case "&&":
		return func(fr *Frame) Value { return l(fr).(bool) && r(fr).(bool) }
	case "||":
		return func(fr *Frame) Value { return l(fr).(bool) || r(fr).(bool) }
	case "==":
		return func(fr *Frame) Value { return Equal(l(fr), r(fr)) }
	case "!=":
		return func(fr *Frame) Value { return !Equal(l(fr), r(fr)) }
	case "<":
		return func(fr *Frame) Value { return lessThan(l(fr), r(fr)) }
	case ">":
		return func(fr *Frame) Value { return lessThan(r(fr), l(fr)) }
	case "<=":
		return func(fr *Frame) Value { a, b := l(fr), r(fr); return !lessThan(b, a) }
	case ">=":
		return func(fr *Frame) Value { a, b := l(fr), r(fr); return !lessThan(a, b) }
	case "<<", ">>":
		op := x.Op
		return func(fr *Frame) Value { return intShift(op, l(fr), r(fr)) }
	case "+":
		return func(fr *Frame) Value {
			a, b := l(fr), r(fr)
			if x, ok := a.(int64); ok {
				if y, ok2 := b.(int64); ok2 {
					s := x + y
					if (x >= 0) == (y >= 0) && (s >= 0) != (x >= 0) {
						abort(overflowMsg)
					}
					return s
				}
			}
			switch av := a.(type) {
			case *StrV:
				return strConcat(av, b.(*StrV))
			case float64:
				return av + b.(float64)
			}
			return intArith("+", a, b)
		}
	}
	op := x.Op
	return func(fr *Frame) Value { return arith(op, l(fr), r(fr)) }
}

func arith(op string, a, b Value) Value {
	if x, ok := a.(int64); ok {
		if y, ok2 := b.(int64); ok2 {
			return int64Arith(op, x, y)
		}
	}
	switch av := a.(type) {
	case float64:
		bv := b.(float64)
		switch op {
		case "-":
			return av - bv
		case "*":
			return av * bv
		case "/":
			return av / bv
		case "%":
			return math.Mod(av, bv)
		}
	case bool:
		bv := b.(bool)
		switch op {
		case "&":
			return av && bv
		case "|":
			return av || bv
		case "^":
			return av != bv
		}
	}
	return intArith(op, a, b)
}

func lessThan(a, b Value) bool {
	if x, ok := a.(int64); ok {
		if y, ok2 := b.(int64); ok2 {
			return x < y
		}
	}
	return Compare(a, b) < 0
}

func (c *cctx) cast(x *ECast) evalFn {
	v := c.expr(x.X)
	if it, ok := intTypeOf(x.T); ok {
		return func(fr *Frame) Value { return convertInt(v(fr), it, true) }
	}
	switch x.T.Name {
	case "Float":
		return func(fr *Frame) Value {
			switch a := v(fr).(type) {
			case float64:
				return a
			case int64:
				return float64(a)
			case *Num:
				f, _ := new(bigFloat).SetInt(a.V).Float64()
				return f
			}
			abort("cannot convert to Float")
			return nil
		}
	}
	return v
}

// ─── Literals ──────────────────────────────────────────────────────────

func litValue(x *ELit) Value {
	switch x.Kind {
	case "int":
		t := strings.ReplaceAll(x.Text, "_", "")
		if n, err := strconv.ParseInt(t, 0, 64); err == nil {
			return n
		}
		bi, ok := new(bigInt).SetString(t, 0)
		if !ok {
			panic("bad integer literal " + x.Text)
		}
		// A literal too wide for Int: typed at the next width that holds
		// it; the binding or operand it meets converts it.
		w := 128
		for !inRange(bi, w, true) {
			w *= 2
		}
		return &Num{w, true, bi}
	case "float":
		f, _ := strconv.ParseFloat(x.Text, 64)
		return f
	case "bool":
		return x.Text == "true"
	case "str", "raw":
		return S(unescape(x.Text, x.Kind == "raw"))
	case "char":
		r := []rune(unescape(strings.Trim(x.Text, "'"), false))
		if len(r) == 0 {
			return int64(0)
		}
		return int64(r[0])
	case "bytes":
		s := unescapeBytes(x.Text)
		vs := make([]Value, len(s))
		for i, b := range s {
			vs[i] = int64(b)
		}
		return NewList(vs)
	}
	panic("literal kind " + x.Kind + " not supported")
}

func unescape(t string, raw bool) string {
	if strings.HasPrefix(t, "r\"") {
		return t[2 : len(t)-1]
	}
	if strings.HasPrefix(t, "\"") {
		t = t[1 : len(t)-1]
	}
	if raw {
		return t
	}
	return unescapeBody(t)
}

func unescapeBody(t string) string {
	if !strings.Contains(t, "\\") {
		return t
	}
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c != '\\' || i+1 >= len(t) {
			b.WriteByte(c)
			continue
		}
		i++
		switch t[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case '\'':
			b.WriteByte('\'')
		case 'x':
			v, _ := strconv.ParseUint(t[i+1:i+3], 16, 8)
			b.WriteRune(rune(v))
			i += 2
		default:
			b.WriteByte('\\')
			b.WriteByte(t[i])
		}
	}
	return b.String()
}

func unescapeBytes(t string) []byte {
	t = t[2 : len(t)-1]
	var out []byte
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		switch t[i] {
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case '0':
			out = append(out, 0)
		case 'x':
			v, _ := strconv.ParseUint(t[i+1:i+3], 16, 8)
			out = append(out, byte(v))
			i += 2
		default:
			out = append(out, t[i])
		}
	}
	return out
}

// ─── Coercion to a declared type ───────────────────────────────────────

// coercer returns how a value bound to type t is converted, or nil when
// it is used as is: an integer literal (an Int value) meeting an Int(N) or
// UInt(N) declaration takes that type, as does an Int widened implicitly.
func coercer(in *Interp, t *Type) func(Value) Value {
	if t == nil {
		return nil
	}
	if it, ok := intTypeOf(t); ok {
		if it.Bits == 64 && it.Signed {
			return func(v Value) Value {
				if n, ok := v.(*Num); ok {
					return convertInt(n, it, true)
				}
				return v
			}
		}
		return func(v Value) Value {
			switch x := v.(type) {
			case int64:
				return convertInt(x, it, true)
			case *Num:
				if x.Bits != it.Bits || x.Signed != it.Signed {
					return convertInt(x, it, true)
				}
			}
			return v
		}
	}
	if t.Name == "Float" {
		return func(v Value) Value {
			if x, ok := v.(int64); ok {
				return float64(x)
			}
			return v
		}
	}
	if (t.Name == "List" || t.Name == "Option") && len(t.Args) >= 1 {
		inner := coercer(in, t.Args[0])
		if inner == nil {
			return nil
		}
		if _, ok := intTypeOf(t.Args[0]); !ok {
			return nil
		}
		return func(v Value) Value {
			switch x := v.(type) {
			case *ListV:
				// Lists stay homogeneous (adoptElems), so the first element
				// tells whether the whole list still needs converting.
				if x.n == 0 {
					return v
				}
				if inner(x.At(0)) == x.At(0) {
					return v
				}
				out := make([]Value, x.Len())
				for i, e := range x.Vals() {
					out[i] = inner(e)
				}
				return NewList(out)
			case *Var:
				if x.V == vSome {
					return Some(inner(x.P[0]))
				}
			}
			return v
		}
	}
	return nil
}
