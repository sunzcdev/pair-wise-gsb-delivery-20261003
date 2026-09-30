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

## Compile-Time Shape Checking

When the rough shape of the input document is known ahead of time, you can
catch obvious function type errors *before* a query ever runs, and infer the
shape of the result. This uses a lightweight shape description (optional
fields, array element types, `null` and union types) rather than a full JSON
Schema engine.

```ruby
shape = JMESPath::Shape.object(
  people: JMESPath::Shape.array(
    JMESPath::Shape.object(
      id:   JMESPath::Shape.number,
      name: JMESPath::Shape.string,
      age:  JMESPath::Shape.optional(JMESPath::Shape.number)  # may be missing
    )
  )
)

result = JMESPath.check('people[*].age', shape)
result.ok?            #=> true
result.output_shape   #=> #<JMESPath::Shape array<number>>

JMESPath.check('abs(people[0].name)', shape).ok?  #=> false (definite type error)
```

Only *definite* failures are reported as errors. Anything that could still
succeed is accepted and listed as a deferred runtime check, so unknown data
is never treated as an error:

```ruby
result = JMESPath.check('abs(people[0].age)', shape)
result.ok?                       #=> true (age is number | null)
result.runtime_checks.size       #=> 1 (kept for runtime)

# Filter predicates narrow the surviving branch:
JMESPath.check('map(&abs, people[?age != `null`].age)', shape).ok?  #=> true
```

Shapes can also be inferred from example values with
`JMESPath::Shape.from_value` / `JMESPath::Shape.from_values`. Use
`JMESPath.check!` to raise `JMESPath::Errors::StaticTypeError` instead of
returning a result. See `examples/shape_check_demo.rb` for a runnable
walkthrough contrasting compile-time errors with runtime checks.

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
