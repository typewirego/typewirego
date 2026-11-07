package typewirego_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/typewirego/typewirego"
)

// expectedGoCases is how many cases in cases.json run on Go. A driver that
// skips cases cannot pass.
const expectedGoCases = 34

// casesPath is the conformance case file: testdata/conformance/cases.json, or
// the path in TYPEWIRE_CONFORMANCE_CASES.
func casesPath() string {
	if p := os.Getenv("TYPEWIRE_CONFORMANCE_CASES"); p != "" {
		return p
	}
	return filepath.Join("testdata", "conformance", "cases.json")
}

type caseFile struct {
	Description string            `json:"description"`
	Languages   []string          `json:"languages"`
	Rationales  []string          `json:"rationales"`
	Ops         map[string]string `json:"ops"`
	Cases       []confCase        `json:"cases"`
}

type confCase struct {
	Name       string                       `json:"name"`
	Rules      []string                     `json:"rules"`
	Operations []map[string]json.RawMessage `json:"operations"`
	Languages  []string                     `json:"languages"`
	Rationale  *string                      `json:"rationale"`
}

// The keys each op may carry, besides "op".
var opKeys = map[string][]string{
	"define":        {"id", "label", "creator", "scope", "imports", "resolves", "value", "cleanup"},
	"override":      {"id", "keep_imports"},
	"apply":         {"ids"},
	"resolve":       {"id", "expect_calls", "expect_same_as", "expect_error", "expect_path"},
	"teardown":      {"expect_error", "expect_failed"},
	"creator_calls": {"expect"},
	"cleanup_order": {"expect"},
	"graph":         {"expect_nodes", "expect_edges"},
}

// ruleKeys are the entries of the ops object that describe rules, not ops.
var ruleKeys = map[string]bool{"state": true, "expect_error": true, "unknown": true}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func strictKeys(raw json.RawMessage, allowed ...string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range m {
		if !ok[k] {
			return fmt.Errorf("unknown key %q", k)
		}
	}
	return nil
}

// loadCases reads and validates a case file against its ops schema.
func loadCases(path string) (*caseFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCases(raw)
}

func parseCases(raw []byte) (*caseFile, error) {
	if err := strictKeys(raw, "description", "languages", "rationales", "ops", "cases"); err != nil {
		return nil, fmt.Errorf("case file: %w", err)
	}
	var cf caseFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&cf); err != nil {
		return nil, err
	}
	for name := range opKeys {
		if _, ok := cf.Ops[name]; !ok {
			return nil, fmt.Errorf("ops schema does not define op %q", name)
		}
	}
	for name := range cf.Ops {
		if _, ok := opKeys[name]; !ok && !ruleKeys[name] {
			return nil, fmt.Errorf("ops schema defines %q, which this driver does not know", name)
		}
	}
	rationales := map[string]bool{}
	for _, r := range cf.Rationales {
		rationales[r] = true
	}
	var rawCases struct {
		Cases []json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &rawCases); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for k, rc := range rawCases.Cases {
		if err := strictKeys(rc, "name", "rules", "operations", "languages", "rationale"); err != nil {
			return nil, fmt.Errorf("case %d: %w", k, err)
		}
		cs := cf.Cases[k]
		if cs.Name == "" || names[cs.Name] {
			return nil, fmt.Errorf("case %d: missing or duplicate name %q", k, cs.Name)
		}
		names[cs.Name] = true
		if cs.Languages != nil {
			if cs.Rationale == nil || !rationales[*cs.Rationale] {
				return nil, fmt.Errorf("case %q: languages without a permitted rationale", cs.Name)
			}
		} else if cs.Rationale != nil {
			return nil, fmt.Errorf("case %q: rationale without languages", cs.Name)
		}
		if err := validateOps(cs); err != nil {
			return nil, fmt.Errorf("case %q: %w", cs.Name, err)
		}
	}
	return &cf, nil
}

