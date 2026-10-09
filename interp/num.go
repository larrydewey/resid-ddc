package interp

import (
	"math/big"
	"math/bits"
)

// Num is an integer of a declared width other than Int (Int(64)), held
// exactly: V is always inside the type's range.
type Num struct {
	Bits   int
	Signed bool
	V      *big.Int
}

func (n *Num) Big() *big.Int { return n.V }

func NumFromInt64(b int, signed bool, x int64) *Num {
	return &Num{b, signed, big.NewInt(x)}
}

func rangeOf(b int, signed bool) (lo, hi *big.Int) {
	if signed {
		hi = new(big.Int).Lsh(big.NewInt(1), uint(b-1))
		lo = new(big.Int).Neg(hi)
		hi.Sub(hi, big.NewInt(1))
		return
	}
	hi = new(big.Int).Lsh(big.NewInt(1), uint(b))
	hi.Sub(hi, big.NewInt(1))
	return big.NewInt(0), hi
}

func inRange(v *big.Int, b int, signed bool) bool {
	lo, hi := rangeOf(b, signed)
	return v.Cmp(lo) >= 0 && v.Cmp(hi) <= 0
}

// wrap reduces v to the type's range modulo 2^b.
func wrap(v *big.Int, b int, signed bool) *big.Int {
	m := new(big.Int).Lsh(big.NewInt(1), uint(b))
	r := new(big.Int).Mod(v, m) // in [0, 2^b)
	if signed {
		half := new(big.Int).Lsh(big.NewInt(1), uint(b-1))
		if r.Cmp(half) >= 0 {
			r.Sub(r, m)
		}
	}
	return r
}

// mkInt builds a value of the given integer type, checked or wrapping.
func mkInt(v *big.Int, b int, signed bool, checked bool, what string) Value {
	if !inRange(v, b, signed) {
		if checked {
			abort(what)
		}
		v = wrap(v, b, signed)
	}
	if b == 64 && signed {
		return v.Int64()
	}
	return &Num{b, signed, v}
}

// IntType describes an integer type; Int is {64, true}.
type IntType struct {
	Bits   int
	Signed bool
}

func typeOfInt(v Value) (IntType, bool) {
	switch x := v.(type) {
	case int64:
		return IntType{64, true}, true
	case *Num:
		return IntType{x.Bits, x.Signed}, true
	}
	return IntType{}, false
}

func bigOf(v Value) *big.Int {
	switch x := v.(type) {
	case int64:
		return big.NewInt(x)
	case *Num:
		return x.V
	}
	abort("not an integer")
	return nil
}

// joinType: the type of a binary operation's result (spec §6.1): the
// wider operand; an Int beside another type is a literal that adopts it.
func joinType(a, b Value) IntType {
	ta, _ := typeOfInt(a)
	tb, _ := typeOfInt(b)
	if _, ok := a.(int64); ok {
		if _, ok2 := b.(int64); !ok2 {
			if tb.Signed && tb.Bits < 64 {
				return ta
			}
			return tb
		}
	}
	if _, ok := b.(int64); ok {
		if ta.Signed && ta.Bits < 64 {
			return tb
		}
		return ta
	}
	if tb.Bits > ta.Bits {
		return tb
	}
	return ta
}

const overflowMsg = "integer overflow in checked arithmetic"

func intArith(op string, a, b Value) Value {
	if x, ok := a.(int64); ok {
		if y, ok2 := b.(int64); ok2 {
			return int64Arith(op, x, y)
		}
	}
	t := joinType(a, b)
	x, y := bigOf(a), bigOf(b)
	r := new(big.Int)
	switch op {
	case "+":
		r.Add(x, y)
	case "-":
		r.Sub(x, y)
	case "*":
		r.Mul(x, y)
	case "/":
		if y.Sign() == 0 {
			abort("integer division by zero")
		}
		r.Quo(x, y)
	case "%":
		if y.Sign() == 0 {
			abort("integer division by zero")
		}
		r.Rem(x, y)
	case "&", "|", "^":
		ux, uy := toUnsigned(x, t.Bits), toUnsigned(y, t.Bits)
		switch op {
		case "&":
			r.And(ux, uy)
		case "|":
			r.Or(ux, uy)
		case "^":
			r.Xor(ux, uy)
		}
		return mkInt(wrap(r, t.Bits, t.Signed), t.Bits, t.Signed, false, "")
	}
	return mkInt(r, t.Bits, t.Signed, true, overflowMsg)
}

func toUnsigned(x *big.Int, b int) *big.Int {
	if x.Sign() >= 0 {
		return x
	}
	return wrap(x, b, false)
}

