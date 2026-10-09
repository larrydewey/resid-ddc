package interp

import (
	"bufio"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

type bigInt = big.Int
type bigFloat = big.Float

func itoa64(x int64) string { return strconv.FormatInt(x, 10) }

type outBuf struct {
	out, err *bufio.Writer
}

func (o *outBuf) flush() {
	o.out.Flush()
	o.err.Flush()
}

// exitNow ends the program from inside the evaluator.
type exitNow struct{ code int }

type Options struct{ StopAtExec bool }

// Run interprets the program's main with argv (argv[0] is the program).
func Run(p *Program, args []string, o Options) (code int) {
	in := &Interp{prog: p, fns: map[*FnDecl]*cfunc{}, opts: o, args: args,
		out:   &outBuf{bufio.NewWriterSize(os.Stdout, 1<<16), bufio.NewWriterSize(os.Stderr, 1<<12)},
		flags: map[string]bool{}, cur: &stackSeg{vals: make([]Value, 1<<16)}, rawmem: map[int64]byte{}, globals: map[string]int64{}, nextGlb: 1 << 20}
	defer in.out.flush()
	if os.Getenv("RESIDI_COUNT") != "" {
		defer in.report()
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		go func() {
			<-ch
			in.report()
			os.Exit(130)
		}()
	}
	if os.Getenv("RESIDI_RPROF") != "" {
		self, incl := map[string]int{}, map[string]int{}
		go func() {
			for range time.Tick(time.Millisecond) {
				d := in.depth
				if d <= 0 || d >= len(in.frames) {
					continue
				}
				if f := in.frames[d].fn; f != nil {
					self[f.name]++
				}
				seen := map[string]bool{}
				for i := 1; i <= d && i < len(in.frames); i++ {
					if f := in.frames[i].fn; f != nil && !seen[f.name] {
						seen[f.name] = true
						incl[f.name]++
					}
				}
			}
		}()
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		go func() {
			<-ch
			top := func(m map[string]int, what string) {
				type kv struct {
					k string
					v int
				}
				var xs []kv
				for k, v := range m {
					xs = append(xs, kv{k, v})
				}
				sort.Slice(xs, func(i, j int) bool { return xs[i].v > xs[j].v })
				fmt.Fprintln(os.Stderr, "--", what)
				for i := 0; i < len(xs) && i < 40; i++ {
					fmt.Fprintf(os.Stderr, "%8d %s\n", xs[i].v, xs[i].k)
				}
			}
			top(self, "self")
			top(incl, "inclusive")
			os.Exit(130)
		}()
	}
	debug.SetGCPercent(400)
	mainFn := p.Main.Fns["main"]
	if mainFn == nil {
		fmt.Fprintln(os.Stderr, "residi: no main")
		return 2
	}
	defer func() {
		if r := recover(); r != nil {
			switch x := r.(type) {
			case *exitNow:
				code = x.code
			case *Abort:
				in.out.out.Flush()
				fmt.Fprintf(in.out.err, "resid: abort: %s\n", x.Msg)
				code = 134
			default:
				panic(r)
			}
		}
	}()
	r := in.call(in.cfuncOf(mainFn), nil, nil)
	switch v := r.(type) {
	case int64:
		return int(v)
	}
	return 0
}

// CheckAll compiles every function and reports those that fail.
func CheckAll(p *Program) []string {
	in := &Interp{prog: p, fns: map[*FnDecl]*cfunc{}, flags: map[string]bool{}, cur: &stackSeg{vals: make([]Value, 1<<16)}}
	var errs []string
	for _, m := range p.Mods {
		for _, d := range m.Fns {
			if d.Link {
				continue
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						errs = append(errs, fmt.Sprintf("%s: %s: %v", m.Path, d.Name, r))
					}
				}()
				in.ensure(in.cfuncOf(d))
			}()
		}
	}
	sort.Strings(errs)
	return errs
}

// ─── Iteration, indexing, slicing ──────────────────────────────────────