func validateOps(cs confCase) error {
	defined := map[string]bool{}
	use := func(id string) error {
		if !defined[id] {
			return fmt.Errorf("id %q used before it is defined", id)
		}
		return nil
	}
	useAll := func(ids []string) error {
		for _, id := range ids {
			if err := use(id); err != nil {
				return err
			}
		}
		return nil
	}
	var resolves []string
	for n, op := range cs.Operations {
		var name string
		if err := json.Unmarshal(op["op"], &name); err != nil {
			return fmt.Errorf("operation %d: no op", n)
		}
		keys, ok := opKeys[name]
		if !ok {
			return fmt.Errorf("operation %d: unknown op %q", n, name)
		}
		raw, _ := json.Marshal(op)
		if err := strictKeys(raw, append([]string{"op"}, keys...)...); err != nil {
			return fmt.Errorf("operation %d (%s): %w", n, name, err)
		}
		var o operation
		if err := json.Unmarshal(raw, &o); err != nil {
			return fmt.Errorf("operation %d (%s): %w", n, name, err)
		}
		var err error
		switch name {
		case "define":
			if !idPattern.MatchString(o.ID) {
				return fmt.Errorf("operation %d: bad id %q", n, o.ID)
			}
			if defined[o.ID] {
				return fmt.Errorf("operation %d: id %q defined twice", n, o.ID)
			}
			// resolves run when the creator runs, so they may name ids
			// defined later in the case; they are checked at the end.
			if err = useAll(o.Imports); err == nil {
				defined[o.ID] = true
				resolves = append(resolves, o.Resolves...)
			}
			switch o.Creator {
			case "ok", "fail", "value":
			default:
				err = fmt.Errorf("unknown creator %q", o.Creator)
			}
		case "override":
			err = use(o.ID)
		case "apply":
			err = useAll(o.IDs)
		case "resolve":
			if err = use(o.ID); err == nil && o.ExpectSameAs != "" {
				err = use(o.ExpectSameAs)
			}
		case "teardown":
			err = useAll(o.ExpectFailed)
		case "creator_calls":
			var expect map[string]int
			if err = json.Unmarshal(o.Expect, &expect); err == nil {
				for k := range expect {
					if err = use(strings.TrimSuffix(k, ".override")); err != nil {
						break
					}
				}
			}
		case "cleanup_order":
			var expect []string
			if err = json.Unmarshal(o.Expect, &expect); err == nil {
				err = useAll(expect)
			}
		case "graph":
			for _, nd := range o.ExpectNodes {
				if err = use(nd.ID); err != nil {
					break
				}
			}
			for _, e := range o.ExpectEdges {
				if err == nil {
					err = useAll([]string{e.From, e.To})
				}
			}
		}
		if err != nil {
			return fmt.Errorf("operation %d (%s): %w", n, name, err)
		}
	}
	for _, id := range resolves {
		if !defined[id] {
			return fmt.Errorf("resolves names %q, which is never defined", id)
		}
	}
	return nil
}

type operation struct {
	Op           string          `json:"op"`
	ID           string          `json:"id"`
	Label        string          `json:"label"`
	Creator      string          `json:"creator"`
	Scope        string          `json:"scope"`
	Imports      []string        `json:"imports"`
	Resolves     []string        `json:"resolves"`
	Value        json.RawMessage `json:"value"`
	Cleanup      string          `json:"cleanup"`
	KeepImports  *bool           `json:"keep_imports"`
	IDs          []string        `json:"ids"`
	ExpectCalls  *int            `json:"expect_calls"`
	ExpectSameAs string          `json:"expect_same_as"`
	ExpectError  string          `json:"expect_error"`
	ExpectPath   []string        `json:"expect_path"`
	ExpectFailed []string        `json:"expect_failed"`
	Expect       json.RawMessage `json:"expect"`
	ExpectNodes  []graphNode     `json:"expect_nodes"`
	ExpectEdges  []graphEdge     `json:"expect_edges"`
}

type graphNode struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Scope      string `json:"scope"`
	Created    bool   `json:"created"`
	Overridden bool   `json:"overridden"`
}

type graphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func goApplies(cs confCase) bool {
	if cs.Languages == nil {
		return true
	}
	for _, l := range cs.Languages {
		if l == "go" {
			return true
		}
	}
	return false
}

