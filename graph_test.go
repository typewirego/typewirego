package typewirego_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/typewirego/typewirego"
)

// TestGraphGoldenScenario is the scenario Appendix D of the build prompt pins.
func TestGraphGoldenScenario(t *testing.T) {
	ctx := context.Background()
	cfgW := typewirego.Of(typewirego.Opts[string]{Token: "app.Config",
		Creator: func(context.Context) (string, error) { return "cfg", nil }})
	dbW := typewirego.Of(typewirego.Opts[string]{Token: "app.DB",
		Imports:    []typewirego.Import{{Name: "cfg", Wire: cfgW}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (string, error) { return "db", nil }})
	hW := typewirego.Of(typewirego.Opts[string]{Token: "app.Handler", Scope: typewirego.Transient,
		Imports:    []typewirego.Import{{Name: "db", Wire: dbW}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (string, error) { return "h", nil }})
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	_ = typewirego.GroupOf(hW).Apply(c)
	_, _ = dbW.GetInstance(ctx, c)
	_ = cfgW.WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[string]) (string, error) {
		return "o", nil
	}).Apply(c)
	g := c.Graph()
	wantDOT := "digraph wires {\n" +
		"  n1 [label=\"app.Config\"]\n" +
		"  n2 [label=\"app.DB\"]\n" +
		"  n3 [label=\"app.Handler\"]\n" +
		"  n1 -> n2\n" +
		"  n2 -> n3\n" +
		"}\n"
	if got := g.DOT(); got != wantDOT {
		t.Errorf("DOT:\n%s\nwant:\n%s", got, wantDOT)
	}
	wantJSON := `{"nodes":[{"id":"n1","label":"app.Config","scope":"singleton","created":true,"overridden":true,"observer_pulled":false},{"id":"n2","label":"app.DB","scope":"singleton","created":true,"overridden":false,"observer_pulled":false},{"id":"n3","label":"app.Handler","scope":"transient","created":false,"overridden":false,"observer_pulled":false}],"edges":[{"from":"n1","to":"n2"},{"from":"n2","to":"n3"}]}`
	got, err := g.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wantJSON {
		t.Errorf("JSON:\n%s\nwant:\n%s", got, wantJSON)
	}
}

func TestGraphOfAnEmptyContainerRendersEmptyLists(t *testing.T) {
	ctx := context.Background()
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	g := c.Graph()
	b, err := g.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"nodes":[],"edges":[]}` {
		t.Errorf("empty graph JSON = %s", b)
	}
	if b, _ := (typewirego.Graph{}).JSON(); string(b) != `{"nodes":[],"edges":[]}` {
		t.Errorf("zero Graph JSON = %s", b)
	}
	if g.DOT() != "digraph wires {\n}\n" {
		t.Errorf("empty graph DOT = %q", g.DOT())
	}
}

func TestGraphDOTAndJSONRenderDuplicateLabelsAsTwoNodes(t *testing.T) {
	ctx := context.Background()
	first := leafWire("service.Client")
	second := leafWire("service.Client")
	user := typewirego.Of(typewirego.Opts[string]{Token: "service.User",
		Imports:    []typewirego.Import{{Name: "client", Wire: second}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (string, error) { return "", nil }})
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	_ = typewirego.GroupOf(first, user).Apply(c)
	g := c.Graph()
	want := "digraph wires {\n" +
		"  n1 [label=\"service.Client#1\"]\n" +
		"  n2 [label=\"service.Client#2\"]\n" +
		"  n3 [label=\"service.User\"]\n" +
		"  n2 -> n3\n" +
		"}\n"
	if got := g.DOT(); got != want {
		t.Errorf("DOT:\n%s\nwant:\n%s", got, want)
	}
	b, _ := g.JSON()
	if strings.Count(string(b), `"label":"service.Client"`) != 2 {
		t.Errorf("JSON should keep both labels unchanged: %s", b)
	}
	if !strings.Contains(string(b), `"edges":[{"from":"n2","to":"n3"}]`) {
		t.Errorf("JSON edges: %s", b)
	}
}

func TestGraphSkipsAnImportThatWasNeverBound(t *testing.T) {
	ctx := context.Background()
	dep := leafWire("t.Dep")
	svc := typewirego.Of(typewirego.Opts[string]{Token: "t.Svc",
		Imports:    []typewirego.Import{{Name: "dep", Wire: dep}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (string, error) { return "", nil }})
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	c.Bind(svc) // Bind, unlike Apply, does not bind the import.
	g := c.Graph()
	if len(g.Nodes) != 1 || g.Nodes[0].Label != "t.Svc" {
		t.Fatalf("nodes = %+v", g.Nodes)
	}
	if len(g.Edges) != 0 {
		t.Fatalf("edges = %+v, want none", g.Edges)
	}
}

func TestGraphNodeOrderIsNumericPastNine(t *testing.T) {
	ctx := context.Background()
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	var ws []typewirego.TypeWire[string]
	for i := 1; i <= 12; i++ {
		ws = append(ws, leafWire(fmt.Sprintf("t.W%d", i)))
	}
	// w12 imports w1 and w10 imports w2, so edges sort by ordinal, not text.
	top := typewirego.Of(typewirego.Opts[string]{Token: "t.Top",
		Imports: []typewirego.Import{{Name: "w2", Wire: ws[1]}, {Name: "w10", Wire: ws[9]}, {Name: "w1", Wire: ws[0]}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (string, error) {
			return "", nil
		}})
	for _, w := range ws {
		c.Bind(w)
	}
	c.Bind(top)
	g := c.Graph()
	for i, n := range g.Nodes {
		if n.ID != fmt.Sprintf("n%d", i+1) {
			t.Fatalf("node %d has id %s", i, n.ID)
		}
	}
	var froms []string
	for _, e := range g.Edges {
		froms = append(froms, e.From)
	}
	if got := strings.Join(froms, ","); got != "n1,n2,n10" {
		t.Fatalf("edge order = %s, want n1,n2,n10", got)
	}
	if !strings.Contains(g.DOT(), "  n9 [label=\"t.W9\"]\n  n10 [label=\"t.W10\"]\n") {
		t.Fatalf("DOT node order:\n%s", g.DOT())
	}
}

func TestGraphReapplyingTheSameWireIsNotAnOverride(t *testing.T) {
	ctx := context.Background()
	w := leafWire("t.W")
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	_ = w.Apply(c)
	_ = w.Apply(c)
	if c.Graph().Nodes[0].Overridden {
		t.Fatal("re-applying the same wire marked it overridden")
	}
	_ = w.WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[string]) (string, error) {
		return "", nil
	}).Apply(c)
	if !c.Graph().Nodes[0].Overridden {
		t.Fatal("binding a different wire for the token is an override")
	}
}

func TestGraphMarksATransientCreatedOnce(t *testing.T) {
	ctx := context.Background()
	tr := typewirego.Of(typewirego.Opts[string]{Token: "t.Tr", Scope: typewirego.Transient,
		Creator: func(context.Context) (string, error) { return "x", nil }})
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)
	_ = tr.Apply(c)
	if c.Graph().Nodes[0].Created {
		t.Fatal("transient marked created before any resolve")
	}
	if _, err := tr.GetInstance(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !c.Graph().Nodes[0].Created {
		t.Fatal("transient not marked created after a resolve")
	}
}