func iterValues(v Value) []Value {
	switch x := v.(type) {
	case *ListV:
		return x.Vals()
	case *MapV:
		return x.Keys()
	case *StrV:
		r := []rune(x.s)
		out := make([]Value, len(r))
		for i, c := range r {
			out[i] = S(string(c))
		}
		return out
	}
	abort(fmt.Sprintf("cannot iterate over %T", v))
	return nil
}

func index(o, i Value) Value {
	switch x := o.(type) {
	case *ListV:
		k, ok := i.(int64)
		if !ok {
			k = bigOf(i).Int64()
		}
		if k < 0 || k >= int64(x.n) {
			abort(fmt.Sprintf("index %d out of bounds for a list of length %d", k, x.n))
		}
		return x.At(int(k))
	case *MapV:
		v, ok := x.Get(i)
		if !ok {
			abort("key not found")
		}
		return v
	case *StrV:
		return x.At(int(i.(int64)))
	}
	abort(fmt.Sprintf("cannot index %T", o))
	return nil
}

func sliceOf(o Value, a, b int64, hasHi bool) Value {
	switch x := o.(type) {
	case *ListV:
		if !hasHi {
			b = int64(x.n)
		}
		if a < 0 || b > int64(x.n) || a > b {
			abort(fmt.Sprintf("slice [%d..%d] out of bounds for a list of length %d", a, b, x.n))
		}
		return x.Slice(int(a), int(b))
	case *StrV:
		if !hasHi {
			b = int64(x.Len())
		}
		return x.Slice(int(a), int(b))
	}
	abort(fmt.Sprintf("cannot slice %T", o))
	return nil
}

// ─── Showing values (f-string holes) ───────────────────────────────────

func show(v Value) string {
	switch x := v.(type) {
	case int64:
		return itoa64(x)
	case *Num:
		return x.V.String()
	case *StrV:
		return x.s
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return showFloat(x)
	case *ListV:
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range x.Vals() {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(showNested(e))
		}
		b.WriteByte(']')
		return b.String()
	case *Var:
		if len(x.P) == 0 {
			return x.V.Name
		}
		var b strings.Builder
		b.WriteString(x.V.Name + "(")
		for i, e := range x.P {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(showNested(e))
		}
		b.WriteByte(')')
		return b.String()
	}
	abort(fmt.Sprintf("cannot show a %T", v))
	return ""
}

func showNested(v Value) string {
	if s, ok := v.(*StrV); ok {
		return strconv.Quote(s.s)
	}
	return show(v)
}

func showFloat(x float64) string { return floatShort(x) }

func (c *cctx) fstr(x *EFStr) evalFn {
	holes := c.exprs(x.Holes)
	parts := make([]string, len(x.Parts))
	for i, p := range x.Parts {
		parts[i] = unescapeBody(p)
	}
	specs := x.Specs
	return func(fr *Frame) Value {
		var b strings.Builder
		for i, p := range parts {
			b.WriteString(p)
			if i < len(holes) {
				v := holes[i](fr)
				if i < len(specs) && specs[i] != "" {
					b.WriteString(fmtSpec(v, specs[i]))
				} else {
					b.WriteString(show(v))
				}
			}
		}
		return S(b.String())
	}
}

func fmtSpec(v Value, spec string) string {
	abort("f-string spec {:" + spec + "} not supported by the interpreter")
	return ""
}

// ─── Methods ───────────────────────────────────────────────────────────

func (c *cctx) mcall(x *EMCall) evalFn {
	// A provider verb: the family name is not shadowed by a local.
	if r, ok := x.Recv.(*ERef); ok {
		if _, local := c.lookup(r.Name); !local {
			switch r.Name {
			case "filesystem", "process", "environment", "args", "clock":
				args := c.exprs(x.Args)
				fam, verb, in := r.Name, x.Name, c.in
				return func(fr *Frame) Value { return in.provider(fam, verb, evalAll(args, fr)) }
			}
		}
	}
	recv := c.expr(x.Recv)
	args := c.exprs(x.Args)
	name := x.Name
	in := c.in
	switch name {
	case "concat":
		a := args[0]
		return func(fr *Frame) Value {
			r := recv(fr)
			v := a(fr)
			switch l := r.(type) {
			case *ListV:
				return l.Concat(adoptElems(l, v.(*ListV).Vals()))
			case *StrV:
				return strConcat(l, v.(*StrV))
			}
			abort(fmt.Sprintf("concat on a %T", r))
			return nil
		}
	case "len":
		return func(fr *Frame) Value {
			switch l := recv(fr).(type) {
			case *ListV:
				return int64(l.n)
			case *MapV:
				return int64(l.Len())
			case *StrV:
				return int64(l.Len())
			case *ListBufV:
				return int64(len(l.vals))
			case *StrBufV:
				return int64(runeLen(l.b.String()))
			}
			abort("len of an unknown value")
			return nil
		}
	}
	return func(fr *Frame) Value { return in.method(recv(fr), name, evalAll(args, fr)) }
}

