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

**[See the JMESPath specification for a full list of supported search expressions.](http://jmespath.org/specification.html)**

## Searching With Sources

When you need to know where every returned value came from (for example when
debugging a configuration document), call `JMESPath.search_with_sources`. It
returns a `JMESPath::SearchResult` with the same `value` that `search` would
return plus a `sources` tree that mirrors the result shape.

Each leaf is either:

* `JMESPath::Sources::Origin` - the value was copied from the original
  document; its `pointer` is an RFC 6901 JSON Pointer into the input.
* `JMESPath::Sources::Derived` - the value was computed by the expression;
  `inputs` lists the JSON Pointers of the direct inputs.

Positions are propagated structurally, so filtering, slices, `[]` flatten and
`sort_by` reordering keep pointing at the original location of each value;
result array indices never replace source array indices. Equal values that
appear more than once are tracked independently rather than located by value.

```ruby
result = JMESPath.search_with_sources(
  'people[?age > `20`] | sort_by(@, &age) | [*].name',
  { "people" => [
    { "name" => "Zoe",  "age" => 40 },
    { "name" => "Amy",  "age" => 30 }
  ]}
)
result.value
#=> ["Amy", "Zoe"]
result.sources['sources']
#=> [#<JMESPath::Sources::Origin pointer="/people/1/name">,
#    #<JMESPath::Sources::Origin pointer="/people/0/name">]

JMESPath.search_with_sources('length(people)', { "people" => [] }).sources
#=> #<JMESPath::Sources::Derived inputs=["/people"]>
```

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
