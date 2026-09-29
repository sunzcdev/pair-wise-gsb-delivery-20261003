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

## Streaming

In addition to searching complete Ruby objects, `JMESPath.stream` can evaluate expressions over a read-only, streaming array source. Filters and projections produce results as elements are read, so consumers can stop early without reading the entire source.

A streaming source must respond to `#each`, yielding array elements in order. If the source responds to `#close`, it is closed exactly once: when iteration completes, when the consumer stops early (e.g. via `break`, `first` or `take`), or when an error is raised while evaluating the query.

```ruby
class RowSource
  def each
    # yield rows one at a time, e.g. from a database cursor
  end

  def close
    # release the underlying resource
  end
end

result = JMESPath.stream('[?state == `active`].name', RowSource.new)
result.take(3) # reads only until 3 matches are found, then closes the source
```

Only expressions that can be evaluated incrementally stream element by element: filters, projections, flatten, non-negative indices and slices, and chains of these. Expressions that require the full input up front (e.g. `sort`, negative indices or slices) are evaluated by materializing the source into an array. Pass `materialize: false` to reject such expressions with a `JMESPath::Errors::StreamingNotSupportedError` before the source is consumed:

```ruby
JMESPath.stream('sort(@)', source, materialize: false)
#=> raises JMESPath::Errors::StreamingNotSupportedError
```

The returned `JMESPath::StreamingResult` is a lazy, forward-only `Enumerable`. Each element of the result array is yielded in order; expressions with a non-array result yield that value as a single element.

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
