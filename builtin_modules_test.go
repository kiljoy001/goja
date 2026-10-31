package goja

import "testing"

func TestModuleRegistryAndLinking(t *testing.T) {
	vm := New()
	if err := vm.DefineModule("a.js", `import {x} from "b.js"; export const y = 1; export default 2;`); err != nil {
		t.Fatal(err)
	}
	if err := vm.DefineModule("b.js", `export const x = 1;`); err != nil {
		t.Fatal(err)
	}
	if err := vm.LinkModule("a.js"); err != nil {
		t.Fatal(err)
	}
	if err := vm.DefineModule("dup.js", `export {a}; export {a};`); err == nil {
		t.Fatal("expected a duplicate export error")
	}
	if err := vm.LinkModule("missing.js"); err == nil {
		t.Fatal("expected a missing module error")
	}
	if err := vm.DefineModule("c1.js", `import "c2.js"; export let a = 1;`); err != nil {
		t.Fatal(err)
	}
	if err := vm.DefineModule("c2.js", `import "c1.js"; export let b = 2;`); err != nil {
		t.Fatal(err)
	}
	if err := vm.LinkModule("c1.js"); err != nil {
		t.Fatalf("cyclic import should link: %v", err)
	}
}

func TestModuleNamespace(t *testing.T) {
	vm := New()
	if err := vm.DefineModule("ns.js", `export const b = 1; export function a() {} export class C {}; export {b as z};`); err != nil {
		t.Fatal(err)
	}
	nsv, err := vm.ModuleNamespace("ns.js")
	if err != nil {
		t.Fatal(err)
	}
	ns := nsv.(*Object)
	if got := ns.Keys(); len(got) != 4 || got[0] != "C" || got[1] != "a" || got[2] != "b" || got[3] != "z" {
		t.Fatalf("unexpected namespace keys: %v", got)
	}
	if ns.Prototype() != nil {
		t.Fatal("module namespace must have a null prototype")
	}
	if tag := ns.self.getSym(SymToStringTag, nil); tag == nil || tag.String() != "Module" {
		t.Fatalf("unexpected toStringTag: %v", tag)
	}
	d := ns.self.getOwnPropStr("a")
	if d == nil {
		t.Fatal("missing export property")
	}
}

func TestModuleEvaluation(t *testing.T) {
	vm := New()
	if err := vm.DefineModule("dep.js", `export const two = 2; export default 40;`); err != nil {
		t.Fatal(err)
	}
	if err := vm.DefineModule("main.js", `import def, {two} from "dep.js"; export const sum = def + two; export default sum * 2;`); err != nil {
		t.Fatal(err)
	}
	nsv, err := vm.ModuleNamespace("main.js")
	if err != nil {
		t.Fatal(err)
	}
	ns := nsv.(*Object)
	if got := ns.Get("sum"); got == nil || got.ToInteger() != 42 {
		t.Fatalf("sum = %v", got)
	}
	if got := ns.Get("default"); got == nil || got.ToInteger() != 84 {
		t.Fatalf("default = %v", got)
	}
}

func TestDynamicImportResolution(t *testing.T) {
	vm := New()
	if err := vm.DefineModule("dyn.js", `export const value = 7;`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`var got = -1; import("dyn.js").then(function (ns) { got = ns.value; });`); err != nil {
		t.Fatal(err)
	}
	if v := vm.Get("got"); v == nil || v.ToInteger() != 7 {
		t.Fatalf("import() resolved to %v", v)
	}
}

func TestAmbiguousStarExports(t *testing.T) {
	vm := New()
	for name, src := range map[string]string{
		"s1.js":   `export const dup = 1; export const one = 1;`,
		"s2.js":   `export const dup = 2; export const two = 2;`,
		"star.js": `export * from "s1.js"; export * from "s2.js";`,
		"bad.js":  `import {dup} from "star.js"; export const x = dup;`,
		"ok.js":   `import {one, two} from "star.js"; export const x = one + two;`,
	} {
		if err := vm.DefineModule(name, src); err != nil {
			t.Fatal(err)
		}
	}
	if err := vm.LinkModule("star.js"); err != nil {
		t.Fatal(err)
	}
	if err := vm.LinkModule("ok.js"); err != nil {
		t.Fatalf("non-ambiguous star import should link: %v", err)
	}
	if err := vm.LinkModule("bad.js"); err == nil {
		t.Fatal("expected an ambiguous export link error")
	}
}

func TestDefineModuleIdempotent(t *testing.T) {
	vm := New()
	src := `export const a = 1;`
	if err := vm.DefineModule("m.js", src); err != nil {
		t.Fatal(err)
	}
	if err := vm.LinkModule("m.js"); err != nil {
		t.Fatal(err)
	}
	if err := vm.DefineModule("m.js", src); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.ModuleNamespace("m.js"); err != nil {
		t.Fatal(err)
	}
}
