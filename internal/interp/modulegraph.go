package interp

import (
	"path/filepath"
	"sort"

	"github.com/fadion/aria/internal/source"
	"github.com/fadion/aria/internal/value"
)

// ModuleState is where a node is in one top-level execution's lifecycle.
//
// It is the queryable failure boundary the module graph exists to provide: a
// caller can ask, after a run, which modules parsed, resolved and evaluated,
// rather than inferring all of it from diagnostics.
type ModuleState int

const (
	// ModulePending has been discovered as an import target, but its file has
	// not been parsed yet.
	ModulePending ModuleState = iota
	// ModuleParsed parsed, but the graph has not been resolved as one
	// compilation yet.
	ModuleParsed
	// ModuleResolved resolved with the rest of the graph, without being
	// evaluated yet.
	ModuleResolved
	// ModuleEvaluating is running its top-level code right now. Nothing stays
	// in this state: success moves to ModuleReady, failure to ModuleFailed.
	ModuleEvaluating
	// ModuleReady evaluated successfully; its exports are in the cache.
	ModuleReady
	// ModuleFailed did not make it through loading, resolution or evaluation.
	// Failure carries the categorized reason.
	ModuleFailed
)

func (s ModuleState) String() string {
	switch s {
	case ModulePending:
		return "pending"
	case ModuleParsed:
		return "parsed"
	case ModuleResolved:
		return "resolved"
	case ModuleEvaluating:
		return "evaluating"
	case ModuleReady:
		return "ready"
	case ModuleFailed:
		return "failed"
	}
	return "unknown"
}

// ImportErrorKind categorizes a module failure.
//
// The four cases a program can provoke deliberately read differently: an alias
// that names two sources, a file importing itself, an indirect cycle whose
// initialization order cannot satisfy a name, and an ordinary runtime fault
// raised while a module's top level runs. Loading, parse and resolve failures
// are categorized too, so every failed node answers the same question.
type ImportErrorKind int

const (
	// ImportErrNone is the absence of a failure.
	ImportErrNone ImportErrorKind = iota
	// ImportErrRead is a file the import names that cannot be read.
	ImportErrRead
	// ImportErrParse is a module whose file did not parse.
	ImportErrParse
	// ImportErrResolve is a module whose names did not resolve.
	ImportErrResolve
	// ImportErrAliasConflict is one alias asked to name two different sources.
	ImportErrAliasConflict
	// ImportErrSelfImport is a file that imports itself directly.
	ImportErrSelfImport
	// ImportErrCycle is an indirect import cycle that left a name unavailable
	// while one of its members evaluated.
	ImportErrCycle
	// ImportErrRuntime is a runtime fault raised while a module evaluated.
	ImportErrRuntime
)

func (k ImportErrorKind) String() string {
	switch k {
	case ImportErrRead:
		return "read"
	case ImportErrParse:
		return "parse"
	case ImportErrResolve:
		return "resolve"
	case ImportErrAliasConflict:
		return "alias conflict"
	case ImportErrSelfImport:
		return "self import"
	case ImportErrCycle:
		return "import cycle"
	case ImportErrRuntime:
		return "runtime error"
	}
	return "none"
}

// ImportError is one categorized module failure.
//
// Span is the span of the import responsible, in Importer's text — the place
// the failure entered this compilation — so every category points at an import
// statement. The underlying runtime fault keeps its own span and file through
// Unwrap and renders exactly where it occurred.
type ImportError struct {
	Kind     ImportErrorKind
	ID       string
	Span     source.Span
	Importer *source.File
	Msg      string
	Err      error
	// Cycle is the cycle path, when Kind is ImportErrCycle.
	Cycle []string
}

func (e *ImportError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Msg
}

// Unwrap exposes the underlying fault, for errors.As against *Error.
func (e *ImportError) Unwrap() error { return e.Err }

// ModuleNode is one source file in the module graph, identified by its
// canonical source id rather than the import text that reached it.
type ModuleNode struct {
	ID    string
	Path  string
	Entry bool
	State ModuleState
	// Executions is how many times this node's top-level code has been
	// evaluated; a cache hit is not an execution. A failed attempt still
	// counts: it is how far the file got before the failure.
	Executions int
}

// ModuleEdge is one import statement: From imports To.
type ModuleEdge struct {
	From  string
	To    string
	Alias string
	Span  source.Span
}

// ModuleGraph is the candidate graph of one top-level execution, accumulated
// across the executions that share an Interp.
//
// Nodes are keyed by canonical source id, so the same file reached through
// different relative paths is one node, and two files sharing a basename are
// two. Edges retain every import statement, including the alias it asked for.
type ModuleGraph struct {
	nodes  map[string]*ModuleNode
	order  []string
	edges  []ModuleEdge
	failed map[string]*ImportError
	cycles [][]string
}

func newModuleGraph() *ModuleGraph {
	return &ModuleGraph{nodes: map[string]*ModuleNode{}, failed: map[string]*ImportError{}}
}

func (g *ModuleGraph) ensureNode(id, path string, entry bool) *ModuleNode {
	if n, ok := g.nodes[id]; ok {
		if entry {
			n.Entry = true
		}
		return n
	}
	n := &ModuleNode{ID: id, Path: path, Entry: entry, State: ModulePending}
	g.nodes[id] = n
	g.order = append(g.order, id)
	return n
}

