package typewirego

// Group is an immutable ordered list of wires applied as a unit.
type Group interface {
	Wires() []AnyWire
	Apply(b Binder) error
	WithExtraWires(extra ...AnyWire) Group
}

type group struct {
	wires []AnyWire
}

// GroupOf returns a group of wires, applied in order. A nil wire panics.
func GroupOf(wires ...AnyWire) Group {
	out := make([]AnyWire, len(wires))
	for i, w := range wires {
		if w == nil {
			panic("typewirego.GroupOf: nil wire")
		}
		out[i] = w
	}
	return &group{wires: out}
}

// CombineGroups flattens groups into one, keeping their order.
func CombineGroups(groups ...Group) Group {
	var out []AnyWire
	for _, g := range groups {
		if g == nil {
			panic("typewirego.CombineGroups: nil group")
		}
		out = append(out, g.Wires()...)
	}
	return &group{wires: out}
}

func (g *group) Wires() []AnyWire {
	out := make([]AnyWire, len(g.wires))
	copy(out, g.wires)
	return out
}

func (g *group) Apply(b Binder) error {
	for _, w := range g.wires {
		if err := w.Apply(b); err != nil {
			return err
		}
	}
	return nil
}

func (g *group) WithExtraWires(extra ...AnyWire) Group {
	out := make([]AnyWire, 0, len(g.wires)+len(extra))
	out = append(out, g.wires...)
	for _, w := range extra {
		if w == nil {
			panic("typewirego: WithExtraWires: nil wire")
		}
		out = append(out, w)
	}
	return &group{wires: out}
}