func (in *Interp) method(r Value, name string, a []Value) Value {
	switch x := r.(type) {
	case *StrBufV:
		switch name {
		case "push":
			x.b.WriteString(a[0].(*StrV).s)
			return x
		case "push_char":
			x.b.WriteRune(rune(a[0].(int64)))
			return x
		case "finish":
			return S(x.b.String())
		}
	case *ListBufV:
		switch name {
		case "push":
			v := a[0]
			if len(x.vals) > 0 {
				v = adopt(x.vals[len(x.vals)-1], v)
			}
			x.vals = append(x.vals, v)
			return x
		case "finish":
			return NewList(x.vals)
		}
	case *ListV:
		switch name {
		case "get":
			k := a[0].(int64)
			if k < 0 || k >= int64(x.n) {
				return noneV
			}
			return Some(x.At(int(k)))
		case "with":
			k := a[0].(int64)
			if k < 0 || k >= int64(x.n) {
				abort("index out of bounds in with")
			}
			return x.With(int(k), adopt(x.At(int(k)), a[1]))
		case "contains":
			for _, e := range x.Vals() {
				if Equal(e, a[0]) {
					return true
				}
			}
			return false
		case "reverse":
			vs := x.Vals()
			out := make([]Value, len(vs))
			for i, e := range vs {
				out[len(vs)-1-i] = e
			}
			return NewList(out)
		case "sum":
			var s Value = int64(0)
			for _, e := range x.Vals() {
				s = arith2("+", s, e)
			}
			return s
		case "to_list":
			return x
		}
	case *MapV:
		switch name {
		case "get":
			v, ok := x.Get(a[0])
			if !ok {
				return noneV
			}
			return Some(v)
		case "insert":
			if len(a) == 1 {
				return x.Insert(a[0], true)
			}
			return x.Insert(a[0], a[1])
		case "contains":
			_, ok := x.Get(a[0])
			return ok
		case "remove":
			return x.Remove(a[0])
		case "keys":
			return NewList(x.Keys())
		case "values":
			return NewList(x.Values())
		case "to_list":
			return NewList(x.Keys())
		}
	case *StrV:
		switch name {
		case "contains":
			return strings.Contains(x.s, a[0].(*StrV).s)
		}
	}
	abort(fmt.Sprintf("method %s on a %T is not supported by the interpreter", name, r))
	return nil
}

func arith2(op string, a, b Value) Value {
	if op == "+" {
		if x, ok := a.(float64); ok {
			return x + b.(float64)
		}
		if x, ok := b.(float64); ok {
			if a == int64(0) {
				return x
			}
		}
		return intArith("+", a, b)
	}
	return arith(op, a, b)
}

// ─── Providers ─────────────────────────────────────────────────────────

