package interp

import (
	"math/big"
	"strings"
	"unicode/utf8"
	"unsafe"
)

// Value is one of:
//
//	int64        Int (Int(64))
//	*Num         any other integer type (Int(N), UInt(N))
//	float64      Float
//	bool         Bool
//	*StrV        Str
//	*ListV       List(T)
//	*MapV        Map(K, V) and Set(T)
//	*Rec         a record
//	*Var         a sum value (Option, Result and declared sums)
//	*StrBufV, *ListBufV   builders
//	*Closure     a lambda or function value
//	nil          no value (Void)
type Value = any

// ─── Abort ─────────────────────────────────────────────────────────────

type Abort struct{ Msg string }

func abort(msg string) { panic(&Abort{msg}) }

// ─── Strings ───────────────────────────────────────────────────────────

// StrV is a string indexed by codepoint, as Resid's str_* builtins are.
type StrV struct {
	s      string
	r      []rune
	ascii  int8 // 0 unknown, 1 ascii, -1 not
	hashed bool
	h      uint64
	back   *sbacking // set when s is a prefix of a growable buffer
}

// sbacking: the bytes of strings built by +; the string that ends at
// len(buf) may append in place (older strings view shorter prefixes).
type sbacking struct{ buf []byte }

// strConcat is a + b, linear over a loop that keeps appending to its
// newest result, as the native compiler's string accumulator is.
func strConcat(a, b *StrV) *StrV {
	if len(b.s) == 0 {
		return a
	}
	if len(a.s) == 0 {
		return b
	}
	n := len(a.s) + len(b.s)
	if n < 256 {
		return &StrV{s: a.s + b.s}
	}
	bk := a.back
	if bk == nil || len(bk.buf) != len(a.s) {
		buf := make([]byte, len(a.s), 2*n)
		copy(buf, a.s)
		bk = &sbacking{buf}
	}
	bk.buf = append(bk.buf, b.s...)
	return &StrV{s: unsafe.String(&bk.buf[0], len(bk.buf)), back: bk}
}

var emptyStr = &StrV{s: "", ascii: 1}

func S(s string) *StrV {
	if s == "" {
		return emptyStr
	}
	return &StrV{s: s}
}

func (v *StrV) isASCII() bool {
	if v.ascii == 0 {
		v.ascii = 1
		for i := 0; i < len(v.s); i++ {
			if v.s[i] >= 0x80 {
				v.ascii = -1
				break
			}
		}
	}
	return v.ascii == 1
}

func (v *StrV) runes() []rune {
	if v.r == nil {
		v.r = []rune(v.s)
	}
	return v.r
}

func (v *StrV) Len() int {
	if v.isASCII() {
		return len(v.s)
	}
	return len(v.runes())
}

func (v *StrV) At(i int) int64 {
	if v.isASCII() {
		if i < 0 || i >= len(v.s) {
			return -1
		}
		return int64(v.s[i])
	}
	r := v.runes()
	if i < 0 || i >= len(r) {
		return -1
	}
	return int64(r[i])
}

// Slice of codepoints [a, b), clamped.
func (v *StrV) Slice(a, b int) *StrV {
	n := v.Len()
	if a < 0 {
		a = 0
	}
	if b > n {
		b = n
	}
	if a >= b {
		return emptyStr
	}
	if v.isASCII() {
		return &StrV{s: v.s[a:b], ascii: 1}
	}
	return &StrV{s: string(v.runes()[a:b])}
}

func (v *StrV) String() string { return v.s }

// ─── Lists ─────────────────────────────────────────────────────────────

// A list is a prefix [0, n) of a shared backing slice. Appending to the
// newest list of a backing extends the backing in place; every older list
// keeps seeing only its own prefix, so values stay immutable while the
// compiler's `acc.concat([x])` loops stay linear.
type lbacking struct{ vals []Value }

type ListV struct {
	b *lbacking
	n int
}

func NewList(vals []Value) *ListV { return &ListV{&lbacking{vals}, len(vals)} }

func (l *ListV) Len() int       { return l.n }
func (l *ListV) At(i int) Value { return l.b.vals[i] }
func (l *ListV) Vals() []Value  { return l.b.vals[:l.n] }

