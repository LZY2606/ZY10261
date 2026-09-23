package interp

import (
	"os"
	"path/filepath"

	"github.com/fadion/aria/internal/ast"
	"github.com/fadion/aria/internal/diag"
	"github.com/fadion/aria/internal/parser"
	"github.com/fadion/aria/internal/source"
	"github.com/fadion/aria/internal/value"
)

// A unit is one file of a program. The entry file and everything it imports,
// directly or not, are units of the same compilation — which is what lets the
// resolver see all of it.
//
// Before this, an imported file was parsed and evaluated and never resolved, so
// inside one an undefined name was a runtime error, `let` immutability was
// unenforced, and every guarantee the resolver provides was absent. And a single
// `import` anywhere in a file turned undefined-name checking off for the whole
// of the importing file too, because the resolver could not see what the import
// had brought in.
type unit struct {
	// id is the canonical source id the graph knows the module under.
	id   string
	file *source.File
	prog *ast.Program
	bag  *diag.Bag
	// alias is the name an `import ... as Name` gives the file's own top-level
	// bindings. Empty for a plain import, whose names join the importing scope.
	alias string
	// importSpan is the import statement in the importing file that pulled
	// this unit into the graph. A failure is categorized against it.
	importSpan source.Span
	importer   *source.File
	// cyclic marks a unit that lies on a recorded import cycle.
	cyclic bool
}

// loadUnits builds the candidate graph for one top-level execution: it parses
// every file prog imports, directly or not, keyed by canonical source id.
//
// The walk is depth first, so the returned units are post-ordered — an imported
// file's own imports precede it, which is the order their names have to become
// visible. The entry unit is not included; it is the caller's, already parsed.
//
// Modules successfully evaluated by an earlier execution are already cached:
// their files are not read or parsed again. An alias first used for a cached
// module is returned separately, since it still has to be republished and
// resolved.
func (i *Interp) loadUnits(file *source.File, prog *ast.Program, bag *diag.Bag) ([]unit, []cachedAlias, bool) {
	l := &graphLoader{interp: i, seen: map[string]bool{}, aliases: map[string]map[string]string{}}

	entryID := ModuleID(file.Name)
	i.graph.ensureNode(entryID, file.Name, true)
	// The entry is already being compiled: an import that reaches it again
	// closes a cycle rather than pulling it in a second time.
	l.seen[entryID] = true
	if !l.walk(entryID, filepath.Dir(file.Name), file, prog, bag) {
		return l.units, l.cachedAliases, false
	}
	return l.units, l.cachedAliases, true
}

// graphLoader is one candidate-graph build. It carries no state outside the
// Interp it belongs to: every session builds its own graph and cache.
type graphLoader struct {
	interp *Interp
	// seen holds the canonical id of every node already entered on this walk,
	// which is what makes a cycle terminate: a second edge to it is recorded,
	// but the node is not entered twice.
	seen    map[string]bool
	stack   []string
	units   []unit
	aliases map[string]map[string]string

	cachedAliases []cachedAlias
}

func (l *graphLoader) walk(id, dir string, file *source.File, prog *ast.Program, bag *diag.Bag) bool {
	l.stack = append(l.stack, id)
	defer func() { l.stack = l.stack[:len(l.stack)-1] }()

	for _, n := range prog.Nodes {
		imp, ok := n.(*ast.Import)
		if !ok {
			continue
		}
		if !l.load(id, dir, file, imp, bag) {
			return false
		}
	}
	return true
}

