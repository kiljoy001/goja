package goja

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kiljoy001/goja/ast"
	"github.com/kiljoy001/goja/parser"
	"github.com/kiljoy001/goja/unistring"
)

// ModuleStatus is a module record's lifecycle state.
type ModuleStatus int

const (
	ModuleUnlinked ModuleStatus = iota
	ModuleLinking
	ModuleLinked
	ModuleEvaluating
	ModuleEvaluated
)

// ModuleLoader resolves a module specifier relative to the referrer module.
type ModuleLoader func(specifier, referrer string) (string, error)

type moduleImportEntry struct {
	localName  string
	importName string
	from       string
}

type moduleExportEntry struct {
	localName  string
	exportName string
	from       string // non-empty for indirect exports
}

// ModuleRecord holds the compiled shape of a module.
type ModuleRecord struct {
	starExports map[string][]string
	ambiguous   map[string]bool
	src         string
	values      map[string]Value
	name        string
	program     *ast.Program
	status      ModuleStatus
	imports     []moduleImportEntry
	exports     []moduleExportEntry
	stars       []string
	starAs      map[string]string // namespace name -> source
	namespace   *Object
}

// SetModuleLoader installs the host hook that resolves module specifiers.
func (r *Runtime) SetModuleLoader(l ModuleLoader) {
	r.moduleLoader = l
}

// DefineModule parses src as a module and registers it under name.
func (r *Runtime) DefineModule(name, src string) error {
	if rec, ok := r.modules[name]; ok && rec.src == src {
		return nil
	}
	prog, err := parser.ParseModule(name, src)
	if err != nil {
		return err
	}
	rec := &ModuleRecord{name: name, src: src, program: prog, status: ModuleUnlinked, starAs: map[string]string{}, values: map[string]Value{}, starExports: map[string][]string{}, ambiguous: map[string]bool{}}
	seen := map[string]bool{}
	for _, stmt := range prog.Body {
		switch s := stmt.(type) {
		case *ast.ImportDeclaration:
			if s.Source == nil {
				continue
			}
			from := s.Source.Value.String()
			for _, spec := range s.Specifiers {
				importName := "default"
				switch spec.Kind {
				case ast.ImportNamespace:
					importName = "*"
				case ast.ImportNamed:
					if spec.Imported != nil {
						importName = spec.Imported.Name.String()
					}
				}
				rec.imports = append(rec.imports, moduleImportEntry{
					localName:  spec.Local.Name.String(),
					importName: importName,
					from:       from,
				})
			}
		case *ast.ExportDeclaration:
			if s.Declaration != nil && !s.IsDefault {
				for _, name := range moduleDeclaredNames(s.Declaration) {
					if err := addExport(rec, seen, name, name, ""); err != nil {
						return err
					}
				}
			}
			if s.IsDefault {
				if err := addExport(rec, seen, "", "default", ""); err != nil {
					return err
				}
				continue
			}
			if s.Source != nil {
				from := s.Source.Value.String()
				if s.Namespace != nil {
					name := s.Namespace.Name.String()
					rec.stars = append(rec.stars, from)
					rec.starAs[name] = from
					if err := addExport(rec, seen, "", name, from); err != nil {
						return err
					}
					continue
				}
				if len(s.Specifiers) == 0 {
					rec.stars = append(rec.stars, from)
					continue
				}
				for _, spec := range s.Specifiers {
					local := ""
					if spec.Local != nil {
						local = spec.Local.Name.String()
					}
					exported := local
					if spec.Exported != nil {
						exported = spec.Exported.Name.String()
					}
					if err := addExport(rec, seen, local, exported, from); err != nil {
						return err
					}
				}
				continue
			}
			for _, spec := range s.Specifiers {
				local := ""
				if spec.Local != nil {
					local = spec.Local.Name.String()
				}
				exported := local
				if spec.Exported != nil {
					exported = spec.Exported.Name.String()
				}
				if err := addExport(rec, seen, local, exported, ""); err != nil {
					return err
				}
			}
		}
	}
	if r.modules == nil {
		r.modules = map[string]*ModuleRecord{}
	}
	r.modules[name] = rec
	return nil
}

