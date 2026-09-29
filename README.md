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

## Diagnostic Mode

When reviewing configuration files, a key that is absent often means
something different than a key explicitly set to `null`. The default
`JMESPath.search` maps both to `nil`. `JMESPath.diagnose` keeps them
apart: it evaluates the expression with an internal `missing` sentinel
and returns a JSON serializable report instead of a plain value.

```ruby
JMESPath.diagnose(expression, data)
#=> {"value" => ..., "diagnostics" => [{"path" => "...", "reason" => "..."}]}
```

The `"value"` follows the default semantics (missing becomes `nil`), and
`"diagnostics"` lists where the expression relied on something absent.
Reasons are `"missing_key"`, `"index_out_of_bounds"`, `"not_traversable"`
and `"function_argument"`. The internal sentinel never leaks into the
report.

How `missing` behaves inside an expression, compared to `null`:

| Context | `null` | `missing` (diagnostic mode) |
| --- | --- | --- |
| `a == \`null\`` | `true` | `false` |
| `a != \`null\`` | `false` | `true` |
| `a > \`1\`` (ordering) | `null` | `null` |
| `!a`, `a && b`, `a \|\| b` | falsey | falsey |
| `[*].a` projection | element dropped | element dropped + recorded |
| `f(a)` function argument | passed as `null` | passed as `null` + recorded |

Running both modes over the same input, including a path that goes
missing in the middle (see `examples/diagnose.rb`):

```ruby
data = {
  'service' => {
    'endpoint' => 'https://example.com',
    'retries'  => nil,                 # explicitly null
    # 'timeout' is absent
    'tags' => [{ 'name' => 'a' }, { 'name' => 'b', 'weight' => 2 }]
  }
}

JMESPath.search('service.timeout.seconds', data)
#=> nil
JMESPath.diagnose('service.timeout.seconds', data)
#=> {"value"=>nil,
#    "diagnostics"=>[{"path"=>"service.timeout", "reason"=>"missing_key"}]}

JMESPath.search('service.timeout == `null`', data)
#=> true
JMESPath.diagnose('service.timeout == `null`', data)
#=> {"value"=>false,
#    "diagnostics"=>[{"path"=>"service.timeout", "reason"=>"missing_key"}]}

JMESPath.search('service.tags[*].weight', data)
#=> [2]
JMESPath.diagnose('service.tags[*].weight', data)
#=> {"value"=>[2],
#    "diagnostics"=>[{"path"=>"weight", "reason"=>"missing_key"}]}
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