// okValue is the fresh value a creator "ok" returns; its cleanup logs its id.
type okValue struct {
	id   string
	fail bool
	run  *caseRun
}

func (v *okValue) Close() error {
	v.run.cleanups = append(v.run.cleanups, v.id)
	if v.fail {
		return &cleanupFailure{id: v.id}
	}
	return nil
}

// cleanupFailure is a case-local cleanup failure carrying its id.
type cleanupFailure struct{ id string }

func (e *cleanupFailure) Error() string { return "cleanup of " + e.id + " failed" }

func failedIDs(err error) []string {
	var out []string
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if cf, ok := err.(*cleanupFailure); ok {
			out = append(out, cf.id)
		}
		switch u := err.(type) {
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				walk(e)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	sort.Strings(out)
	return out
}

type caseRun struct {
	c        *typewirego.Container
	wires    map[string]typewirego.TypeWire[any]
	ids      []string
	calls    map[string]int
	cleanups []string
	last     map[string]any
}

func (r *caseRun) define(o operation) error {
	var imports []typewirego.Import
	for _, id := range o.Imports {
		imports = append(imports, typewirego.Import{Name: id, Wire: r.wires[id]})
	}
	var value any
	if o.Creator == "value" {
		if err := json.Unmarshal(o.Value, &value); err != nil && len(o.Value) > 0 {
			return err
		}
	}
	id, resolves, creator, fail := o.ID, o.Resolves, o.Creator, o.Cleanup == "fail"
	w := typewirego.Of(typewirego.Opts[any]{
		Token:   o.Label,
		Scope:   typewirego.Scope(o.Scope),
		Imports: imports,
		CreateWith: func(ctx context.Context, _ typewirego.Deps, res typewirego.Resolver) (any, error) {
			r.calls[id]++
			for _, rid := range resolves {
				if _, err := res.Resolve(ctx, r.wires[rid].Token()); err != nil {
					return nil, err
				}
			}
			switch creator {
			case "ok":
				return &okValue{id: id, fail: fail, run: r}, nil
			case "fail":
				return nil, fmt.Errorf("creator %s fails", id)
			}
			return value, nil
		},
	})
	r.wires[id] = w
	r.ids = append(r.ids, id)
	return nil
}

func matchesClass(err error, class string) bool {
	switch class {
	case "cycle":
		return errors.Is(err, typewirego.ErrCircularDependency)
	case "not_registered":
		return errors.Is(err, typewirego.ErrWireNotRegistered)
	case "creator":
		return errors.Is(err, typewirego.ErrCreator)
	case "closing":
		return errors.Is(err, typewirego.ErrClosing)
	case "cleanup":
		return err != nil && len(failedIDs(err)) > 0
	}
	return false
}

// runCase runs one case against a fresh container and returns the first
// mismatch.
func runCase(cs confCase) (err error) {
	ctx := context.Background()
	r := &caseRun{
		c:     typewirego.NewContainer(ctx),
		wires: map[string]typewirego.TypeWire[any]{},
		calls: map[string]int{},
		last:  map[string]any{},
	}
	tornDown := false
	defer func() {
		if !tornDown {
			r.c.Teardown(ctx)
		}
	}()
	for n, raw := range cs.Operations {
		b, _ := json.Marshal(raw)
		var o operation
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		fail := func(format string, args ...any) error {
			return fmt.Errorf("operation %d (%s): %s", n, o.Op, fmt.Sprintf(format, args...))
		}
		switch o.Op {
		case "define":
			if err := r.define(o); err != nil {
				return fail("%v", err)
			}
		case "override":
			if o.KeepImports == nil || !*o.KeepImports {
				return fail("keep_imports false is not supported in Go")
			}
			id := o.ID
			r.wires[id] = r.wires[id].WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[any]) (any, error) {
				r.calls[id+".override"]++
				return &okValue{id: id, run: r}, nil
			})
		case "apply":
			var ws []typewirego.AnyWire
			for _, id := range o.IDs {
				ws = append(ws, r.wires[id])
			}
			if err := typewirego.GroupOf(ws...).Apply(r.c); err != nil {
				return fail("apply: %v", err)
			}
		case "resolve":
			v, err := r.c.Resolve(ctx, r.wires[o.ID].Token())
			if o.ExpectError == "" && err != nil {
				return fail("resolve %s: unexpected error %v", o.ID, err)
			}
			if o.ExpectError != "" && !matchesClass(err, o.ExpectError) {
				return fail("resolve %s: error %v, want class %s", o.ID, err, o.ExpectError)
			}
			if o.ExpectCalls != nil && r.calls[o.ID] != *o.ExpectCalls {
				return fail("resolve %s: %d creator calls, want %d", o.ID, r.calls[o.ID], *o.ExpectCalls)
			}
			if o.ExpectPath != nil {
				var cycle *typewirego.CircularDependencyError
				if !errors.As(err, &cycle) || !reflect.DeepEqual(cycle.Path, o.ExpectPath) {
					return fail("resolve %s: cycle path %v, want %v", o.ID, cycle, o.ExpectPath)
				}
			}
			if o.ExpectSameAs != "" {
				prev, ok := r.last[o.ExpectSameAs]
				if !ok || prev != v {
					return fail("resolve %s: value is not the one last resolved for %s", o.ID, o.ExpectSameAs)
				}
			}
			if err == nil {
				r.last[o.ID] = v
			}
		case "teardown":
			tornDown = true
			err := r.c.Teardown(ctx)
			switch {
			case o.ExpectError == "" && err != nil:
				return fail("teardown: unexpected error %v", err)
			case o.ExpectError != "" && !matchesClass(err, o.ExpectError):
				return fail("teardown: error %v, want class %s", err, o.ExpectError)
			}
			if o.ExpectFailed != nil {
				want := append([]string(nil), o.ExpectFailed...)
				sort.Strings(want)
				if got := failedIDs(err); !reflect.DeepEqual(got, want) {
					return fail("teardown: failed cleanups %v, want %v", got, want)
				}
			}
		case "creator_calls":
			var expect map[string]int
			if err := json.Unmarshal(o.Expect, &expect); err != nil {
				return fail("%v", err)
			}
			for k, want := range expect {
				if r.calls[k] != want {
					return fail("%s: %d creator calls, want %d", k, r.calls[k], want)
				}
			}
		case "cleanup_order":
			var expect []string
			if err := json.Unmarshal(o.Expect, &expect); err != nil {
				return fail("%v", err)
			}
			got := r.cleanups
			if got == nil {
				got = []string{}
			}
			if !reflect.DeepEqual(got, expect) {
				return fail("cleanup order %v, want %v", got, expect)
			}
		case "graph":
			if err := r.checkGraph(o); err != nil {
				return fail("%v", err)
			}
		default:
			return fail("unknown op")
		}
	}
	return nil
}

