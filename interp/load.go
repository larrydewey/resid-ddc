package interp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Program is every module reachable from the main file. Names follow the
// compiler's merge (typecheck.resid imp_apply_vis): a module's private
// functions are visible only inside it; everything else (the main file's
// functions, every module's `pub` functions, all types) shares one
// namespace.
type Program struct {
	Main    *Module
	Mods    []*Module
	ByPath  map[string]*Module
	Pub     map[string]*FnDecl
	Types   map[string]*TypeDecl
	Vars    map[string]*Variant
	LibDirs []string
}

func Load(mainPath string, libDirs []string) (*Program, error) {
	pr := &Program{ByPath: map[string]*Module{}, Pub: map[string]*FnDecl{}, Types: map[string]*TypeDecl{}, Vars: map[string]*Variant{}, LibDirs: libDirs}
	abs, _ := filepath.Abs(mainPath)
	m, err := pr.load(abs, true)
	if err != nil {
		return nil, err
	}
	pr.Main = m
	for _, mod := range pr.Mods {
		for n, f := range mod.Fns {
			if mod.Main || f.Pub {
				if old, dup := pr.Pub[n]; dup && old.Mod != mod && !mod.Main {
					if old.Mod.Main {
						continue
					}
				}
				pr.Pub[n] = f
			}
		}
		for n, t := range mod.Types {
			pr.Types[n] = t
			for i := range t.Variants {
				pr.Vars[t.Variants[i].Name] = &t.Variants[i]
			}
		}
	}
	// The main file's own functions win over any module's export.
	for n, f := range pr.Main.Fns {
		pr.Pub[n] = f
	}
	return pr, nil
}

func (pr *Program) load(path string, main bool) (m *Module, err error) {
	if m, ok := pr.ByPath[path]; ok {
		return m, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src := &Src{R: []rune(string(data)), Path: path}
	m = &Module{Path: path, Src: src, Fns: map[string]*FnDecl{}, Types: map[string]*TypeDecl{}, Main: main}
	pr.ByPath[path] = m
	defer func() {
		if r := recover(); r != nil {
			if pe, ok := r.(*ParseError); ok {
				err = pe
				return
			}
			panic(r)
		}
	}()
	p := &parser{s: src}
	for _, imp := range scanImports(src) {
		ip := pr.resolveImport(filepath.Dir(path), imp)
		if ip == "" {
			return nil, fmt.Errorf("%s: cannot find module %q", path, imp)
		}
		im, err := pr.load(ip, false)
		if err != nil {
			return nil, err
		}
		m.Imports = append(m.Imports, im)
	}
	p.decls(m, 0, "")
	pr.Mods = append(pr.Mods, m)
	return m, nil
}

// Import paths: relative to the importing file, then the library dirs.
func (pr *Program) resolveImport(dir, imp string) string {
	cands := []string{filepath.Join(dir, imp)}
	for _, d := range pr.LibDirs {
		cands = append(cands, filepath.Join(d, imp))
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			a, _ := filepath.Abs(c)
			return a
		}
	}
	return ""
}

func scanImports(s *Src) []string {
	var out []string
	pos := 0
	depth := 0
	for {
		t := s.Lex(pos)
		if t.Kind == "eof" {
			return out
		}
		pos = t.Pos
		switch t.Text {
		case "{":
			depth++
		case "}":
			depth--
		case "import":
			if depth == 0 {
				n := s.Lex(pos)
				if n.Kind == "str" {
					out = append(out, strings.Trim(n.Text, `"`))
				}
			}
		}
	}
}
