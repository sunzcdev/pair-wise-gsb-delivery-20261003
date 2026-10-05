package lua

import "testing"

func runStableSortScript(t *testing.T, script string) {
	L := NewState()
	defer L.Close()
	if err := L.DoString(script); err != nil {
		t.Fatal(err)
	}
}

func TestTableStableSortErrorRollback(t *testing.T) {
	// An error raised by the comparator must not reorder or truncate the table.
	runStableSortScript(t, `
		local t = {3, 9, 1, 7, 4}
		local ok, err = pcall(table.stable_sort, t, function(a, b)
			if a == 9 or b == 9 then
				error("boom")
			end
			return a < b
		end)
		assert(not ok)
		assert(t[1] == 3 and t[2] == 9 and t[3] == 1 and t[4] == 7 and t[5] == 4)
		assert(#t == 5)
	`)

	// A failing default comparison (incomparable types) must also roll back.
	runStableSortScript(t, `
		local t = {3, "x", 1}
		local ok, err = pcall(table.stable_sort, t)
		assert(not ok)
		assert(t[1] == 3 and t[2] == "x" and t[3] == 1)
		assert(#t == 3)
	`)

	// A faulty __lt metamethod is another comparator failure path.
	runStableSortScript(t, `
		local mt = {__lt = function(a, b) error("meta boom") end}
		local a, b = setmetatable({v = 1}, mt), setmetatable({v = 2}, mt)
		local t = {a, b}
		local ok, err = pcall(table.stable_sort, t)
		assert(not ok)
		assert(t[1] == a and t[2] == b)
	`)
}

func TestTableStableSortStability(t *testing.T) {
	// Equal keys keep their original relative order.
	runStableSortScript(t, `
		local function entry(k, id) return {k = k, id = id} end
		local t = {
			entry(2, 1), entry(1, 2), entry(2, 3),
			entry(1, 4), entry(2, 5), entry(3, 6),
		}
		table.stable_sort(t, function(a, b) return a.k < b.k end)
		assert(t[1].id == 2)
		assert(t[2].id == 4)
		assert(t[3].id == 1)
		assert(t[4].id == 3)
		assert(t[5].id == 5)
		assert(t[6].id == 6)
	`)

	// Stability with the default comparator and an __lt metamethod.
	runStableSortScript(t, `
		local mt = {__lt = function(a, b) return a.rank < b.rank end}
		local function v(rank, id) return setmetatable({rank = rank, id = id}, mt) end
		local t = {v(1, 1), v(0, 2), v(1, 3), v(0, 4), v(1, 5)}
		table.stable_sort(t)
		assert(t[1].id == 2)
		assert(t[2].id == 4)
		assert(t[3].id == 1)
		assert(t[4].id == 3)
		assert(t[5].id == 5)
	`)
}

func TestTableStableSortComparatorMutation(t *testing.T) {
	// The comparator replaces an entry while sorting: the result must be
	// rejected and the comparator's write preserved, not overwritten.
	runStableSortScript(t, `
		local t = {3, 1, 2}
		local calls = 0
		local ok, err = pcall(table.stable_sort, t, function(a, b)
			calls = calls + 1
			if calls == 2 then
				t[1] = 99
			end
			return a < b
		end)
		assert(not ok)
		assert(t[1] == 99)
		assert(t[2] == 1 and t[3] == 2)
		assert(#t == 3)
	`)

	// Appending during sorting changes the array length and is rejected.
	runStableSortScript(t, `
		local t = {3, 1, 2}
		local calls = 0
		local ok, err = pcall(table.stable_sort, t, function(a, b)
			calls = calls + 1
			if calls == 2 then
				t[4] = 8
			end
			return a < b
		end)
		assert(not ok)
		assert(t[1] == 3 and t[2] == 1 and t[3] == 2 and t[4] == 8)
		assert(#t == 4)
	`)

	// Removing during sorting shrinks the length and is rejected.
	runStableSortScript(t, `
		local t = {3, 1, 2}
		local calls = 0
		local ok, err = pcall(table.stable_sort, t, function(a, b)
			calls = calls + 1
			if calls == 2 then
				table.remove(t)
			end
			return a < b
		end)
		assert(not ok)
		assert(t[1] == 3 and t[2] == 1)
		assert(#t == 2)
	`)
}

func TestTableStableSortHashUntouched(t *testing.T) {
	// Hash keys are never reordered and hash writes survive a successful sort.
	runStableSortScript(t, `
		local t = {5, 2, 4, key = "v", [-7] = "h"}
		table.stable_sort(t, function(a, b) return a < b end)
		assert(t[1] == 2 and t[2] == 4 and t[3] == 5)
		assert(t.key == "v")
		assert(t[-7] == "h")

		local u = {3, 1, mark = "x"}
		table.stable_sort(u, function(a, b)
			u.tag = "y"
			return a < b
		end)
		assert(u[1] == 1 and u[2] == 3)
		assert(u.mark == "x" and u.tag == "y")
	`)
}

func TestTableStableSortSparse(t *testing.T) {
	// The range follows the Lua length rule: trailing nil entries are
	// outside the range and beyond-length array keys are never touched.
	runStableSortScript(t, `
		local t = {3, 1, 2, nil}
		assert(#t == 3)
		table.stable_sort(t)
		assert(t[1] == 1 and t[2] == 2 and t[3] == 3)

		local v = {3, 1, 2, side = 99}
		table.stable_sort(v)
		assert(v[1] == 1 and v[2] == 2 and v[3] == 3)
		assert(v.side == 99)

		-- Interior holes are inside the length range, so comparing the nil
		-- entry (just like the existing table.sort) raises and rolls back.
		local u = {3, 1, nil, 2}
		assert(#u == 4)
		local ok, err = pcall(table.stable_sort, u)
		assert(not ok)
		assert(u[1] == 3 and u[2] == 1 and u[3] == nil and u[4] == 2)
	`)
}