func (r *caseRun) checkGraph(o operation) error {
	g := r.c.Graph()
	caseID := map[string]string{}
	for _, id := range r.ids {
		if nid, ok := typewirego.NodeIDOf(r.c, r.wires[id].Token()); ok {
			caseID[nid] = id
		}
	}
	var nodes []graphNode
	for _, n := range g.Nodes {
		id, ok := caseID[n.ID]
		if !ok {
			return fmt.Errorf("graph node %s has no case id", n.ID)
		}
		nodes = append(nodes, graphNode{ID: id, Label: n.Label, Scope: string(n.Scope),
			Created: n.Created, Overridden: n.Overridden})
	}
	edges := []graphEdge{}
	for _, e := range g.Edges {
		edges = append(edges, graphEdge{From: caseID[e.From], To: caseID[e.To]})
	}
	want := append([]graphNode{}, o.ExpectNodes...)
	wantEdges := append([]graphEdge{}, o.ExpectEdges...)
	sort.Slice(nodes, func(a, b int) bool { return nodes[a].ID < nodes[b].ID })
	sort.Slice(want, func(a, b int) bool { return want[a].ID < want[b].ID })
	sortEdges := func(es []graphEdge) {
		sort.Slice(es, func(a, b int) bool { return es[a].From+" "+es[a].To < es[b].From+" "+es[b].To })
	}
	sortEdges(edges)
	sortEdges(wantEdges)
	if !reflect.DeepEqual(nodes, want) {
		return fmt.Errorf("graph nodes %+v, want %+v", nodes, want)
	}
	if !reflect.DeepEqual(edges, wantEdges) {
		return fmt.Errorf("graph edges %+v, want %+v", edges, wantEdges)
	}
	return nil
}

