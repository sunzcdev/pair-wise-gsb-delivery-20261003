golang-lru
==========

This provides the `lru` package which implements a fixed-size
thread safe LRU cache. It is based on the cache in Groupcache.

Documentation
=============

Full docs are available on [Go Packages](https://pkg.go.dev/github.com/hashicorp/golang-lru/v2)

LRU cache example
=================

```go
package main

import (
	"fmt"
	"github.com/hashicorp/golang-lru/v2"
)

func main() {
	l, _ := lru.New[int, any](128)
	for i := 0; i < 256; i++ {
		l.Add(i, nil)
	}
	if l.Len() != 128 {
		panic(fmt.Sprintf("bad len: %v", l.Len()))
	}
}
```

Weighted LRU cache example
==========================

```go
package main

import (
	"fmt"

	"github.com/hashicorp/golang-lru/v2"
)

func main() {
	// bound the cache by the total weight of its entries, not their count
	l, _ := lru.NewWeighted[int, int](128)

	// Add(key, value, weight). Returns whether the entry was stored and
	// how many entries were evicted (oldest first) to fit the budget.
	added, evicted := l.Add(1, 1, 100)
	fmt.Println(added, evicted) // true 0

	// a single value larger than the budget is rejected and nothing moves
	added, _ = l.Add(2, 2, 200)
	fmt.Println(added, l.Len(), l.Weight()) // false 1 100
}
```

Expirable LRU cache example
===========================

```go
package main

import (
	"fmt"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

func main() {
	// make cache with 10ms TTL and 5 max keys
	cache := expirable.NewLRU[string, string](5, nil, time.Millisecond*10)


	// set value under key1.
	cache.Add("key1", "val1")

	// get value under key1
	r, ok := cache.Get("key1")

	// check for OK value
	if ok {
		fmt.Printf("value before expiration is found: %v, value: %q\n", ok, r)
	}

	// wait for cache to expire
	time.Sleep(time.Millisecond * 12)

	// get value under key1 after key expiration
	r, ok = cache.Get("key1")
	fmt.Printf("value after expiration is found: %v, value: %q\n", ok, r)

	// set value under key2, would evict old entry because it is already expired.
	cache.Add("key2", "val2")

	fmt.Printf("Cache len: %d\n", cache.Len())
	// Output:
	// value before expiration is found: true, value: "val1"
	// value after expiration is found: false, value: ""
	// Cache len: 1
}
```
