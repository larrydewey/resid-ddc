package interp

// Type is a parsed Resid type: Name with arguments (List(Int), Int(256),
// Map(Str, Int)); an integer argument (a width or capacity) is kept in N.
type Type struct {
	Name    string
	Args    []*Type
	N       int // width for Int(N)/UInt(N)/Float(N)/Dec(N), or a literal arg
	IsLit   bool
	Closure bool // Ret closure(Params): Args[0] is Ret, rest params
}

func (t *Type) String() string {
	if t == nil {
		return "?"
	}
	if t.IsLit {
		return itoa(t.N)
	}
	if t.Closure {
		s := t.Args[0].String() + " closure("
		for i, a := range t.Args[1:] {
			if i > 0 {
				s += ", "
			}
			s += a.String()
		}
		return s + ")"
	}
	if len(t.Args) == 0 {
		return t.Name
	}
	s := t.Name + "("
	for i, a := range t.Args {
		if i > 0 {
			s += ", "
		}
		s += a.String()
	}
	return s + ")"
}

type Expr interface{}

type (
	ELit struct {
		Kind, Text string
		Pos        int
	}
	EFStr struct {
		Parts []string
		Holes []Expr
		Specs []string
	}
	ERef struct {
		Name string
		Pos  int
	}
	ENamed struct {
		Name string
		X    Expr
	}
	ECall struct {
		Name string
		Args []Expr
		Pos  int
	}
	ECallV struct {
		Fn   Expr
		Args []Expr
	}
	EMCall struct {
		Recv Expr
		Name string
		Args []Expr
		Pos  int
	}
	EField struct {
		X    Expr
		Name string
	}
	EIndex struct{ X, I Expr }
	ESlice struct {
		X, Lo, Hi Expr
		Incl      bool
	}
	ERange struct {
		Lo, Hi Expr
		Incl   bool
	}
	ERangeFrom struct{ Lo Expr }
	EBin       struct {
		Op   string
		L, R Expr
	}
	EUn struct {
		Op string
		X  Expr
	}
	ECast struct {
		T *Type
		X Expr
	}
	EIf struct {
		C    Expr
		Then Expr
		Else Expr // nil when absent
	}
	ETern   struct{ C, A, B Expr }
	ETry    struct{ X Expr }
	EOrElse struct{ X, D Expr }
	EMatch  struct {
		X    Expr
		Arms []Arm
	}
	ELambda struct {
		Params []string
		Body   Expr
	}
	EStruct struct {
		Name   string
		Fields []string
		Vals   []Expr
	}
	EList  struct{ Elems []Expr }
	EMap   struct{ Keys, Vals []Expr }
	ESet   struct{ Elems []Expr }
	EEmpty struct{}
	EBlock struct {
		Binds []Stmt
		Last  Expr
	}
	ESpawn struct {
		Caps string
		Body *SBlock
	}
)

type Arm struct {
	Variant string
	Names   []string
	Body    Expr
}

type Stmt interface{}

type (
	SReturn struct{ X Expr }
	SBind   struct {
		T    *Type
		Name string
		X    Expr
	}
	SDiscard struct{ X Expr }
	SExpr    struct{ X Expr }
	SIf      struct {
		C          Expr // or *CPat
		Then, Else Stmt
	}
	SWhile struct {
		C    Expr
		Body Stmt
	}
	SFor struct {
		T    *Type
		Name string
		Iter Expr
		Body Stmt
	}
	SBlock  struct{ Stmts []Stmt }
	SBreak  struct{}
	SCont   struct{}
	SLetPat struct {
		Variant, Name string
		X             Expr
	}
	SDestr struct {
		TypeName string
		Names    []string
		X        Expr
	}
	CPat struct {
		Variant, Name string
		X             Expr
	}
)

type Param struct {
	T       *Type
	Name    string
	Default Expr // nil when none
}

type FnDecl struct {
	Name   string
	Params []Param
	Ret    *Type
	Body   *SBlock
	Pub    bool
	Annots string
	Mod    *Module
	Link   bool // @link: a native binding, not interpretable
}

type TypeDecl struct {
	Name     string
	Kind     string // struct, sum, alias
	Fields   []Param
	Variants []Variant
	Alias    *Type
	Mod      *Module
}

type Variant struct {
	Name     string
	Payloads []*Type
	Sum      *TypeDecl
}

type Module struct {
	Path    string
	Src     *Src
	Fns     map[string]*FnDecl
	Types   map[string]*TypeDecl
	Imports []*Module
	Main    bool
}