func (in *Interp) provider(fam, verb string, a []Value) Value {
	str := func(i int) string { return a[i].(*StrV).s }
	switch fam + "." + verb {
	case "args.count":
		return int64(len(in.args))
	case "args.get":
		i := a[0].(int64)
		if i < 0 || i >= int64(len(in.args)) {
			return S("")
		}
		return S(in.args[i])
	case "environment.get":
		return S(os.Getenv(str(0)))
	case "environment.has":
		_, ok := os.LookupEnv(str(0))
		return ok
	case "filesystem.exists":
		if str(0) == "" {
			return false
		}
		_, err := os.Stat(str(0))
		return err == nil
	case "filesystem.is_dir":
		st, err := os.Stat(str(0))
		return err == nil && st.IsDir()
	case "filesystem.read_all":
		b, err := os.ReadFile(str(0))
		if err != nil {
			return S("")
		}
		// A C string: the text ends at the first NUL.
		if i := strings.IndexByte(string(b), 0); i >= 0 {
			b = b[:i]
		}
		return S(string(b))
	case "filesystem.read_bytes":
		b, err := os.ReadFile(str(0))
		if err != nil {
			return NewList(nil)
		}
		return bytesList(b)
	case "filesystem.write_all":
		return os.WriteFile(str(0), []byte(str(1)), 0o666) == nil
	case "filesystem.write_bytes":
		return os.WriteFile(str(0), listBytes(a[1]), 0o666) == nil
	case "filesystem.write_secret":
		return os.WriteFile(str(0), listBytes(a[1]), 0o600) == nil
	case "filesystem.append_bytes":
		return appendFile(str(0), listBytes(a[1]))
	case "filesystem.write_hex", "filesystem.append_hex":
		b, err := hex.DecodeString(str(1))
		if err != nil {
			return false
		}
		if verb == "write_hex" {
			return os.WriteFile(str(0), b, 0o666) == nil
		}
		return appendFile(str(0), b)
	case "filesystem.create_dir":
		return os.MkdirAll(str(0), 0o777) == nil
	case "filesystem.list_dir":
		es, err := os.ReadDir(str(0))
		if err != nil {
			return NewList(nil)
		}
		// getdents order is the file system's; sorted here (callers sort).
		var out []Value
		for _, e := range es {
			out = append(out, S(e.Name()))
		}
		return NewList(out)
	case "filesystem.sha256":
		b, err := os.ReadFile(str(0))
		if err != nil {
			return S("")
		}
		h := sha256.Sum256(b)
		return S(hex.EncodeToString(h[:]))
	case "process.run":
		return in.run(str(0))
	}
	abort("provider verb " + fam + "." + verb + " is not supported by the interpreter")
	return nil
}

func appendFile(path string, b []byte) bool {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return false
	}
	_, err = f.Write(b)
	return f.Close() == nil && err == nil
}

// run: fork and exec on the space-split words, as the runtime does. With
// StopAtExec, the first clang command ends the program successfully: the
// compiler has written its IR, and linking is out of scope.
func (in *Interp) run(cmd string) Value {
	words := strings.Fields(cmd)
	if len(words) == 0 {
		return int64(-1)
	}
	if in.opts.StopAtExec && strings.HasPrefix(words[0], "clang") {
		in.out.flush()
		panic(&exitNow{0})
	}
	in.out.flush()
	c := exec.Command(words[0], words[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return int64(ee.ExitCode())
		}
		return int64(-1)
	}
	return int64(0)
}

func bytesList(b []byte) *ListV {
	vs := make([]Value, len(b))
	for i, c := range b {
		vs[i] = int64(c)
	}
	return NewList(vs)
}

func listBytes(v Value) []byte {
	l := v.(*ListV)
	out := make([]byte, l.n)
	for i, e := range l.Vals() {
		out[i] = byte(bigOf(e).Int64())
	}
	return out
}

// ─── Builtin functions ─────────────────────────────────────────────────

type builtinFn func([]Value) Value

func sArg(a []Value, i int) string { return a[i].(*StrV).s }
func iArg(a []Value, i int) int64 {
	if x, ok := a[i].(int64); ok {
		return x
	}
	return bigOf(a[i]).Int64()
}

