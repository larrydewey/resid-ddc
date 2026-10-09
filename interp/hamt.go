package interp

import (
	"math/bits"
	"strconv"
)

// MapV is an immutable Map or Set (a set's values are true): a persistent
// 32-way hash trie on the key's 64-bit hash, lowest 5-bit chunk first.
// Walking it in slot order gives exactly the Resid runtime's canonical
// order (runtime/rt/map.resid): ascending by the hash's 5-bit chunks,
// lowest chunk most significant, equal full hashes in insertion order.

const fnvOff = 0xcbf29ce484222325
const fnvPrime = 1099511628211

func fnvBytes(h uint64, b []byte) uint64 {
	for _, c := range b {
		if c == 0 {
			break
		}
		h = (h ^ uint64(c)) * fnvPrime
	}
	return h
}

func fnvStr(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 {
			break
		}
		h = (h ^ uint64(c)) * fnvPrime
	}
	return h
}

// Hash of a key as the runtime computes it.
func hashKey(k Value) uint64 {
	switch x := k.(type) {
	case int64:
		var buf [24]byte
		return fnvBytes(fnvOff, strconv.AppendInt(buf[:0], x, 10))
	case *StrV:
		if !x.hashed {
			x.h = fnvStr(fnvOff, x.s)
			x.hashed = true
		}
		return x.h
	case bool:
		if x {
			return fnvBytes(fnvOff, []byte("t"))
		}
		return fnvBytes(fnvOff, []byte("f"))
	case float64:
		return fnvBytes(fnvOff, []byte(strconv.FormatFloat(x, 'g', 17, 64)))
	case *Num:
		// A boxed wide integer hashes its low 64-bit word as decimal.
		lo := wrap(x.V, 64, true).Int64()
		return fnvBytes(fnvOff, []byte(strconv.FormatInt(lo, 10)))
	}
	abort("this key type is not supported by the interpreter's maps")
	return 0
}

type mentry struct {
	h    uint64
	k, v Value
}

type mnode struct {
	bitmap uint32    // slots that hold something
	leaf   uint32    // of those, slots holding a single entry
	kids   []any     // per set bit: *mentry or *mnode
	coll   []*mentry // a collision bucket (all equal hashes), at max depth
}

type MapV struct {
	root *mnode
	n    int
}

var emptyMap = &MapV{root: &mnode{}}

func NewMap() *MapV { return emptyMap }

func (m *MapV) Len() int { return m.n }

func chunk(h uint64, level int) uint32 { return uint32((h >> (5 * uint(level))) & 31) }

func (m *MapV) Get(k Value) (Value, bool) {
	h := hashKey(k)
	n := m.root
	for level := 0; ; level++ {
		if n.coll != nil {
			for _, e := range n.coll {
				if Equal(e.k, k) {
					return e.v, true
				}
			}
			return nil, false
		}
		c := chunk(h, level)
		bit := uint32(1) << c
		if n.bitmap&bit == 0 {
			return nil, false
		}
		idx := bits.OnesCount32(n.bitmap & (bit - 1))
		if n.leaf&bit != 0 {
			e := n.kids[idx].(*mentry)
			if e.h == h && Equal(e.k, k) {
				return e.v, true
			}
			return nil, false
		}
		n = n.kids[idx].(*mnode)
	}
}

func (m *MapV) Insert(k, v Value) *MapV {
	h := hashKey(k)
	root, added := insertAt(m.root, &mentry{h, k, v}, 0)
	n := m.n
	if added {
		n++
	}
	return &MapV{root, n}
}

func insertAt(n *mnode, e *mentry, level int) (*mnode, bool) {
	if n.coll != nil || level > 12 {
		c := &mnode{coll: append([]*mentry(nil), n.coll...)}
		for i, x := range c.coll {
			if Equal(x.k, e.k) {
				c.coll[i] = e
				return c, false
			}
		}
		c.coll = append(c.coll, e)
		return c, true
	}
	c := chunk(e.h, level)
	bit := uint32(1) << c
	idx := bits.OnesCount32(n.bitmap & (bit - 1))
	cp := &mnode{bitmap: n.bitmap, leaf: n.leaf, kids: append([]any(nil), n.kids...)}
	if n.bitmap&bit == 0 {
		cp.bitmap |= bit
		cp.leaf |= bit
		cp.kids = append(cp.kids, nil)
		copy(cp.kids[idx+1:], cp.kids[idx:])
		cp.kids[idx] = e
		return cp, true
	}
	if n.leaf&bit != 0 {
		old := n.kids[idx].(*mentry)
		if old.h == e.h && Equal(old.k, e.k) {
			cp.kids[idx] = e
			return cp, false
		}
		// split into a subnode holding both (the older first)
		sub := &mnode{}
		if level+1 > 12 {
			sub = &mnode{coll: []*mentry{old}}
		} else {
			sub, _ = insertAt(sub, old, level+1)
		}
		sub, _ = insertAt(sub, e, level+1)
		cp.leaf &^= bit
		cp.kids[idx] = sub
		return cp, true
	}
	sub, added := insertAt(n.kids[idx].(*mnode), e, level+1)
	cp.kids[idx] = sub
	return cp, added
}

func (m *MapV) Remove(k Value) *MapV {
	h := hashKey(k)
	root, removed := removeAt(m.root, h, k, 0)
	if !removed {
		return m
	}
	return &MapV{root, m.n - 1}
}

func removeAt(n *mnode, h uint64, k Value, level int) (*mnode, bool) {
	if n.coll != nil {
		for i, x := range n.coll {
			if Equal(x.k, k) {
				c := &mnode{coll: append(append([]*mentry(nil), n.coll[:i]...), n.coll[i+1:]...)}
				return c, true
			}
		}
		return n, false
	}
	c := chunk(h, level)
	bit := uint32(1) << c
	if n.bitmap&bit == 0 {
		return n, false
	}
	idx := bits.OnesCount32(n.bitmap & (bit - 1))
	cp := &mnode{bitmap: n.bitmap, leaf: n.leaf, kids: append([]any(nil), n.kids...)}
	if n.leaf&bit != 0 {
		e := n.kids[idx].(*mentry)
		if e.h != h || !Equal(e.k, k) {
			return n, false
		}
		cp.bitmap &^= bit
		cp.leaf &^= bit
		cp.kids = append(cp.kids[:idx], cp.kids[idx+1:]...)
		return cp, true
	}
	sub, removed := removeAt(n.kids[idx].(*mnode), h, k, level+1)
	if !removed {
		return n, false
	}
	cp.kids[idx] = sub
	return cp, true
}

// Each visits entries in canonical order.
func (m *MapV) Each(f func(k, v Value)) { eachNode(m.root, f) }

func eachNode(n *mnode, f func(k, v Value)) {
	if n.coll != nil {
		for _, e := range n.coll {
			f(e.k, e.v)
		}
		return
	}
	idx := 0
	for c := uint32(0); c < 32; c++ {
		bit := uint32(1) << c
		if n.bitmap&bit == 0 {
			continue
		}
		if n.leaf&bit != 0 {
			e := n.kids[idx].(*mentry)
			f(e.k, e.v)
		} else {
			eachNode(n.kids[idx].(*mnode), f)
		}
		idx++
	}
}

func (m *MapV) Keys() []Value {
	out := make([]Value, 0, m.n)
	m.Each(func(k, v Value) { out = append(out, k) })
	return out
}

func (m *MapV) Values() []Value {
	out := make([]Value, 0, m.n)
	m.Each(func(k, v Value) { out = append(out, v) })
	return out
}
