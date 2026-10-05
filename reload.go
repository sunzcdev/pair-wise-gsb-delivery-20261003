package lua

import (
	"fmt"
	"strings"
)

type moduleGraph struct {
	dependencies map[string]map[string]struct{}
}

type moduleLoad struct {
	name   string
	cached LValue
}

func newModuleGraph() *moduleGraph {
	return &moduleGraph{dependencies: make(map[string]map[string]struct{})}
}

func (g *moduleGraph) clone() *moduleGraph {
	clone := newModuleGraph()
	for module, dependencies := range g.dependencies {
		clone.dependencies[module] = make(map[string]struct{}, len(dependencies))
		for dependency := range dependencies {
			clone.dependencies[module][dependency] = struct{}{}
		}
	}
	return clone
}

func (g *moduleGraph) restore(snapshot *moduleGraph) {
	g.dependencies = snapshot.dependencies
}

func (g *moduleGraph) addDependency(module, dependency string) {
	dependencies := g.dependencies[module]
	if dependencies == nil {
		dependencies = make(map[string]struct{})
		g.dependencies[module] = dependencies
	}
	dependencies[dependency] = struct{}{}
}

func (g *moduleGraph) removeOutgoing(module string) {
	delete(g.dependencies, module)
}

func (g *moduleGraph) remove(module string) {
	delete(g.dependencies, module)
	for dependencies := range g.dependencies {
		delete(g.dependencies[dependencies], module)
	}
}

func (g *moduleGraph) dependents(module string) map[string]struct{} {
	result := make(map[string]struct{})
	queue := []string{module}
	for len(queue) > 0 {
		dependency := queue[0]
		queue = queue[1:]
		for candidate, dependencies := range g.dependencies {
			if _, seen := result[candidate]; seen || candidate == module {
				continue
			}
			if _, ok := dependencies[dependency]; ok {
				result[candidate] = struct{}{}
				queue = append(queue, candidate)
			}
		}
	}
	return result
}

// ReloadModule reloads the named module using the configured package.loaders.
//
// When invalidateDependents is false, only package.loaded[name] is replaced.
// When it is true, all modules that transitively required name are removed from
// package.loaded and reloaded on their next require.
//
// package.loaded and the require dependency graph are restored if reloading
// fails. ReloadModule cannot roll back arbitrary global variables, registry
// values, open resources, upvalues, or other side effects written by module
// code before the failure.
func (ls *LState) ReloadModule(name string, invalidateDependents bool) (err error) {
	loaded := ls.packageLoaded()
	graph := ls.G.moduleGraph
	if graph == nil {
		graph = newModuleGraph()
		ls.G.moduleGraph = graph
	}

	cacheSnapshot := snapshotLoadedModules(loaded)
	graphSnapshot := graph.clone()
	committed := false
	defer func() {
		if !committed {
			restoreLoadedModules(loaded, cacheSnapshot)
			graph.restore(graphSnapshot)
		}
	}()

	if invalidateDependents {
		for dependent := range graph.dependents(name) {
			loaded.RawSetString(dependent, LNil)
			graph.remove(dependent)
		}
	}

	if _, err = loadModule(ls, name, true); err != nil {
		return err
	}

	committed = true
	return nil
}

func loRequire(L *LState) int {
	name := L.CheckString(1)
	module, err := loadModule(L, name, false)
	if err != nil {
		L.RaiseError("%s", err.Error())
	}
	L.Push(module)
	return 1
}

func loadModule(L *LState, name string, force bool) (module LValue, err error) {
	loaded := L.packageLoaded()
	graph := L.G.moduleGraph
	if graph == nil {
		graph = newModuleGraph()
		L.G.moduleGraph = graph
	}

	graphSnapshot := graph.clone()
	committed := false
	defer func() {
		if !committed {
			graph.restore(graphSnapshot)
		}
	}()

	for i, loading := range L.loadingModules {
		if loading.name == name {
			module = loaded.RawGetString(name)
			if LVAsBool(module) && module != loading.cached {
				if len(L.loadingModules) > 0 {
					parent := L.loadingModules[len(L.loadingModules)-1].name
					if parent != name {
						graph.addDependency(parent, name)
					}
				}
				committed = true
				return module, nil
			}
			chain := make([]string, 0, len(L.loadingModules)-i+1)
			for _, loading := range L.loadingModules[i:] {
				chain = append(chain, loading.name)
			}
			chain = append(chain, name)
			return nil, fmt.Errorf("circular require detected: %s", strings.Join(chain, " -> "))
		}
	}

	if !force {
		module = loaded.RawGetString(name)
		if LVAsBool(module) {
			if len(L.loadingModules) > 0 {
				graph.addDependency(L.loadingModules[len(L.loadingModules)-1].name, name)
			}
			committed = true
			return module, nil
		}
	}

	if force {
		graph.removeOutgoing(name)
	}

	loader, err := findModuleLoader(L, name)
	if err != nil {
		return nil, err
	}

	parent := ""
	if len(L.loadingModules) > 0 {
		parent = L.loadingModules[len(L.loadingModules)-1].name
	}
	cached := loaded.RawGetString(name)
	L.loadingModules = append(L.loadingModules, moduleLoad{name: name, cached: cached})
	defer func() {
		L.loadingModules = L.loadingModules[:len(L.loadingModules)-1]
	}()

	defer func() {
		if !committed {
			loaded.RawSetString(name, cached)
		}
	}()

	L.Push(loader)
	L.Push(LString(name))
	if err = L.PCall(1, 1, nil); err != nil {
		return nil, err
	}
	result := L.Get(-1)
	L.Pop(1)

	module = loaded.RawGetString(name)
	if module == cached {
		if result != LNil {
			module = result
		} else {
			module = LTrue
		}
		loaded.RawSetString(name, module)
	}

	if parent != "" {
		graph.addDependency(parent, name)
	}
	committed = true
	return module, nil
}

func findModuleLoader(L *LState, name string) (*LFunction, error) {
	loaders, ok := L.GetField(L.Get(RegistryIndex), "_LOADERS").(*LTable)
	if !ok {
		return nil, fmt.Errorf("package.loaders must be a table")
	}

	messages := []string{}
	for i := 1; ; i++ {
		loaderValue := L.RawGetInt(loaders, i)
		if loaderValue == LNil {
			return nil, fmt.Errorf("module %s not found:\n\t%s, ", name, strings.Join(messages, "\n\t"))
		}

		L.Push(loaderValue)
		L.Push(LString(name))
		if err := L.PCall(1, 1, nil); err != nil {
			return nil, err
		}
		result := L.Get(-1)
		L.Pop(1)

		switch value := result.(type) {
		case *LFunction:
			return value, nil
		case LString:
			messages = append(messages, string(value))
		}
	}
}

func (L *LState) packageLoaded() *LTable {
	loaded, ok := L.GetField(L.Get(RegistryIndex), "_LOADED").(*LTable)
	if !ok {
		L.RaiseError("package.loaded must be a table")
	}
	return loaded
}

func snapshotLoadedModules(loaded *LTable) map[LValue]LValue {
	snapshot := make(map[LValue]LValue)
	for _, key := range loaded.keys {
		if value := loaded.RawGet(key); value != LNil {
			snapshot[key] = value
		}
	}
	return snapshot
}

func restoreLoadedModules(loaded *LTable, snapshot map[LValue]LValue) {
	for _, key := range loaded.keys {
		if _, exists := snapshot[key]; !exists {
			loaded.RawSet(key, LNil)
		}
	}
	for key, value := range snapshot {
		loaded.RawSet(key, value)
	}
}