func (l *graphLoader) load(importerID, dir string, importerFile *source.File, imp *ast.Import, bag *diag.Bag) bool {
	name := imp.File
	if filepath.Ext(name) == "" {
		name += ".ari"
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, name)
	}
	id := ModuleID(path)

	alias := ""
	if imp.Alias != nil {
		alias = imp.Alias.Value
	}

	// A direct self-import has nothing it could bring: the file's own names are
	// compiled as the importing file anyway. It is its own category rather than
	// a silent no-op, since a reader usually meant another file.
	if id == importerID {
		l.interp.graph.fail(id, &ImportError{
			Kind: ImportErrSelfImport, ID: id, Span: imp.Span(), Importer: importerFile,
			Msg: "module imports itself",
		})
		bag.Errorf(imp.Span(), "module '%s' imports itself", imp.File)
		return false
	}

	// An alias is a global name: one alias cannot name two different sources,
	// neither within one importing file nor against an earlier execution of the
	// same session. The same source may be re-aliased only with the same alias.
	if alias != "" {
		if byFile := l.aliases[importerID]; byFile != nil {
			if prev := byFile[alias]; prev != "" && prev != id {
				l.interp.graph.fail(id, &ImportError{
					Kind: ImportErrAliasConflict, ID: id, Span: imp.Span(), Importer: importerFile,
					Msg: "alias " + alias + " names another module",
				})
				bag.Errorf(imp.Span(), "alias '%s' already refers to another module in this file", alias)
				return false
			}
		} else {
			l.aliases[importerID] = map[string]string{}
		}
		l.aliases[importerID][alias] = id

		if prev := l.interp.aliasSource[alias]; prev != "" && prev != id {
			l.interp.graph.fail(id, &ImportError{
				Kind: ImportErrAliasConflict, ID: id, Span: imp.Span(), Importer: importerFile,
				Msg: "alias " + alias + " names another module",
			})
			bag.Errorf(imp.Span(), "alias '%s' already refers to another module", alias)
			return false
		}
	}

	l.interp.graph.ensureNode(id, path, false)
	l.interp.graph.addEdge(importerID, id, alias, imp.Span())

	// Published by an earlier execution: nothing is read, parsed, resolved or
	// executed again. A new alias over it is republished from the cache, which
	// is the only thing it still needs.
	if l.interp.cache.Has(id) {
		l.interp.graph.setState(id, ModuleReady)
		if alias != "" && l.interp.aliasSource[alias] != id {
			l.cachedAliases = append(l.cachedAliases, cachedAlias{id: id, alias: alias})
		}
		return true
	}

	if l.seen[id] {
		// A back edge into the current DFS stack closes a cycle. Cycles are
		// legal — the files are one compilation either way — but the path is
		// recorded, so the cycle is an inspectable result rather than a private
		// fact of the loader.
		for at, onStack := range l.stack {
			if onStack == id {
				cycle := append(append([]string(nil), l.stack[at:]...), id)
				l.interp.graph.recordCycle(cycle)
				return true
			}
		}
		return true
	}
	l.seen[id] = true

	src, err := os.ReadFile(path)
	if err != nil {
		l.interp.graph.fail(id, &ImportError{
			Kind: ImportErrRead, ID: id, Span: imp.Span(), Importer: importerFile,
			Msg: "cannot read imported file",
		})
		bag.Errorf(imp.Span(), "cannot read imported file '%s'", imp.File)
		return false
	}

	file := source.NewFile(path, src)
	unitBag := diag.New(file)
	unitProg := parser.New(file, unitBag).Parse()
	if unitBag.HasErrors() {
		l.interp.graph.fail(id, &ImportError{
			Kind: ImportErrParse, ID: id, Span: imp.Span(), Importer: importerFile,
			Msg: "module failed to parse",
		})
		// The unit is still returned: its bag carries its own diagnostics,
		// which render against its own text.
		l.units = append(l.units, unit{id: id, file: file, prog: unitProg, bag: unitBag,
			alias: alias, importSpan: imp.Span(), importer: importerFile})
		return false
	}
	l.interp.graph.setState(id, ModuleParsed)

	// Depth first: what this file imports has to be in scope before it is.
	if !l.walk(id, filepath.Dir(path), file, unitProg, unitBag) {
		l.units = append(l.units, unit{id: id, file: file, prog: unitProg, bag: unitBag,
			alias: alias, importSpan: imp.Span(), importer: importerFile})
		return false
	}

	u := unit{id: id, file: file, prog: unitProg, bag: unitBag,
		alias: alias, importSpan: imp.Span(), importer: importerFile,
		cyclic: l.interp.graph.CyclePath(id) != nil}
	l.units = append(l.units, u)
	return true
}

