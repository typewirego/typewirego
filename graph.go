package typewirego

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Graph is the container's dependency graph: one node per bound token and one
// edge per declared import.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node is one bound token. ID is the token's registration ordinal (n1, n2,
// …), stable for the container's life; Label is the token's label, which two
// tokens may share. Created reports a cached singleton or a transient created
// at least once; Overridden reports a token bound again by a different wire;
// ObserverPulled marks a member of the observer set, which Teardown cleans
// last: never a transient, and never a value held back for a late creation.
type Node struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Scope          Scope  `json:"scope"`
	Created        bool   `json:"created"`
	Overridden     bool   `json:"overridden"`
	ObserverPulled bool   `json:"observer_pulled"`
}

// Edge runs from an import to the node that declares it, by node id.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph returns the dependency graph as declared by the bound wires, nodes
// in registration order and edges by their endpoints' registration order.
// An import whose token was never bound has no node and so no edge.
func (c *Container) Graph() Graph {
	c.mu.Lock()
	defer c.mu.Unlock()
	pulled := map[*slot]bool{}
	for i := range c.observerSetLocked() {
		pulled[i.slot] = true
	}
	g := Graph{Nodes: make([]Node, 0, len(c.order)), Edges: []Edge{}}
	type edge struct{ from, to int }
	var edges []edge
	for _, s := range c.order {
		created := s.cached != nil
		if s.core.scope == Transient {
			created = s.createdOnce
		}
		g.Nodes = append(g.Nodes, Node{ID: s.id, Label: s.token.Label(), Scope: s.core.scope,
			Created: created, Overridden: s.overridden, ObserverPulled: pulled[s]})
		seen := map[int]bool{}
		for _, imp := range s.core.imports {
			from := c.slots[imp.Wire.Token()]
			if from == nil || seen[from.ordinal] {
				continue
			}
			seen[from.ordinal] = true
			edges = append(edges, edge{from.ordinal, s.ordinal})
		}
	}
	sort.Slice(edges, func(a, b int) bool {
		if edges[a].from != edges[b].from {
			return edges[a].from < edges[b].from
		}
		return edges[a].to < edges[b].to
	})
	for _, e := range edges {
		g.Edges = append(g.Edges, Edge{From: fmt.Sprintf("n%d", e.from), To: fmt.Sprintf("n%d", e.to)})
	}
	return g
}

// DOT renders the graph in Graphviz DOT. Labels shared by several nodes get a
// #1, #2, … suffix in node order, so each node reads distinctly.
func (g Graph) DOT() string {
	count := map[string]int{}
	for _, n := range g.Nodes {
		count[n.Label]++
	}
	seen := map[string]int{}
	var b strings.Builder
	b.WriteString("digraph wires {\n")
	for _, n := range g.Nodes {
		label := n.Label
		if count[label] > 1 {
			seen[label]++
			label = fmt.Sprintf("%s#%d", label, seen[label])
		}
		fmt.Fprintf(&b, "  %s [label=%s", n.ID, strconv.Quote(label))
		if n.ObserverPulled {
			b.WriteString(", observer_pulled=true")
		}
		b.WriteString("]\n")
	}
	for _, e := range g.Edges {
		fmt.Fprintf(&b, "  %s -> %s\n", e.From, e.To)
	}
	b.WriteString("}\n")
	return b.String()
}

// JSON renders the graph as {"nodes": [...], "edges": [...]}.
func (g Graph) JSON() ([]byte, error) {
	if g.Nodes == nil {
		g.Nodes = []Node{}
	}
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	return json.Marshal(g)
}