func int64Arith(op string, x, y int64) Value {
	switch op {
	case "+":
		r, c := bits.Add64(uint64(x), uint64(y), 0)
		_ = c
		if (x >= 0) == (y >= 0) && (int64(r) >= 0) != (x >= 0) {
			abort(overflowMsg)
		}
		return int64(r)
	case "-":
		r := x - y
		if (x >= 0) != (y >= 0) && (r >= 0) != (x >= 0) {
			abort(overflowMsg)
		}
		return r
	case "*":
		if x == 0 || y == 0 {
			return int64(0)
		}
		r := x * y
		if r/y != x || (x == -1 && y == -1<<63) || (y == -1 && x == -1<<63) {
			abort(overflowMsg)
		}
		return r
	case "/":
		if y == 0 {
			abort("integer division by zero")
		}
		if x == -1<<63 && y == -1 {
			abort(overflowMsg)
		}
		return x / y
	case "%":
		if y == 0 {
			abort("integer division by zero")
		}
		if y == -1 {
			return int64(0)
		}
		return x % y
	case "&":
		return x & y
	case "|":
		return x | y
	case "^":
		return x ^ y
	}
	abort("bad operator " + op)
	return nil
}

// Shifts keep the left operand's type; >> is arithmetic for signed types
// and logical for unsigned ones; << drops the bits shifted out.
func intShift(op string, a, b Value) Value {
	n := int(bigOf(b).Int64())
	if x, ok := a.(int64); ok {
		if n < 0 || n >= 64 {
			if op == ">>" && x < 0 {
				return int64(-1)
			}
			return int64(0)
		}
		if op == "<<" {
			return x << uint(n)
		}
		return x >> uint(n)
	}
	t, _ := typeOfInt(a)
	x := bigOf(a)
	r := new(big.Int)
	if op == "<<" {
		r.Lsh(x, uint(n))
		return mkInt(wrap(r, t.Bits, t.Signed), t.Bits, t.Signed, false, "")
	}
	r.Rsh(x, uint(n)) // floor division: arithmetic for negative signed values
	return mkInt(r, t.Bits, t.Signed, false, "")
}

func intNot(a Value) Value {
	if x, ok := a.(int64); ok {
		return ^x
	}
	t, _ := typeOfInt(a)
	r := new(big.Int).Not(bigOf(a))
	return mkInt(wrap(r, t.Bits, t.Signed), t.Bits, t.Signed, false, "")
}

func intNeg(a Value) Value {
	if x, ok := a.(int64); ok {
		if x == -1<<63 {
			abort(overflowMsg)
		}
		return -x
	}
	t, _ := typeOfInt(a)
	return mkInt(new(big.Int).Neg(bigOf(a)), t.Bits, t.Signed, true, overflowMsg)
}

// convert: (T) x for an integer target, checked.
func convertInt(v Value, t IntType, checked bool) Value {
	var x *big.Int
	switch y := v.(type) {
	case int64, *Num:
		x = bigOf(v)
	case float64:
		if checked && (y != y) {
			abort("conversion out of range")
		}
		bf := new(big.Float).SetFloat64(y)
		x, _ = bf.Int(nil)
	case bool:
		if y {
			x = big.NewInt(1)
		} else {
			x = big.NewInt(0)
		}
	default:
		abort("cannot convert to an integer")
	}
	return mkInt(x, t.Bits, t.Signed, checked, "conversion out of range")
}

// wrapArith: wrapping_add / wrapping_sub / wrapping_mul at the operands'
// type (the u-variants are the same operation on unsigned types).
func wrapArith(op string, a, b Value) Value {
	t := joinType(a, b)
	x, y := bigOf(a), bigOf(b)
	r := new(big.Int)
	switch op {
	case "add":
		r.Add(x, y)
	case "sub":
		r.Sub(x, y)
	case "mul":
		r.Mul(x, y)
	case "div":
		if y.Sign() == 0 {
			abort("integer division by zero")
		}
		r.Quo(x, y)
	}
	return mkInt(wrap(r, t.Bits, t.Signed), t.Bits, t.Signed, false, "")
}

func satArith(op string, a, b Value) Value {
	t := joinType(a, b)
	x, y := bigOf(a), bigOf(b)
	r := new(big.Int)
	switch op {
	case "add":
		r.Add(x, y)
	case "sub":
		r.Sub(x, y)
	case "mul":
		r.Mul(x, y)
	}
	lo, hi := rangeOf(t.Bits, t.Signed)
	if r.Cmp(lo) < 0 {
		r = lo
	}
	if r.Cmp(hi) > 0 {
		r = hi
	}
	return mkInt(r, t.Bits, t.Signed, false, "")
}

// intType parses Int, UInt, Int(N), UInt(N), ISize, USize.
func intTypeOf(t *Type) (IntType, bool) {
	if t == nil {
		return IntType{}, false
	}
	switch t.Name {
	case "Int", "ISize":
		if t.N != 0 {
			return IntType{t.N, true}, true
		}
		return IntType{64, true}, true
	case "UInt", "USize":
		if t.N != 0 {
			return IntType{t.N, false}, true
		}
		return IntType{64, false}, true
	}
	return IntType{}, false
}

func intText(v Value) string {
	switch x := v.(type) {
	case int64:
		return itoa64(x)
	case *Num:
		return x.V.String()
	}
	return "?"
}