// evalUnits runs each imported unit before the program that imported it,
// publishing exports transactionally.
//
// The same interpreter runs all of them, rather than a sub-interpreter per
// file. A separate Interp had its own signal field, so a stray top-level
// `return` or `break` in an imported file set a copy nobody read — it neither
// propagated nor errored. The resolver rejects those now, and there is no
// second copy to lose them in either way.
//
// Each unit's exports enter the cache only after its whole top level has
// finished, so a failing module never publishes half-initialized exports. If
// any unit fails, everything published by this execution is revoked; modules
// cached by earlier executions stay.
func (i *Interp) evalUnits(units []unit, cachedAliases []cachedAlias) error {
	outer := i.file
	defer func() { i.file = outer }()

	var published []string
	var aliased []aliasBackup

	rollback := func() {
		for _, id := range published {
			i.cache.revoke(id)
		}
		for _, b := range aliased {
			if b.existed {
				i.modules[b.name] = b.module
				i.globals.define(b.name, b.module)
			} else {
				delete(i.modules, b.name)
				delete(i.globals.vars, b.name)
				delete(i.aliasSource, b.name)
			}
		}
	}

	// Aliases over cached modules publish nothing new in the cache, but their
	// module values are part of this execution and roll back with it.
	for _, ca := range cachedAliases {
		entry, ok := i.cache.get(ca.id)
		if !ok {
			continue
		}
		if err := i.publishAlias(ca.alias, ca.id, entry.module(ca.alias), &aliased); err != nil {
			rollback()
			return err
		}
	}

	for _, u := range units {
		i.file = u.file

		// Every unit evaluates in a scope of its own, a child of globals. For
		// an aliased import that scope becomes the module, which is all the
		// machinery aliasing needs. For a plain import the scope's bindings
		// merge into globals — but only once the whole unit has evaluated, so
		// a unit that fails halfway publishes none of its names and a later
		// retry never meets a half-initialized export.
		scope := newEnv(i.globals)

		i.graph.setState(u.id, ModuleEvaluating)
		if node := i.graph.node(u.id); node != nil {
			node.Executions++
		}
		if _, err := i.runNodes(u.prog, scope); err != nil {
			rollback()
			kind := ImportErrRuntime
			var cycle []string
			if u.cyclic {
				kind = ImportErrCycle
				cycle = i.graph.CyclePath(u.id)
			}
			ierr := &ImportError{
				Kind: kind, ID: u.id, Span: u.importSpan, Importer: u.importer,
				Msg: err.Error(), Err: err, Cycle: cycle,
			}
			i.graph.fail(u.id, ierr)
			return ierr
		}

		exports, order := snapshotExports(u.prog, scope)
		i.cache.put(u.id, exports, order)
		published = append(published, u.id)
		i.graph.setState(u.id, ModuleReady)

		if u.alias != "" {
			if err := i.publishAlias(u.alias, u.id, i.buildModule(u.alias, exports, order), &aliased); err != nil {
				rollback()
				return err
			}
			continue
		}
		for name, v := range scope.vars {
			i.globals.define(name, v)
		}
	}
	return nil
}

// aliasBackup remembers how to undo one alias publication.
type aliasBackup struct {
	name    string
	existed bool
	module  *Module
}

// publishAlias defines an alias module, remembering the previous binding of
// the name so a rolled-back execution restores it.
func (i *Interp) publishAlias(name, id string, m *Module, aliased *[]aliasBackup) error {
	backup := aliasBackup{name: name}
	if prev, ok := i.modules[name]; ok {
		backup.existed, backup.module = true, prev
	}
	i.modules[name] = m
	i.globals.define(name, m)
	i.aliasSource[name] = id
	*aliased = append(*aliased, backup)
	return nil
}

// snapshotExports reads a unit's top-level let/var names out of the scope it
// evaluated in, in declaration order.
func snapshotExports(prog *ast.Program, scope *env) (map[string]value.Value, []string) {
	exports := map[string]value.Value{}
	var order []string
	for _, n := range prog.Nodes {
		var bound *ast.Identifier
		switch n := n.(type) {
		case *ast.Let:
			bound = n.Name
		case *ast.Var:
			bound = n.Name
		}
		if bound == nil {
			continue
		}
		if v, ok := scope.vars[bound.Value]; ok {
			if _, seen := exports[bound.Value]; !seen {
				order = append(order, bound.Value)
			}
			exports[bound.Value] = v
		}
	}
	return exports, order
}

// buildModule turns an evaluated unit's exports into a module value.
func (i *Interp) buildModule(name string, exports map[string]value.Value, order []string) *Module {
	m := &Module{Name: name, members: map[string]value.Value{}}
	for _, n := range order {
		m.order = append(m.order, n)
		m.members[n] = exports[n]
	}
	return m
}
