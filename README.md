# jmespath.rb

[![Gitter chat](https://badges.gitter.im/jmespath/jmespath.rb.png)](https://gitter.im/jmespath/jmespath.rb) [![Build Status](https://travis-ci.org/jmespath/jmespath.rb.png?branch=master)](https://travis-ci.org/jmespath/jmespath.rb)

An implementation of [JMESPath](https://github.com/boto/jmespath) for Ruby. This implementation supports searching JSON documents as well as native Ruby data structures.

## Installation

```
$ gem install jmespath
```

## Basic Usage

Call `JMESPath.search` with a valid JMESPath search expression and data to search. It will return the extracted values.

```ruby
require 'jmespath'

JMESPath.search('foo.bar', { foo: { bar: { baz: "value" }}})
#=> {baz: "value"}
```

In addition to accessing nested values, you can exact values from arrays.

```ruby
JMESPath.search('foo.bar[0]', { foo: { bar: ["one", "two"] }})
#=> "one"

JMESPath.search('foo.bar[-1]', { foo: { bar: ["one", "two"] }})
#=> "two"

JMESPath.search('foo[*].name', {foo: [{name: "one"}, {name: "two"}]})
#=> ["one", "two"]
```

If you search for keys no present in the data, then `nil` is returned.

```ruby
JMESPath.search('foo.bar', { abc: "mno" })
#=> nil
```

## Named Query Sets (fused multi-query extraction)

When one response has to populate a dozen fields, it is common for many of
those expressions to re-traverse the same large array (for example several
`people[?...].field` filters with different predicates). Searching them one by
one walks that array once per expression.

A **named query set** compiles a collection of named expressions together. At
compile time it identifies the AST prefixes the expressions share and builds a
single fused plan that walks shared arrays once, branching out to the
different predicates and continuation fields while it does so.

```ruby
qs = JMESPath.query_set(
  adult_names:  "people[?age > `18`].name",
  adult_emails: "people[?age > `18`].email",
  seniors:      "people[?age > `65`].name",
  ny_residents: "people[?city == 'NY'].name",
  headcount:    "length(people)"
)

result = qs.search(data)
result[:adult_names]   #=> [...]
result[:seniors]       #=> [...]
```

Sharing is decided from the **parsed AST**, never by slicing expression
strings, so semantically different prefixes are never conflated:

* a pipe boundary (`a | b`) is kept distinct from a dotted boundary (`a.b`);
* an expression reference (`&age` in `map(&age, people)`) is an opaque value and
  is only shared with an identical reference, never fused into a plain prefix;
* different filter predicates (`[?age > `18`]` vs `[?age > `65`]`) do not share
  the filter step, though they still share every earlier prefix and the source
  array is traversed only once.

### Error policy

When one of the queries raises at search time you can either fail the whole
group or keep the results of the others:

```ruby
result = qs.search(data, on_error: :collect) # default is :raise
result.successful?(:adult_names) #=> true
result.errors[:bad_query]       #=> #<JMESPath::Errors::InvalidTypeError ...>
result.keys                      #=> successful names only

# :nil keeps the failed name with a nil value instead of omitting it
qs.search(data, on_error: :nil)

# an optional block receives every failure: |name, error|
qs.search(data, on_error: :collect) { |name, error| warn "#{name}: #{error}" }
```

### Access counts (proof of the single traversal)

`result.stats` compares the fused plan against running the same expressions
one by one, using the same counters for both:

```ruby
result.stats.fused.array_iterations     #=> 4 (one walk shared by 4 queries)
result.stats.separate.array_iterations  #=> 16 (four independent walks)
result.stats.iterations_saved           #=> 12
result.stats.visits_saved               #=> fewer AST node visits too
```

A runnable demonstration over a large array is provided at
`benchmark/query_set.rb` (`ruby -Ilib benchmark/query_set.rb`).

**[See the JMESPath specification for a full list of supported search expressions.](http://jmespath.org/specification.html)**

## Indifferent Access

The examples above show JMESPath expressions used to search over hashes with symbolized keys. You can use search also for hashes with string keys or Struct objects.

```ruby
JMESPath.search('foo.bar', { "foo" => { "bar" => "value" }})
#=> "value"

data = Struct.new(:foo).new(
  Struct.new(:bar).new("value")
)
JMESPath.search('foo.bar', data)
#=> "value"
```

## JSON Documents

If you have JSON documents on disk, or IO objects that contain JSON documents, you can pass them as the data argument.

```ruby
JMESPath.search(expression, Pathname.new('/path/to/data.json'))

File.open('/path/to/data.json', 'r', encoding:'UTF-8') do |file|
  JMESPath.search(expression, file)
end
```

## Links of Interest

* [Release Notes](https://github.com/trevorrowe/jmespath.rb/releases)
* [License](http://www.apache.org/licenses/LICENSE-2.0)
* [JMESPath Tutorial](http://jmespath.org/tutorial.html)
* [JMESPath Specification](http://jmespath.org/specification.html)

## License

This library is distributed under the apache license, version 2.0

> Copyright 2014 Trevor Rowe; All rights reserved.
>
> Licensed under the apache license, version 2.0 (the "license");
> You may not use this library except in compliance with the license.
> You may obtain a copy of the license at:
>
> http://www.apache.org/licenses/license-2.0
>
> Unless required by applicable law or agreed to in writing, software
> distributed under the license is distributed on an "as is" basis,
> without warranties or conditions of any kind, either express or
> implied.
>
> See the license for the specific language governing permissions and
> limitations under the license.
