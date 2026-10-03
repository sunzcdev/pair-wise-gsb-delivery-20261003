<p align="center">
<img 
    src="logo.png" 
    width="240" height="78" border="0" alt="SJSON">
<br>
<a href="https://godoc.org/github.com/tidwall/sjson"><img src="https://img.shields.io/badge/api-reference-blue.svg?style=flat-square" alt="GoDoc"></a>
</p>

<p align="center">set a json value quickly</p>

SJSON is a Go package that provides a [very fast](#performance) and simple way to set a value in a json document.
For quickly retrieving json values check out [GJSON](https://github.com/tidwall/gjson).

For a command line interface check out [JJ](https://github.com/tidwall/jj).

Getting Started
===============

Installing
----------

To start using SJSON, install Go and run `go get`:

```sh
$ go get -u github.com/tidwall/sjson
```

This will retrieve the library.

Set a value
-----------
Set sets the value for the specified path. 
A path is in dot syntax, such as "name.last" or "age". 
This function expects that the json is well-formed and validated. 
Invalid json will not panic, but it may return back unexpected results.
Invalid paths may return an error.

```go
package main

import "github.com/tidwall/sjson"

const json = `{"name":{"first":"Janet","last":"Prichard"},"age":47}`

func main() {
	value, _ := sjson.Set(json, "name.last", "Anderson")
	println(value)
}
```

This will print:

```json
{"name":{"first":"Janet","last":"Anderson"},"age":47}
```

Path syntax
-----------

A path is a series of keys separated by a dot.
The dot and colon characters can be escaped with ``\``.

```json
{
  "name": {"first": "Tom", "last": "Anderson"},
  "age":37,
  "children": ["Sara","Alex","Jack"],
  "fav.movie": "Deer Hunter",
  "friends": [
	{"first": "James", "last": "Murphy"},
	{"first": "Roger", "last": "Craig"}
  ]
}
```
```
"name.last"          >> "Anderson"
"age"                >> 37
"children.1"         >> "Alex"
"friends.1.last"     >> "Craig"
```

The `-1` key can be used to append a value to an existing array:

```
"children.-1"  >> appends a new value to the end of the children array
```

Normally number keys are used to modify arrays, but it's possible to force a numeric object key by using the colon character:

```json
{
  "users":{
    "2313":{"name":"Sara"},
    "7839":{"name":"Andy"}
  }
}
```

A colon path would look like:

```
"users.:2313.name"    >> "Sara"
```

Supported types
---------------

Pretty much any type is supported:

```go
sjson.Set(`{"key":true}`, "key", nil)
sjson.Set(`{"key":true}`, "key", false)
sjson.Set(`{"key":true}`, "key", 1)
sjson.Set(`{"key":true}`, "key", 10.5)
sjson.Set(`{"key":true}`, "key", "hello")
sjson.Set(`{"key":true}`, "key", []string{"hello", "world"})
sjson.Set(`{"key":true}`, "key", map[string]interface{}{"hello":"world"})
```

When a type is not recognized, SJSON will fallback to the `encoding/json` Marshaller.


Examples
--------

Set a value from empty document:
```go
value, _ := sjson.Set("", "name", "Tom")
println(value)

// Output:
// {"name":"Tom"}
```

Set a nested value from empty document:
```go
value, _ := sjson.Set("", "name.last", "Anderson")
println(value)

// Output:
// {"name":{"last":"Anderson"}}
```

Set a new value:
```go
value, _ := sjson.Set(`{"name":{"last":"Anderson"}}`, "name.first", "Sara")
println(value)

// Output:
// {"name":{"first":"Sara","last":"Anderson"}}
```

Update an existing value:
```go
value, _ := sjson.Set(`{"name":{"last":"Anderson"}}`, "name.last", "Smith")
println(value)

// Output:
// {"name":{"last":"Smith"}}
```

Set a new array value:
```go
value, _ := sjson.Set(`{"friends":["Andy","Carol"]}`, "friends.2", "Sara")
println(value)

// Output:
// {"friends":["Andy","Carol","Sara"]
```

Append an array value by using the `-1` key in a path:
```go
value, _ := sjson.Set(`{"friends":["Andy","Carol"]}`, "friends.-1", "Sara")
println(value)

// Output:
// {"friends":["Andy","Carol","Sara"]
```

Append an array value that is past the end:
```go
value, _ := sjson.Set(`{"friends":["Andy","Carol"]}`, "friends.4", "Sara")
println(value)

// Output:
// {"friends":["Andy","Carol",null,null,"Sara"]
```

Delete a value:
```go
value, _ := sjson.Delete(`{"name":{"first":"Sara","last":"Anderson"}}`, "name.first")
println(value)

// Output:
// {"name":{"last":"Anderson"}}
```

Delete an array value:
```go
value, _ := sjson.Delete(`{"friends":["Andy","Carol"]}`, "friends.1")
println(value)

// Output:
// {"friends":["Andy"]}
```

Delete the last array value:
```go
value, _ := sjson.Delete(`{"friends":["Andy","Carol"]}`, "friends.-1")
println(value)

// Output:
// {"friends":["Andy"]}
```

Typed paths
-----------

The string based path is convenient, but sometimes the parts are already
known values and you do not want to be surprised by dot syntax, numeric
keys, or the `-1` append rule. The typed path API treats every segment
literally:

* `sjson.Key(name)` addresses an object property by its exact name. The
  name is never parsed, so `"0"`, `"a.b"`, `"#"`, and `""` all match an
  object property verbatim.
* `sjson.Index(i)` addresses array element `i`, where `i` must be
  non-negative (a negative value panics). It always means an array
  position, even `0`.
* `sjson.Append()` addresses the slot after the current last array
  element. Setting grows the array by one; deleting removes the last
  element.

Each segment kind has a matching builder method on `sjson.Path` for
chaining:

```go
path := sjson.Path{}.Key("users").Key("a.b").Index(2).Append()
value, _ := sjson.SetPaths(json, path, "Sara")
```

`SetPaths`, `SetRawPaths`, `DeletePaths`, and their `*Bytes*` and
`*Options` variants mirror the string based functions.

Creating missing parents
^^^^^^^^^^^^^^^^^^^^^^^^

When a parent container does not exist, its type is taken from the next
segment:

* a `Key` segment creates an object `{}`;
* an `Index` or `Append` segment creates an array `[]`.

```go
value, _ := sjson.SetPaths(``, sjson.Path{}.Key("a").Index(0), 1)
println(value)

// Output:
// {"a":[1]}
```

If an existing container has the wrong type for the next segment (a key
on an array, or an index/append on an object), the operation returns an
error. Descending through an existing scalar also returns an error; the
old subtree is never replaced wholesale.

Literal keys
^^^^^^^^^^^^

Names that need escaping in the string syntax are reached directly:

```go
sjson.SetPaths(json, sjson.Path{}.Key("0"), 1)     // object key "0"
sjson.SetPaths(json, sjson.Path{}.Key("a.b"), 1)   // single key "a.b"
sjson.SetPaths(json, sjson.Path{}.Key("#"), 1)     // object key "#"
sjson.SetPaths(json, sjson.Path{}.Key(""), 1)      // empty-string key
```

Array padding and deletion
^^^^^^^^^^^^^^^^^^^^^^^^^^

* Setting an `Index` past the current end pads the gap with `null`, so the
  value lands exactly at the requested position:
  `Index(4)` on `[1,2]` yields `[1,2,null,null,5]`.
* Setting an `Append` always grows the array and never replaces the last
  element.
* Deleting an in-range `Index` removes that element and closes the gap.
* Deleting a missing object key, an index at or past the array length, or
  the last element of an empty array is a no-op that leaves the document
  unchanged.

The legacy string entry points (`Set`, `SetRaw`, `Delete`, and friends)
keep their original semantics, including numeric text meaning an array
position, `-1` meaning append, and colon/backslash escaping.

## Performance

Benchmarks of SJSON alongside [encoding/json](https://golang.org/pkg/encoding/json/), 
[ffjson](https://github.com/pquerna/ffjson), 
[EasyJSON](https://github.com/mailru/easyjson),
and [Gabs](https://github.com/Jeffail/gabs)

```
Benchmark_SJSON-8                  	 3000000	       805 ns/op	    1077 B/op	       3 allocs/op
Benchmark_SJSON_ReplaceInPlace-8   	 3000000	       449 ns/op	       0 B/op	       0 allocs/op
Benchmark_JSON_Map-8               	  300000	     21236 ns/op	    6392 B/op	     150 allocs/op
Benchmark_JSON_Struct-8            	  300000	     14691 ns/op	    1789 B/op	      24 allocs/op
Benchmark_Gabs-8                   	  300000	     21311 ns/op	    6752 B/op	     150 allocs/op
Benchmark_FFJSON-8                 	  300000	     17673 ns/op	    3589 B/op	      47 allocs/op
Benchmark_EasyJSON-8               	 1500000	      3119 ns/op	    1061 B/op	      13 allocs/op
```

JSON document used:

```json
{
  "widget": {
    "debug": "on",
    "window": {
      "title": "Sample Konfabulator Widget",
      "name": "main_window",
      "width": 500,
      "height": 500
    },
    "image": { 
      "src": "Images/Sun.png",
      "hOffset": 250,
      "vOffset": 250,
      "alignment": "center"
    },
    "text": {
      "data": "Click Here",
      "size": 36,
      "style": "bold",
      "vOffset": 100,
      "alignment": "center",
      "onMouseUp": "sun1.opacity = (sun1.opacity / 100) * 90;"
    }
  }
}    
```

Each operation was rotated though one of the following search paths:

```
widget.window.name
widget.image.hOffset
widget.text.onMouseUp
```

*These benchmarks were run on a MacBook Pro 15" 2.8 GHz Intel Core i7 using Go 1.7 and can be be found [here](https://github.com/tidwall/sjson-benchmarks)*.

## Contact
Josh Baker [@tidwall](http://twitter.com/tidwall)

## License

SJSON source code is available under the MIT [License](/LICENSE).