func (g *ModuleGraph) node(id string) *ModuleNode { return g.nodes[id] }

func (g *ModuleGraph) addEdge(from, to, alias string, span source.Span) {
	g.edges = append(g.edges, ModuleEdge{From: from, To: to, Alias: alias, Span: span})
}

func (g *ModuleGraph) setState(id string, state ModuleState) {
	if n, ok := g.nodes[id]; ok {
		n.State = state
	}
}

func (g *ModuleGraph) fail(id string, e *ImportError) {
	g.failed[id] = e
	g.setState(id, ModuleFailed)
}

// recordCycle stores a DFS back-edge path. The path is the ids along the cycle,
// ending where it started; rotations of the same cycle are stored once.
func (g *ModuleGraph) recordCycle(path []string) {
	for _, existing := range g.cycles {
		if sameCycle(existing, path) {
			return
		}
	}
	g.cycles = append(g.cycles, append([]string(nil), path...))
}

// sameCycle compares two closed cycle paths up to rotation.
func sameCycle(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	n := len(a)
	for offset := 0; offset < n; offset++ {
		match := true
		for k := 0; k < n; k++ {
			if a[k] != b[(offset+k)%n] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Node returns a copy of the node id identifies.
func (g *ModuleGraph) Node(id string) (ModuleNode, bool) {
	n, ok := g.nodes[id]
	if !ok {
		return ModuleNode{}, false
	}
	return *n, true
}

// Nodes returns every node in discovery order.
func (g *ModuleGraph) Nodes() []ModuleNode {
	out := make([]ModuleNode, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, *g.nodes[id])
	}
	return out
}

// Edges returns every import edge in the order it was seen.
func (g *ModuleGraph) Edges() []ModuleEdge {
	return append([]ModuleEdge(nil), g.edges...)
}

// EdgesFrom returns the import statements written in node id.
func (g *ModuleGraph) EdgesFrom(id string) []ModuleEdge {
	var out []ModuleEdge
	for _, e := range g.edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

// State returns a node's state.
func (g *ModuleGraph) State(id string) (ModuleState, bool) {
	n, ok := g.nodes[id]
	if !ok {
		return ModulePending, false
	}
	return n.State, true
}

// Failure returns the categorized failure recorded for a node.
func (g *ModuleGraph) Failure(id string) *ImportError { return g.failed[id] }

// Cycles returns the recorded import cycles, each a closed path of ids.
func (g *ModuleGraph) Cycles() [][]string {
	out := make([][]string, len(g.cycles))
	for i, c := range g.cycles {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// CyclePath returns a recorded cycle that contains id, as a closed path.
func (g *ModuleGraph) CyclePath(id string) []string {
	for _, c := range g.cycles {
		for _, n := range c {
			if n == id {
				return append([]string(nil), c...)
			}
		}
	}
	return nil
}

// cacheEntry is one module's published exports.
type cacheEntry struct {
	exports map[string]value.Value
	order   []string
}

// ModuleCache holds the exports of modules that evaluated successfully.
//
// One cache belongs to one Interp — and so to one Session — so concurrent
// sessions never share mutable initialization state. Entries are published only
// after a module's whole top level finished, and a failed round revokes every
// entry it published, so a retry can never read a half-initialized module.
type ModuleCache struct {
	entries map[string]*cacheEntry
}

func newModuleCache() *ModuleCache {
	return &ModuleCache{entries: map[string]*cacheEntry{}}
}

func (c *ModuleCache) put(id string, exports map[string]value.Value, order []string) {
	cp := &cacheEntry{exports: map[string]value.Value{}, order: append([]string(nil), order...)}
	for _, name := range order {
		cp.exports[name] = exports[name]
	}
	c.entries[id] = cp
}

func (c *ModuleCache) get(id string) (*cacheEntry, bool) {
	e, ok := c.entries[id]
	return e, ok
}

func (c *ModuleCache) revoke(id string) { delete(c.entries, id) }

// Has reports whether id has published exports.
func (c *ModuleCache) Has(id string) bool { _, ok := c.entries[id]; return ok }

// Exports lists a cached module's export names in declaration order.
func (c *ModuleCache) Exports(id string) ([]string, bool) {
	e, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	return append([]string(nil), e.order...), true
}

// Lookup returns one cached export value.
func (c *ModuleCache) Lookup(id, name string) (value.Value, bool) {
	e, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	v, found := e.exports[name]
	return v, found
}

// IDs lists the cached module ids in sorted order.
func (c *ModuleCache) IDs() []string {
	out := make([]string, 0, len(c.entries))
	for id := range c.entries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// cachedAlias is an alias requested for a module whose exports are already
// cached from an earlier round: it needs no execution, only republishing.
type cachedAlias struct {
	id    string
	alias string
}

func (c *cacheEntry) module(name string) *Module {
	m := &Module{Name: name, members: map[string]value.Value{}}
	for _, n := range c.order {
		m.order = append(m.order, n)
		m.members[n] = c.exports[n]
	}
	return m
}

// ModuleID returns the canonical source id a module is identified by.
//
// Identity is the resolved absolute path — symlinks included — never the import
// text. "g", "./g" and "sub/../g" from the same directory name the same module;
// "lib/g" and "other/g" do not become one just because their basenames agree.
func ModuleID(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
