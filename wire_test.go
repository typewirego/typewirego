package typewirego_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/typewirego/typewirego"
)

func leafWire(label string) typewirego.TypeWire[string] {
	return typewirego.Of(typewirego.Opts[string]{Token: label,
		Creator: func(context.Context) (string, error) { return label, nil }})
}

// recordingBinder is a Binder that records the order of Bind calls.
type recordingBinder struct {
	bound map[typewirego.Token]typewirego.AnyWire
	order []string
}

func newRecordingBinder() *recordingBinder {
	return &recordingBinder{bound: map[typewirego.Token]typewirego.AnyWire{}}
}

func (b *recordingBinder) Bind(w typewirego.AnyWire) {
	b.bound[w.Token()] = w
	b.order = append(b.order, w.Token().Label())
}

func (b *recordingBinder) IsBound(t typewirego.Token) bool { _, ok := b.bound[t]; return ok }

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("%s: expected a panic", name)
			return
		}
		msg, _ := r.(string)
		if !strings.HasPrefix(msg, "typewirego") {
			t.Errorf("%s: panic message %q does not name the package", name, msg)
		}
	}()
	f()
}

func TestOfPanicsOnInvalidOptions(t *testing.T) {
	leaf := leafWire("t.Leaf")
	create := func(context.Context, typewirego.Deps, typewirego.Resolver) (int, error) { return 1, nil }
	plain := func(context.Context) (int, error) { return 1, nil }
	mustPanic(t, "neither creator", func() { typewirego.Of(typewirego.Opts[int]{Token: "x"}) })
	mustPanic(t, "both creators", func() {
		typewirego.Of(typewirego.Opts[int]{Token: "x", Creator: plain, CreateWith: create})
	})
	mustPanic(t, "imports without CreateWith", func() {
		typewirego.Of(typewirego.Opts[int]{Token: "x", Creator: plain,
			Imports: []typewirego.Import{{Name: "leaf", Wire: leaf}}})
	})
	mustPanic(t, "unknown scope", func() {
		typewirego.Of(typewirego.Opts[int]{Token: "x", Creator: plain, Scope: "request"})
	})
	mustPanic(t, "nil import wire", func() {
		typewirego.Of(typewirego.Opts[int]{Token: "x", CreateWith: create,
			Imports: []typewirego.Import{{Name: "leaf"}}})
	})
	mustPanic(t, "two imports sharing a name", func() {
		typewirego.Of(typewirego.Opts[int]{Token: "x", CreateWith: create,
			Imports: []typewirego.Import{{Name: "a", Wire: leaf}, {Name: "a", Wire: leafWire("t.Other")}}})
	})
	// Valid options do not panic, and CreateWith needs no Imports.
	w := typewirego.Of(typewirego.Opts[int]{Token: "x", CreateWith: create})
	if w.Scope() != typewirego.Singleton {
		t.Errorf("default scope = %q, want singleton", w.Scope())
	}
	if w.Token().Label() != "x" {
		t.Errorf("label = %q", w.Token().Label())
	}
}

func TestOfCopiesImports(t *testing.T) {
	a, b := leafWire("t.A"), leafWire("t.B")
	imports := []typewirego.Import{{Name: "a", Wire: a}}
	w := typewirego.Of(typewirego.Opts[int]{Token: "x", Imports: imports,
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (int, error) { return 0, nil }})
	imports[0] = typewirego.Import{Name: "b", Wire: b}
	got := w.Imports()
	if len(got) != 1 || got[0].Wire.Token() != a.Token() {
		t.Fatalf("Of did not copy its imports: %v", got)
	}
	got[0] = typewirego.Import{Name: "b", Wire: b}
	if w.Imports()[0].Wire.Token() != a.Token() {
		t.Fatal("Imports returned the wire's own slice")
	}
}

func TestTokensCompareByIdentity(t *testing.T) {
	a, b := leafWire("t.Same"), leafWire("t.Same")
	if a.Token() == b.Token() {
		t.Fatal("two wires with one label share a token")
	}
	if a.Token() != a.Token() {
		t.Fatal("a token differs from itself")
	}
	var zero typewirego.Token
	if zero.Label() != "" {
		t.Fatal("zero token has a label")
	}
}

