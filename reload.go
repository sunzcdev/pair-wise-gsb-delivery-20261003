package lua

// ReloadModule reloads the named module through the configured package loaders.
//
// When invalidateDependents is false, only package.loaded[name] is replaced.
// When it is true, modules that require name, directly or transitively, are
// removed from package.loaded after name reloads successfully; they are loaded
// again on their next require call.
//
// A failed reload restores both package.loaded and the dependency graph to
// their states before the call. ReloadModule cannot undo arbitrary globals,
// registry values, or other side effects written by module code while it runs.
func (ls *LState) ReloadModule(name string, invalidateDependents bool) (LValue, error) {
	loaded := loRegistryTable(ls, loLoadedRegistryKey, false)
	if loaded == nil {
		return nil, newApiErrorS(ApiErrorRun, "package.loaded must be a table")
	}
	graph := loRequireGraph(ls)

	loadedSnapshot := loCloneTable(loaded)
	graphSnapshot := loCloneRequireGraph(graph)

	affected := map[string]bool{name: true}
	if invalidateDependents {
		for _, dependent := range loFindDependents(graph, name) {
			affected[dependent] = true
		}
	}
	for module := range affected {
		loaded.RawSetString(module, LNil)
	}

	var result LValue
	err := ls.GPCall(func(L *LState) int {
		module, err := loadModule(L, name)
		if err != nil {
			if apiErr, ok := err.(*ApiError); ok {
				panic(apiErr)
			}
			L.RaiseError("%s", err.Error())
		}
		L.Push(module)
		return 1
	}, LNil)
	if err != nil {
		loRestoreTable(loaded, loadedSnapshot)
		loRestoreTable(graph, graphSnapshot)
		return nil, err
	}

	result = ls.Get(-1)
	ls.Pop(1)
	return result, nil
}

func loFindDependents(graph *LTable, name string) []string {
	dependents := []string{}
	visited := map[string]bool{name: true}
	queue := []string{name}

	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]

		var next []string
		graph.ForEach(func(key LValue, value LValue) {
			dependencyName, ok := key.(LString)
			if !ok || visited[string(dependencyName)] {
				return
			}
			dependencies, ok := value.(*LTable)
			if !ok || dependencies.RawGetString(module) == LNil {
				return
			}
			dependent := string(dependencyName)
			visited[dependent] = true
			next = append(next, dependent)
		})

		for _, dependent := range next {
			queue = append(queue, dependent)
			dependents = append(dependents, dependent)
		}
	}
	return dependents
}

func loCloneTable(table *LTable) *LTable {
	clone := newLTable(0, table.Len())
	table.ForEach(func(key LValue, value LValue) {
		clone.RawSet(key, value)
	})
	return clone
}

func loCloneRequireGraph(graph *LTable) *LTable {
	clone := newLTable(0, graph.Len())
	graph.ForEach(func(key LValue, value LValue) {
		if dependencies, ok := value.(*LTable); ok {
			value = loCloneTable(dependencies)
		}
		clone.RawSet(key, value)
	})
	return clone
}

func loRestoreTable(table, snapshot *LTable) {
	keys := []LValue{}
	table.ForEach(func(key LValue, value LValue) {
		keys = append(keys, key)
	})
	for _, key := range keys {
		table.RawSet(key, LNil)
	}
	snapshot.ForEach(func(key LValue, value LValue) {
		table.RawSet(key, value)
	})
}