func addExport(rec *ModuleRecord, seen map[string]bool, local, exported, from string) error {
	if exported == "" {
		return nil
	}
	if seen[exported] {
		return fmt.Errorf("duplicate export %q in module %q", exported, rec.name)
	}
	seen[exported] = true
	rec.exports = append(rec.exports, moduleExportEntry{localName: local, exportName: exported, from: from})
	return nil
}

// LinkModule resolves and links a registered module and its dependencies.
func (r *Runtime) LinkModule(name string) error {
	return r.linkModule(name)
}

func (r *Runtime) linkModule(name string) error {
	rec, ok := r.modules[name]
	if !ok {
		return fmt.Errorf("module %q is not defined", name)
	}
	switch rec.status {
	case ModuleLinking, ModuleLinked, ModuleEvaluating, ModuleEvaluated:
		return nil
	}
	rec.status = ModuleLinking
	resolveAndLink := func(spec string) error {
		dep := spec
		if r.moduleLoader != nil {
			resolved, err := r.moduleLoader(spec, name)
			if err != nil {
				return err
			}
			dep = resolved
		}
		return r.linkModule(dep)
	}
	for _, imp := range rec.imports {
		if err := resolveAndLink(imp.from); err != nil {
			return err
		}
	}
	for _, exp := range rec.exports {
		if exp.from != "" {
			if err := resolveAndLink(exp.from); err != nil {
				return err
			}
		}
	}
	for _, star := range rec.stars {
		if err := resolveAndLink(star); err != nil {
			return err
		}
	}
	// Collect statically known star exports and mark ambiguous names.
	for _, star := range rec.stars {
		dep := star
		if r.moduleLoader != nil {
			if resolved, err := r.moduleLoader(star, name); err == nil {
				dep = resolved
			}
		}
		d, ok := r.modules[dep]
		if !ok {
			continue
		}
		for _, e := range d.exports {
			if e.exportName == "" || e.exportName == "default" {
				continue
			}
			rec.starExports[e.exportName] = appendUnique(rec.starExports[e.exportName], dep)
		}
		for n := range d.starExports {
			rec.starExports[n] = appendUnique(rec.starExports[n], dep)
		}
	}
	for n, srcs := range rec.starExports {
		if len(srcs) > 1 && !moduleHasLocalExport(rec, n) {
			rec.ambiguous[n] = true
		}
	}
	// Validate that every imported name is exported by its dependency.
	for _, imp := range rec.imports {
		if imp.importName == "*" {
			continue
		}
		dep := imp.from
		if r.moduleLoader != nil {
			if resolved, err := r.moduleLoader(imp.from, name); err == nil {
				dep = resolved
			}
		}
		d, ok := r.modules[dep]
		if !ok {
			continue
		}
		if !r.moduleExportsName(d, imp.importName, map[*ModuleRecord]bool{}) {
			return fmt.Errorf("module %q does not export %q", dep, imp.importName)
		}
	}
	rec.status = ModuleLinked
	return nil
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func moduleHasLocalExport(rec *ModuleRecord, name string) bool {
	for _, e := range rec.exports {
		if e.exportName == name && e.from == "" {
			return true
		}
	}
	return false
}

func (r *Runtime) moduleExportsName(rec *ModuleRecord, name string, seen map[*ModuleRecord]bool) bool {
	if seen[rec] {
		return false
	}
	seen[rec] = true
	for _, e := range rec.exports {
		if e.exportName == name {
			return true
		}
	}
	if rec.ambiguous[name] {
		return false
	}
	if _, ok := rec.starExports[name]; ok {
		return true
	}
	for _, star := range rec.stars {
		dep := star
		if r.moduleLoader != nil {
			if resolved, err := r.moduleLoader(star, rec.name); err == nil {
				dep = resolved
			}
		}
		if d, ok := r.modules[dep]; ok && r.moduleExportsName(d, name, seen) {
			return true
		}
	}
	return false
}

// moduleDeclaredNames returns the names bound by an exported declaration.
func moduleDeclaredNames(n ast.Node) []string {
	switch d := n.(type) {
	case *ast.VariableStatement:
		return moduleBindingNames(d.List)
	case *ast.LexicalDeclaration:
		return moduleBindingNames(d.List)
	case *ast.FunctionDeclaration:
		if d.Function != nil && d.Function.Name != nil {
			return []string{d.Function.Name.Name.String()}
		}
	case *ast.ClassDeclaration:
		if d.Class != nil && d.Class.Name != nil {
			return []string{d.Class.Name.Name.String()}
		}
	}
	return nil
}

func moduleBindingNames(list []*ast.Binding) []string {
	var res []string
	for _, b := range list {
		if id, ok := b.Target.(*ast.Identifier); ok {
			res = append(res, id.Name.String())
		}
	}
	return res
}

// ModuleNamespace evaluates a module (if needed) and returns its namespace.
func (r *Runtime) ModuleNamespace(name string) (Value, error) {
	if err := r.evaluateModule(name); err != nil {
		return nil, err
	}
	return r.moduleNamespaceValue(r.modules[name]), nil
}

func (r *Runtime) moduleNamespaceValue(rec *ModuleRecord) *Object {
	if rec.namespace != nil {
		return rec.namespace
	}
	ns := r.newBaseObject(nil, classObject)
	seen := map[string]bool{}
	var names []string
	for _, e := range rec.exports {
		if e.exportName == "" || seen[e.exportName] {
			continue
		}
		seen[e.exportName] = true
		names = append(names, e.exportName)
	}
	sort.Strings(names)
	for _, n := range names {
		key := unistring.NewFromString(n)
		v := rec.values[n]
		if v == nil {
			v = _undefined
		}
		ns.defineOwnPropertyStr(key, PropertyDescriptor{
			Value:        v,
			Writable:     FLAG_TRUE,
			Enumerable:   FLAG_TRUE,
			Configurable: FLAG_FALSE,
		}, false)
	}
	ns.defineOwnPropertySym(SymToStringTag, PropertyDescriptor{
		Value:        asciiString("Module"),
		Writable:     FLAG_FALSE,
		Enumerable:   FLAG_FALSE,
		Configurable: FLAG_FALSE,
	}, false)
	rec.namespace = ns.val
	return ns.val
}

// evaluateModule links and evaluates a module and its dependencies.
func (r *Runtime) evaluateModule(name string) error {
	rec, ok := r.modules[name]
	if !ok {
		return fmt.Errorf("module %q is not defined", name)
	}
	switch rec.status {
	case ModuleEvaluated, ModuleEvaluating:
		return nil
	}
	if err := r.linkModule(name); err != nil {
		return err
	}
	rec.status = ModuleEvaluating
	deps := r.newBaseObject(nil, classObject)
	evalDep := func(spec string) error {
		dep := spec
		if r.moduleLoader != nil {
			resolved, err := r.moduleLoader(spec, name)
			if err != nil {
				return err
			}
			dep = resolved
		}
		if err := r.evaluateModule(dep); err != nil {
			return err
		}
		deps.setOwnStr(unistring.NewFromString(spec), r.moduleNamespaceValue(r.modules[dep]), false)
		return nil
	}
	for _, imp := range rec.imports {
		if err := evalDep(imp.from); err != nil {
			return err
		}
	}
	for _, exp := range rec.exports {
		if exp.from != "" {
			if err := evalDep(exp.from); err != nil {
				return err
			}
		}
	}
	for _, star := range rec.stars {
		if err := evalDep(star); err != nil {
			return err
		}
	}
	exportsObj := r.newBaseObject(nil, classObject)
	body := r.rewriteModuleSource(rec)
	prevMeta := r.importMeta
	meta := r.NewObject()
	meta.Set("url", asciiString("file:///"+name))
	r.importMeta = meta
	wrapper := "(function(__mods, __exports) {\"use strict\";\n" + body + "\n})"
	fnVal, err := r.RunString(wrapper)
	if err != nil {
		return fmt.Errorf("module %q: %w", name, err)
	}
	fn, ok := r.toObject(fnVal).self.assertCallable()
	if !ok {
		return fmt.Errorf("module %q: invalid module body", name)
	}
	_ = fn(FunctionCall{This: _undefined, Arguments: []Value{deps.val, exportsObj.val}})
	r.importMeta = prevMeta
	if keys, ok := exportsObj.val.self.(*baseObject); ok {
		for k, v := range keys.values {
			rec.values[k.String()] = v
		}
	}
	// Star exports.
	for _, star := range rec.stars {
		dep := star
		if r.moduleLoader != nil {
			if resolved, err := r.moduleLoader(star, name); err == nil {
				dep = resolved
			}
		}
		if d, ok := r.modules[dep]; ok {
			for k, v := range d.values {
				if k == "default" {
					continue
				}
				if rec.ambiguous[k] {
					continue
				}
				if _, exists := rec.values[k]; !exists {
					rec.values[k] = v
					rec.exports = append(rec.exports, moduleExportEntry{exportName: k, from: star})
				}
			}
		}
	}
	rec.status = ModuleEvaluated
	return nil
}

// rewriteModuleSource lowers import/export declarations to plain script code.
func (r *Runtime) rewriteModuleSource(rec *ModuleRecord) string {
	src := rec.src
	var sb strings.Builder
	for _, stmt := range rec.program.Body {
		idx0 := int(stmt.Idx0()) - 1
		idx1 := int(stmt.Idx1()) - 1
		if idx0 < 0 || idx1 > len(src) || idx0 >= idx1 {
			continue
		}
		text := src[idx0:idx1]
		switch s := stmt.(type) {
		case *ast.ImportDeclaration:
			if s.Source == nil {
				continue
			}
			from := s.Source.Value.String()
			sb.WriteString("__mods[" + strconvQuote(from) + "];")
			for _, spec := range s.Specifiers {
				local := spec.Local.Name.String()
				switch spec.Kind {
				case ast.ImportNamespace:
					sb.WriteString("const " + local + " = __mods[" + strconvQuote(from) + "];")
				case ast.ImportDefault:
					sb.WriteString("const " + local + " = __mods[" + strconvQuote(from) + "].default;")
				default:
					imported := local
					if spec.Imported != nil {
						imported = spec.Imported.Name.String()
					}
					sb.WriteString("const " + local + " = __mods[" + strconvQuote(from) + "][" + strconvQuote(imported) + "];")
				}
			}
		case *ast.ExportDeclaration:
			if s.IsDefault {
				rest := text
				if i := strings.Index(rest, "default"); i >= 0 {
					rest = rest[i+len("default"):]
				}
				rest = strings.TrimSpace(rest)
				rest = strings.TrimSuffix(rest, ";")
				sb.WriteString("__exports.default = (" + strings.TrimSpace(rest) + ");\n")
				continue
			}
			if s.Declaration != nil {
				sb.WriteString(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "export")) + ";\n")
				for _, n := range moduleDeclaredNames(s.Declaration) {
					sb.WriteString("__exports[" + strconvQuote(n) + "] = " + n + ";\n")
				}
				continue
			}
			from := ""
			if s.Source != nil {
				from = s.Source.Value.String()
			}
			for _, spec := range s.Specifiers {
				local := ""
				if spec.Local != nil {
					local = spec.Local.Name.String()
				}
				exported := local
				if spec.Exported != nil {
					exported = spec.Exported.Name.String()
				}
				if from != "" {
					sb.WriteString("__exports[" + strconvQuote(exported) + "] = __mods[" + strconvQuote(from) + "][" + strconvQuote(local) + "];")
				} else {
					sb.WriteString("__exports[" + strconvQuote(exported) + "] = " + local + ";\n")
				}
			}
		default:
			sb.WriteString(text)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func strconvQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}
