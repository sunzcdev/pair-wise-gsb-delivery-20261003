go-immutable-radix [![Run CI Tests](https://github.com/hashicorp/go-immutable-radix/actions/workflows/ci.yaml/badge.svg)](https://github.com/hashicorp/go-immutable-radix/actions/workflows/ci.yaml)
=========

Provides the `iradix` package that implements an immutable [radix tree](http://en.wikipedia.org/wiki/Radix_tree).
The package only provides a single `Tree` implementation, optimized for sparse nodes.

As a radix tree, it provides the following:
 * O(k) operations. In many cases, this can be faster than a hash table since
   the hash function is an O(k) operation, and hash tables have very poor cache locality.
 * Minimum / Maximum value lookups
 * Ordered iteration

A tree supports using a transaction to batch multiple updates (insert, delete)
in a more efficient manner than performing each operation one at a time.

For a mutable variant, see [go-radix](https://github.com/armon/go-radix).

V2
==

The v2 of go-immutable-radix introduces generics to improve compile-time type
safety for users of the package. The module name for v2 is
`github.com/hashicorp/go-immutable-radix/v2`.

Documentation
=============

The full documentation is available on [Godoc](http://godoc.org/github.com/hashicorp/go-immutable-radix).

Example
=======

Below is a simple example of usage

```go
// Create a tree
r := iradix.New[int]()
r, _, _ = r.Insert([]byte("foo"), 1)
r, _, _ = r.Insert([]byte("bar"), 2)
r, _, _ = r.Insert([]byte("foobar"), 2)

// Find the longest prefix match
m, _, _ := r.Root().LongestPrefix([]byte("foozip"))
if string(m) != "foo" {
    panic("should be foo")
}
```

Here is an example of performing a range scan of the keys.

```go
// Create a tree
r := iradix.New[int]()
r, _, _ = r.Insert([]byte("001"), 1)
r, _, _ = r.Insert([]byte("002"), 2)
r, _, _ = r.Insert([]byte("005"), 5)
r, _, _ = r.Insert([]byte("010"), 10)
r, _, _ = r.Insert([]byte("100"), 10)

// Range scan over the keys that sort lexicographically between [003, 050)
it := r.Root().Iterator()
it.SeekLowerBound([]byte("003"))
for key, _, ok := it.Next(); ok; key, _, ok = it.Next() {
  if string(key) >= "050" {
      break
  }
  fmt.Println(string(key))
}
// Output:
//  005
//  010
```

Prefix rule view (`rules` subpackage)
=====================================

The `rules` subpackage layers an "effective rule" view over the tree when a
prefix may carry several rules and entries may stop inheritance from their
parents. It is not a plain longest-prefix lookup: for a queried key it walks
exactly the entries on the key's path (siblings never participate), applies
barriers that clear inherited rules, and returns the surviving rule plus an
explanation of every rule shadowed along the way. Higher priority wins; ties
are broken by the smaller stable rule ID.

Updates use one immutable snapshot and a transactional batch:

```go
snap := rules.New().Batch().
    SetRules(nil, []rules.Rule{{ID: "default", Priority: 100}}).
    Replace([]byte("api/"), []rules.Rule{{ID: "api", Priority: 5}}, true). // barrier
    Commit()

d := snap.Decide([]byte("api/v2/users"))
// d.Hit, d.Effective, d.MatchPrefix
// d.Shadows  - why each ancestor rule lost (barrier or outranked)
// d.Barriers - barriers crossed and how many rules each cleared

next := snap.Batch().SetRules([]byte("api/"), nil).Commit()
// snap keeps its previous decision; next is a new immutable snapshot.
```
