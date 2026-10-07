# XPath

[![GoDoc](https://godoc.org/github.com/antchfx/xpath?status.svg)](https://godoc.org/github.com/antchfx/xpath)
[![Coverage Status](https://coveralls.io/repos/github/antchfx/xpath/badge.svg?branch=master)](https://coveralls.io/github/antchfx/xpath?branch=master)
[![Build Status](https://github.com/antchfx/xpath/actions/workflows/testing.yml/badge.svg)](https://github.com/antchfx/xpath/actions/workflows/testing.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/antchfx/xpath)](https://goreportcard.com/report/github.com/antchfx/xpath)

XPath is Go package provides selecting nodes from XML, HTML or other documents using XPath expression.

# Implementation

- [htmlquery](https://github.com/antchfx/htmlquery) - an XPath query package for HTML document

- [xmlquery](https://github.com/antchfx/xmlquery) - an XPath query package for XML document.

- [jsonquery](https://github.com/antchfx/jsonquery) - an XPath query package for JSON document

# Supported Features

#### The basic XPath patterns.

> The basic XPath patterns cover 90% of the cases that most stylesheets will need.

- `node` : Selects all child elements with nodeName of node.

- `*` : Selects all child elements.

- `@attr` : Selects the attribute attr.

- `@*` : Selects all attributes.

- `node()` : Matches an org.w3c.dom.Node.

- `text()` : Matches a org.w3c.dom.Text node.

- `comment()` : Matches a comment.

- `.` : Selects the current node.

- `..` : Selects the parent of current node.

- `/` : Selects the document node.

- `a[expr]` : Select only those nodes matching a which also satisfy the expression expr.

- `a[n]` : Selects the nth matching node matching a When a filter's expression is a number, XPath selects based on position.

- `a/b` : For each node matching a, add the nodes matching b to the result.

- `a//b` : For each node matching a, add the descendant nodes matching b to the result.

- `//b` : Returns elements in the entire document matching b.

- `a|b` : All nodes matching a or b, union operation(not boolean or).

- `(a, b, c)` : Evaluates each of its operands and concatenates the resulting sequences, in order, into a single result sequence

- `(a/b)` : Selects all matches nodes as grouping set.

#### Node Axes

- `child::*` : The child axis selects children of the current node.

  - `child::node()`: Selects all the children of the context node.
  - `child::text()`: Selects all text node children of the context node.

- `descendant::*` : The descendant axis selects descendants of the current node. It is equivalent to '//'.

- `descendant-or-self::*` : Selects descendants including the current node.

- `attribute::*` : Selects attributes of the current element. It is equivalent to @\*

- `following-sibling::*` : Selects nodes after the current node.

- `preceding-sibling::*` : Selects nodes before the current node.

- `following::*` : Selects the first matching node following in document order, excluding descendants.

- `preceding::*` : Selects the first matching node preceding in document order, excluding ancestors.

- `parent::*` : Selects the parent if it matches. The '..' pattern from the core is equivalent to 'parent::node()'.

- `ancestor::*` : Selects matching ancestors.

- `ancestor-or-self::*` : Selects ancestors including the current node.

- `self::*` : Selects the current node. '.' is equivalent to 'self::node()'.

#### Expressions

The gxpath supported three types: number, boolean, string.

- `path` : Selects nodes based on the path.

- `a = b` : Standard comparisons.

  - `a = b` : True if a equals b.
  - `a != b` : True if a is not equal to b.
  - `a < b` : True if a is less than b.
  - `a <= b` : True if a is less than or equal to b.
  - `a > b` : True if a is greater than b.
  - `a >= b` : True if a is greater than or equal to b.

- `a + b` : Arithmetic expressions.

  - `- a` Unary minus
  - `a + b` : Addition
  - `a - b` : Subtraction
  - `a * b` : Multiplication
  - `a div b` : Division
  - `a mod b` : Modulus (division remainder)

- `a or b` : Boolean `or` operation.

- `a and b` : Boolean `and` operation.

- `(expr)` : Parenthesized expressions.

- `fun(arg1, ..., argn)` : Function calls:

| Function                | Supported |
| ----------------------- | --------- |
| `boolean()`             | ✓         |
| `ceiling()`             | ✓         |
| `choose()`              | ✗         |
| `concat()`              | ✓         |
| `contains()`            | ✓         |
| `count()`               | ✓         |
| `current()`             | ✗         |
| `document()`            | ✗         |
| `element-available()`   | ✗         |
| `ends-with()`           | ✓         |
| `false()`               | ✓         |
| `floor()`               | ✓         |
| `format-number()`       | ✗         |
| `function-available()`  | ✗         |
| `generate-id()`         | ✗         |
| `id()`                  | ✗         |
| `key()`                 | ✗         |
| `lang()`                | ✗         |
| `last()`                | ✓         |
| `local-name()`          | ✓         |
| `lower-case()`[^1]      | ✓         |
| `matches()`             | ✓         |
| `name()`                | ✓         |
| `namespace-uri()`       | ✓         |
| `normalize-space()`     | ✓         |
| `not()`                 | ✓         |
| `number()`              | ✓         |
| `position()`            | ✓         |
| `replace()`             | ✓         |
| `reverse()`             | ✓         |
| `round()`               | ✓         |
| `starts-with()`         | ✓         |
| `string()`              | ✓         |
| `string-join()`[^1]     | ✓         |
| `string-length()`       | ✓         |
| `substring()`           | ✓         |
| `substring-after()`     | ✓         |
| `substring-before()`    | ✓         |
| `sum()`                 | ✓         |
| `system-property()`     | ✗         |
| `translate()`           | ✓         |
| `true()`                | ✓         |
| `unparsed-entity-url()` | ✗         |

[^1]: XPath-2.0 expression

# Extension Functions

Business logic can be exposed to XPath rules as **expression-scoped
extension functions**. A function is identified by a namespace URI and a
local name, and invoked with a QName (`prefix:local-name()`).

Functions are registered on a `FunctionRegistry` passed to
`CompileWithExtensions`. The registry is per compiled expression — there is
no package-global function table — so two registries can register the same
QName with different implementations without interfering with each other.

```go
registry := xpath.NewFunctionRegistry()
err := registry.Register(&xpath.FunctionSpec{
    NamespaceURI: "http://example.com/business",
    LocalName:    "classify",
    // Fixed arity: one string argument, string result.
    Params: []xpath.ExtensionType{xpath.ExtensionString},
    Return: xpath.ExtensionString,
    Func: func(ctx xpath.ExtensionContext, args []interface{}) (interface{}, error) {
        code := args[0].(string)          // XPath string() conversion already applied
        _ = ctx.ContextNode               // copy of the current node
        _ = ctx.Position                  // 1-based position in the context node-set
        _ = ctx.Last                      // size of the context node-set
        caller, _ := ctx.CallerContext.(*Request) // value passed to Eval
        return lookupClassification(caller, code)
    },
})

// Bind the "biz" prefix to the function namespace and compile.
ns := map[string]string{"biz": "http://example.com/business"}
expr, err := xpath.CompileWithExtensions(`//item[biz:classify(@code) = 'premium']`, ns, registry)

// Evaluate with an optional, opaque caller context (nil is allowed).
result, err := expr.Eval(navigator, request)
```

Types use the XPath 1.0 conversion rules at runtime:

- `ExtensionBoolean` (`bool`), `ExtensionNumber` (`float64`),
  `ExtensionString` (`string`), `ExtensionNodeSet` (`[]NodeNavigator`), and
  `ExtensionAny` (the dynamic value's Go form).
- Scalar arguments are converted with `boolean()`, `number()`, or
  `string()`; a node-set argument receives the materialized
  `[]NodeNavigator`. Passing a statically scalar expression where a node-set
  is declared is a compile-time error, as is an arity mismatch or an
  unbound prefix / unregistered function.
- Scalar and node-set results are both supported. A node-set-returning
  function may itself be used as a path step
  (`biz:related(.)/price`); a scalar function may not.

### Errors

An error returned by a function is never folded into `false` or an empty
result. It aborts evaluation as `*xpath.ExtensionError` (use `errors.As` /
`errors.Is` to reach the underlying error). For scalar expressions
`Eval` returns the error; for node-set iteration use
`SelectWithContext` and check `NodeIterator.Err()` after `MoveNext()`
returns false.

Because every `Eval`/`SelectWithContext` call clones the internal query
tree, a failed evaluation does not poison the compiled expression: the same
`*Expr` can be reused with different data or caller contexts afterwards.

### Node ownership

The `ExtensionContext.ContextNode` and every navigator in a node-set
argument are copies owned by the engine; user code may navigate them freely.
Navigators returned in a node-set result must point at nodes in the source
document (the caller owns the document and must keep it alive for the
evaluation). The engine copies each returned navigator while streaming
results, so a function may return freshly positioned navigators whose
underlying nodes remain owned by the document.