func TestConformanceCases(t *testing.T) {
	cf, err := loadCases(casesPath())
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, cs := range cf.Cases {
		if !goApplies(cs) {
			continue
		}
		ran++
		t.Run(cs.Name, func(t *testing.T) {
			if err := runCase(cs); err != nil {
				t.Fatal(err)
			}
		})
	}
	if ran != expectedGoCases {
		t.Fatalf("ran %d Go cases, want %d", ran, expectedGoCases)
	}
}

func TestConformanceDriverFailsACaseThatContradictsTheLibrary(t *testing.T) {
	raw := `{"description":"","languages":["go"],"rationales":["capability-absent"],"ops":` + opsJSON(t) + `,"cases":[
	{"name":"wrong","rules":["P3"],"operations":[
		{"op":"define","id":"s","label":"t.S","creator":"ok"},
		{"op":"apply","ids":["s"]},
		{"op":"resolve","id":"s","expect_calls":1},
		{"op":"resolve","id":"s","expect_calls":2}]}]}`
	cf, err := parseCases([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := runCase(cf.Cases[0]); err == nil {
		t.Fatal("a case contradicting the library passed")
	}
}

func opsJSON(t *testing.T) string {
	t.Helper()
	cf, err := loadCases(filepath.Join("testdata", "conformance", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cf.Ops)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConformanceDriverRejectsMalformedInput(t *testing.T) {
	ops := opsJSON(t)
	file := func(cases string) string {
		return `{"description":"","languages":["go"],"rationales":["capability-absent"],"ops":` + ops + `,"cases":[` + cases + `]}`
	}
	define := `{"op":"define","id":"a","label":"t.A","creator":"ok"}`
	bad := map[string]string{
		"unknown op":           file(`{"name":"x","rules":["P1"],"operations":[` + define + `,{"op":"explode","id":"a"}]}`),
		"unknown op key":       file(`{"name":"x","rules":["P1"],"operations":[{"op":"define","id":"a","label":"t.A","creator":"ok","colour":"red"}]}`),
		"key of another op":    file(`{"name":"x","rules":["P1"],"operations":[` + define + `,{"op":"resolve","id":"a","ids":["a"]}]}`),
		"unknown case key":     file(`{"name":"x","rules":["P1"],"priority":1,"operations":[` + define + `]}`),
		"languages, no reason": file(`{"name":"x","rules":["P1"],"languages":["go"],"operations":[` + define + `]}`),
		"unknown rationale":    file(`{"name":"x","rules":["P1"],"languages":["go"],"rationale":"because","operations":[` + define + `]}`),
		"id defined twice":     file(`{"name":"x","rules":["P1"],"operations":[` + define + `,` + define + `]}`),
		"id used before define": file(`{"name":"x","rules":["P1"],"operations":[` +
			`{"op":"define","id":"b","label":"t.B","creator":"ok","imports":["a"]},` + define + `]}`),
		"resolve of undefined": file(`{"name":"x","rules":["P1"],"operations":[{"op":"resolve","id":"a"}]}`),
		"bad id":               file(`{"name":"x","rules":["P1"],"operations":[{"op":"define","id":"A","label":"t.A","creator":"ok"}]}`),
		"unknown top key":      strings.Replace(file(`{"name":"x","rules":["P1"],"operations":[`+define+`]}`), `"description"`, `"extra":1,"description"`, 1),
	}
	for name, raw := range bad {
		if _, err := parseCases([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := file(`{"name":"x","rules":["P1"],"operations":[` + define + `,{"op":"apply","ids":["a"]},{"op":"resolve","id":"a"}]}`)
	if _, err := parseCases([]byte(good)); err != nil {
		t.Fatalf("well-formed file rejected: %v", err)
	}
}
