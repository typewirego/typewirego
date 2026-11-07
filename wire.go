package typewirego

import (
	"context"
	"fmt"
)

// Binder is the cutout a wire registers itself through.
type Binder interface {
	Bind(w AnyWire)
	IsBound(t Token) bool
}

// Resolver is the cutout a wire resolves values through. GetCached is a
// cache-only read: it never runs a creator.
type Resolver interface {
	Resolve(ctx context.Context, t Token) (any, error)
	GetCached(t Token) (any, error)
}

// Creator builds a wire's value from its resolved imports. r carries the
// resolve chain, so a value resolved through it is checked for cycles.
type Creator[T any] func(ctx context.Context, imports Deps, r Resolver) (T, error)

// Import is a named dependency of a wire. Imports resolve in declaration
// order.
type Import struct {
	Name string
	Wire AnyWire
}

// Deps holds a creator's imports, already resolved, plus the lifetime context
// of the value being created.
type Deps struct {
	vals map[Token]any
	life context.Context
}

// Dep returns the resolved value of w, which must be one of the creator's
// declared imports; any other wire is an ErrNotImported error.
func Dep[T any](d Deps, w TypeWire[T]) (T, error) {
	var zero T
	if w == nil {
		return zero, ErrNotImported
	}
	v, ok := d.vals[w.Token()]
	if !ok {
		return zero, fmt.Errorf("%w: %s", ErrNotImported, w.Token().Label())
	}
	return castValue[T](w.Token(), v)
}

// Lifetime returns a context that ends only after the cleanup of the
// value being created, so a value may keep background work running through
// Teardown's Closing phase until its own reverse-order slot.
func Lifetime(d Deps) context.Context {
	if d.life == nil {
		return context.Background()
	}
	return d.life
}

func castValue[T any](t Token, v any) (T, error) {
	var zero T
	if v == nil {
		return zero, nil
	}
	tv, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("typewirego: %s resolved to %T, not %T", t.Label(), v, zero)
	}
	return tv, nil
}

// AnyWire is a wire of any value type. The unexported method keeps the set of
// implementations to this package.
type AnyWire interface {
	Token() Token
	Imports() []Import
	Scope() Scope
	Apply(b Binder) error

	core() *wireCore
}

// TypeWire is an immutable description of how to create a T.
type TypeWire[T any] interface {
	AnyWire
	// GetInstance resolves the wire's value through r.
	GetInstance(ctx context.Context, r Resolver) (T, error)
	// WithCreator returns a wire with the same token, scope, declared
	// imports and KeepOpen whose value comes from fn. fn receives Deps with
	// no imports resolved (only Lifetime applies to it); original resolves
	// the declared imports through the Resolver it is given, when and only
	// if fn calls it.
	WithCreator(fn func(ctx context.Context, imports Deps, r Resolver, original Creator[T]) (T, error)) TypeWire[T]
}

// Opts configures Of. Set exactly one of Creator and CreateWith; Imports
// requires CreateWith. Scope defaults to Singleton.
type Opts[T any] struct {
	Token      string
	Creator    func(ctx context.Context) (T, error)
	CreateWith Creator[T]
	Imports    []Import
	Scope      Scope
	// KeepOpen means the container never closes this wire's value; use it
	// for values the wire does not own, such as os.Stdout or a shared client
	// passed in from the entrypoint. A KeepOpen singleton also protects the
	// value when another wire returns it; a KeepOpen transient does not
	// protect a value a singleton owns. The value is treated as one with no
	// closer, and its events report closer "none". The container recognizes
	// the value again by ==, so a borrowed value whose dynamic type is not
	// comparable, such as a map or slice type with Close, must be wrapped in
	// a pointer, or every wire that returns it must be KeepOpen too.
	KeepOpen bool
}

// wireCore is the type-erased part of a wire the container works with.
type wireCore struct {
	token    Token
	scope    Scope
	imports  []Import
	keepOpen bool
	create   func(ctx context.Context, d Deps, r Resolver) (any, error)
}

type wire[T any] struct {
	c       *wireCore
	creator Creator[T]
}