func (in *Interp) builtin(name string, argc int) builtinFn {
	switch name {
	case "println", "print", "eprintln", "eprint":
		return func(a []Value) Value {
			s := sArg(a, 0)
			w := in.out.out
			if strings.HasPrefix(name, "e") {
				in.out.out.Flush()
				w = in.out.err
			}
			w.WriteString(s)
			if strings.HasSuffix(name, "ln") {
				w.WriteByte('\n')
			}
			if w == in.out.err {
				w.Flush()
			}
			return true
		}
	case "print_bytes":
		return func(a []Value) Value { in.out.out.Write(listBytes(a[0])); return true }
	case "str_len":
		return func(a []Value) Value { return int64(a[0].(*StrV).Len()) }
	case "str_char_at":
		return func(a []Value) Value { return a[0].(*StrV).At(int(iArg(a, 1))) }
	case "str_from_code":
		return func(a []Value) Value { return S(string(rune(iArg(a, 0)))) }
	case "str_slice":
		return func(a []Value) Value {
			return a[0].(*StrV).Slice(clampInt(iArg(a, 1)), clampInt(iArg(a, 2)))
		}
	case "str_sb_new":
		return func([]Value) Value { return &StrBufV{} }
	case "str_sb_append":
		return func(a []Value) Value { b := a[0].(*StrBufV); b.b.WriteString(sArg(a, 1)); return b }
	case "str_sb_append_slice":
		return func(a []Value) Value {
			b := a[0].(*StrBufV)
			b.b.WriteString(a[1].(*StrV).Slice(clampInt(iArg(a, 2)), clampInt(iArg(a, 3))).s)
			return b
		}
	case "str_sb_append_cp":
		return func(a []Value) Value { b := a[0].(*StrBufV); b.b.WriteRune(rune(iArg(a, 1))); return b }
	case "str_sb_finish":
		return func(a []Value) Value { return S(a[0].(*StrBufV).b.String()) }
	case "str_sha256":
		return func(a []Value) Value {
			h := sha256.Sum256([]byte(sArg(a, 0)))
			return S(hex.EncodeToString(h[:]))
		}
	case "str_trim":
		return func(a []Value) Value { return S(strings.Trim(sArg(a, 0), " \t\n\r\v\f")) }
	case "str_contains":
		return func(a []Value) Value { return strings.Contains(sArg(a, 0), sArg(a, 1)) }
	case "str_starts_with":
		return func(a []Value) Value { return strings.HasPrefix(sArg(a, 0), sArg(a, 1)) }
	case "str_ends_with":
		return func(a []Value) Value { return strings.HasSuffix(sArg(a, 0), sArg(a, 1)) }
	case "str_repeat":
		return func(a []Value) Value {
			n := iArg(a, 1)
			if n < 0 {
				n = 0
			}
			return S(strings.Repeat(sArg(a, 0), int(n)))
		}
	case "str_replace":
		return func(a []Value) Value {
			if sArg(a, 1) == "" {
				return a[0]
			}
			return S(strings.ReplaceAll(sArg(a, 0), sArg(a, 1), sArg(a, 2)))
		}
	case "str_split":
		return func(a []Value) Value {
			if sArg(a, 1) == "" {
				return NewList([]Value{a[0]})
			}
			ps := strings.Split(sArg(a, 0), sArg(a, 1))
			out := make([]Value, len(ps))
			for i, p := range ps {
				out[i] = S(p)
			}
			return NewList(out)
		}
	case "str_join":
		return func(a []Value) Value {
			l := a[0].(*ListV)
			ps := make([]string, l.n)
			for i, e := range l.Vals() {
				ps[i] = e.(*StrV).s
			}
			return S(strings.Join(ps, sArg(a, 1)))
		}
	case "str_is_int":
		return func(a []Value) Value { return isIntText(sArg(a, 0)) }
	case "str_parse_int":
		return func(a []Value) Value {
			s := sArg(a, 0)
			if !isIntText(s) {
				return int64(0)
			}
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				if strings.HasPrefix(s, "-") {
					return int64(math.MinInt64)
				}
				return int64(math.MaxInt64)
			}
			return v
		}
	case "str_is_float":
		return func(a []Value) Value { _, ok := parseFloatText(sArg(a, 0)); return ok }
	case "str_parse_float":
		return func(a []Value) Value { f, _ := parseFloatText(sArg(a, 0)); return f }
	case "str_count":
		return func(a []Value) Value {
			if sArg(a, 1) == "" {
				return int64(0)
			}
			return int64(strings.Count(sArg(a, 0), sArg(a, 1)))
		}
	case "str_index_of":
		return func(a []Value) Value {
			s := a[0].(*StrV)
			from := iArg(a, 2)
			if from < 0 {
				from = 0
			}
			if from > int64(s.Len()) {
				return int64(-1)
			}
			rest := s.Slice(int(from), s.Len()).s
			k := strings.Index(rest, sArg(a, 1))
			if k < 0 {
				return int64(-1)
			}
			return from + int64(runeLen(rest[:k]))
		}
	case "str_reverse":
		return func(a []Value) Value {
			r := []rune(sArg(a, 0))
			for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
				r[i], r[j] = r[j], r[i]
			}
			return S(string(r))
		}
	case "str_to_lower":
		return func(a []Value) Value { return S(toLower(sArg(a, 0))) }
	case "str_to_upper":
		return func(a []Value) Value { return S(toUpper(sArg(a, 0))) }
	case "min", "max":
		return func(a []Value) Value {
			less := lessThan(a[1], a[0])
			if (name == "min") == less {
				return a[1]
			}
			return a[0]
		}
	case "abs":
		return func(a []Value) Value {
			if f, ok := a[0].(float64); ok {
				return math.Abs(f)
			}
			if lessThan(a[0], int64(0)) {
				return intNeg(a[0])
			}
			return a[0]
		}
	case "clamp":
		return func(a []Value) Value {
			if lessThan(a[0], a[1]) {
				return a[1]
			}
			if lessThan(a[2], a[0]) {
				return a[2]
			}
			return a[0]
		}
	case "sqrt":
		return func(a []Value) Value { return math.Sqrt(a[0].(float64)) }
	case "sort":
		return func(a []Value) Value {
			l := a[0].(*ListV)
			vs := append([]Value(nil), l.Vals()...)
			if len(a) > 1 {
				f := a[1]
				sort.SliceStable(vs, func(i, j int) bool { return in.apply(f, []Value{vs[i], vs[j]}).(bool) })
			} else {
				sort.SliceStable(vs, func(i, j int) bool { return Compare(vs[i], vs[j]) < 0 })
			}
			return NewList(vs)
		}
	case "assert":
		return func(a []Value) Value {
			if !a[0].(bool) {
				abort("assertion failed")
			}
			return true
		}
	case "todo", "unimplemented":
		return func([]Value) Value { abort(name); return nil }
	// Region and memory controls: the interpreter has a garbage collector.
	case "resid_arena_push", "resid_arena_pop", "resid_bulk_push", "resid_bulk_pop",
		"resid_mem_mark", "resid_mem_since_mark", "resid_mem_limit_set":
		return func([]Value) Value { return int64(0) }
	case "resid_list_str_persist_copy", "resid_dec_persist", "ct_public", "ct_hide":
		return func(a []Value) Value { return a[0] }
	case "resid_rtmod", "resid_internals", "resid_quiet":
		flag := name
		return func([]Value) Value { return in.flags[flag] }
	case "resid_rtmod_set", "resid_internals_set", "resid_quiet_set":
		flag := strings.TrimSuffix(name, "_set")
		return func(a []Value) Value { in.flags[flag] = a[0].(bool); return true }
	case "resid_raw_bits_f64":
		return func(a []Value) Value { return math.Float64frombits(uint64(iArg(a, 0))) }
	case "resid_raw_f64_bits":
		return func(a []Value) Value { return int64(math.Float64bits(a[0].(float64))) }
	}
	if strings.HasPrefix(name, "wrapping_") || strings.HasPrefix(name, "saturating_") || strings.HasPrefix(name, "checked_") {
		return in.intBuiltin(name)
	}
	if len(name) >= 2 && (name[0] == 'i' || name[0] == 'u') {
		if bits, err := strconv.Atoi(name[1:]); err == nil && bits >= 8 && bits <= 1024 {
			t := IntType{bits, name[0] == 'i'}
			return func(a []Value) Value { return convertInt(a[0], t, true) }
		}
	}
	if name == "resid_crypto_random_byte" {
		return func([]Value) Value {
			var b [1]byte
			crand.Read(b[:])
			return int64(b[0])
		}
	}
	switch name {
	case "resid_fmt_radix":
		return func(a []Value) Value { return S(fmtRadix(sArg(a, 0), iArg(a, 1))) }
	case "resid_fmt_case":
		return func(a []Value) Value { return S(fmtCase(sArg(a, 0), iArg(a, 1))) }
	case "resid_fmt_sign":
		return func(a []Value) Value { return S(fmtSign(sArg(a, 0), iArg(a, 1))) }
	case "resid_fmt_pad":
		return func(a []Value) Value { return S(fmtPad(sArg(a, 0), iArg(a, 1))) }
	case "resid_fmt_group":
		return func(a []Value) Value { return S(fmtGroup(sArg(a, 0))) }
	case "resid_fmt_style":
		return func(a []Value) Value { return S(fmtStyle(a[0].(float64), iArg(a, 1))) }
	}
	return nil
}