func (l *ListV) Concat(xs []Value) *ListV {
	if len(xs) == 0 {
		return l
	}
	if l.n == len(l.b.vals) {
		l.b.vals = append(l.b.vals, xs...)
		return &ListV{l.b, l.n + len(xs)}
	}
	nv := make([]Value, l.n, l.n+len(xs)+8)
	copy(nv, l.b.vals[:l.n])
	nv = append(nv, xs...)
	return &ListV{&lbacking{nv}, len(nv)}
}

func (l *ListV) With(i int, x Value) *ListV {
	nv := make([]Value, l.n)
	copy(nv, l.b.vals[:l.n])
	nv[i] = x
	return NewList(nv)
}

func (l *ListV) Slice(a, b int) *ListV {
	nv := make([]Value, b-a)
	copy(nv, l.b.vals[a:b])
	return NewList(nv)
}

// ─── Records, sums, builders, closures ─────────────────────────────────

type Rec struct {
	T *TypeDecl
	F []Value
}

type Var struct {
	V *Variant
	P []Value
}

type StrBufV struct{ b strings.Builder }

type ListBufV struct{ vals []Value }

type Closure struct {
	Fn     *cfunc
	Env    []Value
	Native func(args []Value) Value
}

// Builtin sums.
var (
	optionT = &TypeDecl{Name: "Option", Kind: "sum"}
	resultT = &TypeDecl{Name: "Result", Kind: "sum"}
	vSome   = &Variant{Name: "Some", Sum: optionT}
	vNone   = &Variant{Name: "None", Sum: optionT}
	vOk     = &Variant{Name: "Ok", Sum: resultT}
	vErr    = &Variant{Name: "Err", Sum: resultT}
	noneV   = &Var{V: vNone}
)

func Some(x Value) *Var { return &Var{V: vSome, P: []Value{x}} }

// ─── Equality and order ────────────────────────────────────────────────

func Equal(a, b Value) bool {
	switch x := a.(type) {
	case int64:
		switch y := b.(type) {
		case int64:
			return x == y
		case *Num:
			return y.Big().Cmp(big.NewInt(x)) == 0
		}
	case *Num:
		switch y := b.(type) {
		case int64:
			return x.Big().Cmp(big.NewInt(y)) == 0
		case *Num:
			return x.Big().Cmp(y.Big()) == 0
		}
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case *StrV:
		y, ok := b.(*StrV)
		return ok && x.s == y.s
	case *ListV:
		y, ok := b.(*ListV)
		if !ok || x.n != y.n {
			return false
		}
		for i := 0; i < x.n; i++ {
			if !Equal(x.At(i), y.At(i)) {
				return false
			}
		}
		return true
	case *Rec:
		y, ok := b.(*Rec)
		if !ok || x.T != y.T {
			return false
		}
		for i := range x.F {
			if !Equal(x.F[i], y.F[i]) {
				return false
			}
		}
		return true
	case *Var:
		y, ok := b.(*Var)
		if !ok || x.V.Name != y.V.Name || len(x.P) != len(y.P) {
			return false
		}
		for i := range x.P {
			if !Equal(x.P[i], y.P[i]) {
				return false
			}
		}
		return true
	case *MapV:
		y, ok := b.(*MapV)
		if !ok || x.Len() != y.Len() {
			return false
		}
		eq := true
		x.Each(func(k, v Value) {
			w, has := y.Get(k)
			if !has || !Equal(v, w) {
				eq = false
			}
		})
		return eq
	case nil:
		return b == nil
	}
	return false
}

// Compare for Ord: -1, 0, 1.
func Compare(a, b Value) int {
	switch x := a.(type) {
	case int64:
		if y, ok := b.(int64); ok {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}
		return big.NewInt(x).Cmp(toNum(b).Big())
	case *Num:
		return x.Big().Cmp(toNum(b).Big())
	case float64:
		y := b.(float64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case *StrV:
		return strings.Compare(x.s, b.(*StrV).s)
	case bool:
		y := b.(bool)
		if x == y {
			return 0
		}
		if !x {
			return -1
		}
		return 1
	}
	abort("values are not ordered")
	return 0
}

func toNum(v Value) *Num {
	switch x := v.(type) {
	case *Num:
		return x
	case int64:
		return NumFromInt64(64, true, x)
	}
	abort("not an integer")
	return nil
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