func TestApplyBindsImportsFirstAndSkipsBoundOnes(t *testing.T) {
	leaf := leafWire("t.Leaf")
	mid := typewirego.Of(typewirego.Opts[string]{Token: "t.Mid",
		Imports:    []typewirego.Import{{Name: "leaf", Wire: leaf}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (string, error) { return "", nil }})
	top := typewirego.Of(typewirego.Opts[string]{Token: "t.Top",
		Imports:    []typewirego.Import{{Name: "mid", Wire: mid}, {Name: "leaf", Wire: leaf}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (string, error) { return "", nil }})
	b := newRecordingBinder()
	if err := top.Apply(b); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(b.order, ","); got != "t.Leaf,t.Mid,t.Top" {
		t.Fatalf("bind order = %s", got)
	}

	// An override bound first survives the original's Apply, together with
	// the imports it skips.
	b = newRecordingBinder()
	override := mid.WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[string]) (string, error) {
		return "fake", nil
	})
	b.Bind(override)
	if err := top.Apply(b); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(b.order, ","); got != "t.Mid,t.Leaf,t.Top" {
		t.Fatalf("bind order with override = %s", got)
	}
	if b.bound[mid.Token()] != override {
		t.Fatal("the original's Apply replaced the override")
	}
}

func TestGroupsApplyInOrderAndAreImmutable(t *testing.T) {
	a, b2, c := leafWire("t.A"), leafWire("t.B"), leafWire("t.C")
	g := typewirego.GroupOf(a, b2)
	g2 := g.WithExtraWires(c)
	if len(g.Wires()) != 2 || len(g2.Wires()) != 3 {
		t.Fatal("WithExtraWires changed the original group")
	}
	ws := g.Wires()
	ws[0] = c
	if g.Wires()[0] != typewirego.AnyWire(a) {
		t.Fatal("Wires returned the group's own slice")
	}
	combined := typewirego.CombineGroups(g2, typewirego.GroupOf(a))
	rb := newRecordingBinder()
	if err := combined.Apply(rb); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rb.order, ","); got != "t.A,t.B,t.C,t.A" {
		t.Fatalf("combined order = %s", got)
	}
	mustPanic(t, "GroupOf(nil)", func() { typewirego.GroupOf(a, nil) })
	mustPanic(t, "WithExtraWires(nil)", func() { g.WithExtraWires(nil) })
}

func TestErrorCarriersMatchTheirSentinels(t *testing.T) {
	cycle := &typewirego.CircularDependencyError{Path: []string{"a", "b", "a"}}
	wrapped := &typewirego.CreatorError{Label: "a", Cause: cycle}
	joined := errors.Join(errors.New("other"), wrapped)
	if !errors.Is(joined, typewirego.ErrCircularDependency) || !errors.Is(joined, typewirego.ErrCreator) {
		t.Fatal("sentinels not reachable through CreatorError and errors.Join")
	}
	var got *typewirego.CircularDependencyError
	if !errors.As(joined, &got) || strings.Join(got.Path, ",") != "a,b,a" {
		t.Fatal("CircularDependencyError not reachable with errors.As")
	}
	if !strings.HasPrefix(cycle.Error(), "typewirego:") || !strings.HasPrefix(wrapped.Error(), "typewirego:") {
		t.Fatal("messages do not start with typewirego:")
	}
	tt := &typewirego.TeardownTimeoutError{Running: []string{"x"}}
	if !errors.Is(errors.Join(tt), typewirego.ErrTeardownTimeout) || !strings.HasPrefix(tt.Error(), "typewirego:") {
		t.Fatal("TeardownTimeoutError does not match ErrTeardownTimeout")
	}
	if errors.Is(cycle, typewirego.ErrCreator) || errors.Is(tt, typewirego.ErrCircularDependency) {
		t.Fatal("a carrier matches a foreign sentinel")
	}
}
func TestDepRejectsAWireOutsideTheImports(t *testing.T) {
	ctx := context.Background()
	in := leafWire("t.In")
	out := leafWire("t.Out")
	var gotIn string
	var errOut, errNil error
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.User",
		Imports: []typewirego.Import{{Name: "in", Wire: in}},
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (string, error) {
			var err error
			if gotIn, err = typewirego.Dep(d, in); err != nil {
				return "", err
			}
			_, errOut = typewirego.Dep(d, out)
			_, errNil = typewirego.Dep[string](d, nil)
			return "user", nil
		}})
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	if err := typewirego.GroupOf(w, out).Apply(c); err != nil {
		t.Fatal(err)
	}
	if _, err := w.GetInstance(ctx, c); err != nil {
		t.Fatal(err)
	}
	if gotIn != "t.In" {
		t.Errorf("Dep(in) = %q", gotIn)
	}
	if !errors.Is(errOut, typewirego.ErrNotImported) {
		t.Errorf("Dep(out) error = %v, want ErrNotImported", errOut)
	}
	if !errors.Is(errNil, typewirego.ErrNotImported) {
		t.Errorf("Dep(nil) error = %v, want ErrNotImported", errNil)
	}
	if _, err := typewirego.Dep(typewirego.Deps{}, in); !errors.Is(err, typewirego.ErrNotImported) {
		t.Errorf("Dep on zero Deps = %v, want ErrNotImported", err)
	}
}