// intBuiltin: wrapping_add/sub/mul/div, saturating_*, and the wrapping
// conversions wrapping_i8 … wrapping_u512 (wrap into that type).
func (in *Interp) intBuiltin(name string) builtinFn {
	pre, op, _ := strings.Cut(name, "_")
	if op == "uadd" || op == "usub" || op == "umul" || op == "udiv" {
		op = op[1:]
	}
	switch op {
	case "add", "sub", "mul", "div":
		if pre == "saturating" {
			return func(a []Value) Value { return satArith(op, a[0], a[1]) }
		}
		if pre == "wrapping" {
			return func(a []Value) Value { return wrapArith(op, a[0], a[1]) }
		}
	}
	if pre == "wrapping" && len(op) > 1 && (op[0] == 'i' || op[0] == 'u') {
		if bits, err := strconv.Atoi(op[1:]); err == nil {
			t := IntType{bits, op[0] == 'i'}
			return func(a []Value) Value {
				return mkInt(wrap(bigOf(a[0]), t.Bits, t.Signed), t.Bits, t.Signed, false, "")
			}
		}
	}
	return nil
}

func clampInt(x int64) int {
	if x > math.MaxInt32 {
		return math.MaxInt32
	}
	if x < math.MinInt32 {
		return math.MinInt32
	}
	return int(x)
}