// Of builds a wire. Invalid options panic, since wires are package-level
// values built once at init: both or neither creator set, Imports without
// CreateWith, an unknown scope, a nil import wire, or two imports sharing a
// name. Unlike typewirepy, CreateWith needs no Imports, because a Go creator
// also receives the Resolver.
func Of[T any](o Opts[T]) TypeWire[T] {
	switch {
	case o.Creator == nil && o.CreateWith == nil:
		panic(fmt.Sprintf("typewirego.Of(%q): set one of Creator and CreateWith", o.Token))
	case o.Creator != nil && o.CreateWith != nil:
		panic(fmt.Sprintf("typewirego.Of(%q): set only one of Creator and CreateWith", o.Token))
	case len(o.Imports) > 0 && o.CreateWith == nil:
		panic(fmt.Sprintf("typewirego.Of(%q): Imports requires CreateWith", o.Token))
	}
	scope := o.Scope
	switch scope {
	case "":
		scope = Singleton
	case Singleton, Transient:
	default:
		panic(fmt.Sprintf("typewirego.Of(%q): unknown scope %q", o.Token, o.Scope))
	}
	imports := make([]Import, len(o.Imports))
	names := make(map[string]bool, len(o.Imports))
	for i, imp := range o.Imports {
		if imp.Wire == nil {
			panic(fmt.Sprintf("typewirego.Of(%q): import %q has a nil wire", o.Token, imp.Name))
		}
		if names[imp.Name] {
			panic(fmt.Sprintf("typewirego.Of(%q): two imports named %q", o.Token, imp.Name))
		}
		names[imp.Name] = true
		imports[i] = imp
	}
	var creator Creator[T]
	if o.Creator != nil {
		leaf := o.Creator
		creator = func(ctx context.Context, _ Deps, _ Resolver) (T, error) { return leaf(ctx) }
	} else {
		with := o.CreateWith
		creator = func(ctx context.Context, d Deps, r Resolver) (T, error) {
			vals, err := resolveImports(ctx, imports, r)
			if err != nil {
				var zero T
				return zero, err
			}
			return with(ctx, Deps{vals: vals, life: d.life}, r)
		}
	}
	return newWire(&wireCore{
		token:    newToken(o.Token),
		scope:    scope,
		imports:  imports,
		keepOpen: o.KeepOpen,
	}, creator)
}

func newWire[T any](c *wireCore, creator Creator[T]) *wire[T] {
	w := &wire[T]{c: c, creator: creator}
	c.create = func(ctx context.Context, d Deps, r Resolver) (any, error) {
		v, err := creator(ctx, d, r)
		if err != nil {
			return nil, err
		}
		return any(v), nil
	}
	return w
}

func resolveImports(ctx context.Context, imports []Import, r Resolver) (map[Token]any, error) {
	vals := make(map[Token]any, len(imports))
	for _, imp := range imports {
		t := imp.Wire.Token()
		v, err := r.Resolve(ctx, t)
		if err != nil {
			return nil, err
		}
		vals[t] = v
	}
	return vals, nil
}

func (w *wire[T]) Token() Token { return w.c.token }

func (w *wire[T]) Scope() Scope { return w.c.scope }

func (w *wire[T]) Imports() []Import {
	out := make([]Import, len(w.c.imports))
	copy(out, w.c.imports)
	return out
}

func (w *wire[T]) core() *wireCore { return w.c }

func (w *wire[T]) Apply(b Binder) error { return applyWire(b, w) }

func applyWire(b Binder, w AnyWire) error {
	for _, imp := range w.core().imports {
		if b.IsBound(imp.Wire.Token()) {
			continue
		}
		if err := imp.Wire.Apply(b); err != nil {
			return err
		}
	}
	b.Bind(w)
	return nil
}

func (w *wire[T]) GetInstance(ctx context.Context, r Resolver) (T, error) {
	v, err := r.Resolve(ctx, w.c.token)
	if err != nil {
		var zero T
		return zero, err
	}
	return castValue[T](w.c.token, v)
}

func (w *wire[T]) WithCreator(fn func(ctx context.Context, imports Deps, r Resolver, original Creator[T]) (T, error)) TypeWire[T] {
	if fn == nil {
		panic(fmt.Sprintf("typewirego: WithCreator(%q): nil function", w.c.token.Label()))
	}
	original := w.creator
	creator := func(ctx context.Context, d Deps, r Resolver) (T, error) {
		return fn(ctx, Deps{life: d.life}, r, original)
	}
	return newWire(&wireCore{
		token:    w.c.token,
		scope:    w.c.scope,
		imports:  w.c.imports,
		keepOpen: w.c.keepOpen,
	}, creator)
}