func isIntText(s string) bool {
	if s == "" {
		return false
	}
	p := s
	if p[0] == '-' || p[0] == '+' {
		p = p[1:]
	}
	if p == "" {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return true
}

func asciiMap(s string, lo, hi byte, d int) string {
	b := []byte(s)
	for i, c := range b {
		if c >= lo && c <= hi {
			b[i] = byte(int(c) + d)
		}
	}
	return string(b)
}

// parseFloatText reads a float as strtod does, then only spaces or tabs.
func parseFloatText(s string) (float64, bool) {
	t := strings.TrimLeft(s, " \t\n\r\v\f")
	t = strings.TrimRight(t, " \t")
	if t == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// adopt: an Int beside an element of another integer type takes that
// type, as a literal does when it meets a typed list.
func adopt(like, v Value) Value {
	if n, ok := like.(*Num); ok {
		if x, ok := v.(int64); ok {
			return convertInt(x, IntType{n.Bits, n.Signed}, true)
		}
	}
	return v
}

func adoptElems(l *ListV, vs []Value) []Value {
	if l.n == 0 {
		return vs
	}
	like := l.At(0)
	if _, ok := like.(*Num); !ok {
		return vs
	}
	var out []Value
	for i, v := range vs {
		if w := adopt(like, v); w != v {
			if out == nil {
				out = append([]Value(nil), vs...)
			}
			out[i] = w
		}
	}
	if out == nil {
		return vs
	}
	return out
}

// report prints the most-called functions (RESIDI_COUNT=1).
func (in *Interp) report() {
	var fs []*cfunc
	for _, cf := range in.fns {
		fs = append(fs, cf)
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].calls > fs[j].calls })
	for i := 0; i < len(fs) && i < 40; i++ {
		fmt.Fprintf(os.Stderr, "%12d %s\n", fs[i].calls, fs[i].name)
	}
}
