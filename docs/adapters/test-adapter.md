# Test Adapter

The built-in Faraday Test adapter lets you define stubbed HTTP requests. This can
be used to mock out network services in an application's unit tests.

The easiest way to do this is to create the stubbed requests when initializing
a `Faraday::Connection`. Stubbing a request by path yields a block with a
`Faraday::Env` object. The stub block expects an Array return value with three
values: an Integer HTTP status code, a Hash of key/value headers, and a
response body.

```ruby
conn = Faraday.new do |builder|
  builder.adapter :test do |stub|
    # block returns an array with 3 items:
    # - Integer response status
    # - Hash HTTP headers
    # - String response body
    stub.get('/ebi') do |env|
      [
        200,
        { 'Content-Type': 'text/plain', },
        'shrimp'
      ]
    end

    # test exceptions too
    stub.get('/boom') do
      raise Faraday::ConnectionFailed
    end
  end
end
```

You can define the stubbed requests outside of the test adapter block:

```ruby
stubs = Faraday::Adapter::Test::Stubs.new do |stub|
  stub.get('/tamago') { |env| [200, {}, 'egg'] }
end
```

This Stubs instance can be passed to a new Connection:

```ruby
conn = Faraday.new do |builder|
  builder.adapter :test, stubs do |stub|
    stub.get('/ebi') { |env| [ 200, {}, 'shrimp' ]}
  end
end
```

It's also possible to stub additional requests after the connection has been
initialized. This is useful for testing.

```ruby
stubs.get('/uni') { |env| [ 200, {}, 'urchin' ]}
```

You can also stub the request body with a string or a proc.
It would be useful to pass a proc if it's OK only to check the parts of the request body are passed.

```ruby
stubs.post('/kohada', 'where=sea&temperature=24') { |env| [ 200, {}, 'spotted gizzard shad' ]}
stubs.post('/anago', -> (request_body) { JSON.parse(request_body).slice('name') == { 'name' => 'Wakamoto' } }) { |env| [200, {}, 'conger eel'] }
```

If you want to stub requests that exactly match a path, parameters, and headers,
`strict_mode` would be useful.

```ruby
stubs = Faraday::Adapter::Test::Stubs.new(strict_mode: true) do |stub|
  stub.get('/ikura?nori=true', 'X-Soy-Sauce' => '5ml' ) { |env| [200, {}, 'ikura gunkan maki'] }
end
```

This stub expects the connection will be called like this:

```ruby
conn.get('/ikura', { nori: 'true' }, { 'X-Soy-Sauce' => '5ml' } )
```

If there are other parameters or headers included, the Faraday Test adapter
will raise `Faraday::Test::Stubs::NotFound`. It also raises the error
if the specified parameters (`nori`) or headers (`X-Soy-Sauce`) are omitted.

You can also enable `strict_mode` after initializing the connection.
In this case, all requests, including ones that have been already stubbed,
will be handled in a strict way.

```ruby
stubs.strict_mode = true
```

## Debugging unmatched requests

When several stubs match the same HTTP method, a `Stubs::NotFound` error only
tells you that no stub matched, not whether the request was rejected because of
its query params, headers or body. Pass `debug: true` when creating the `Stubs`
to append a diagnostics report to that error. The report lists every candidate
stub, separates the ones that have already been consumed from the ones still
pending, and shows the matching step at which each candidate was eliminated
(`host`, `path`, `params`, `body` or `headers`), including the expected and
actual values.

```ruby
stubs = Faraday::Adapter::Test::Stubs.new(debug: true) do |stub|
  stub.get('/ikura?nori=true', 'X-Soy-Sauce' => '5ml') { |env| [200, {}, 'ikura'] }
end
```

A failing request now raises with a message such as:

```text
no stubbed request for get http:/ikura?nori=false
Faraday::Adapter::Test diagnostics for GET http:/ikura?nori=false:
Pending stubs (not consumed yet) (1):
  - /ikura ?nori=true headers={"X-Soy-Sauce"=>"5ml"}
      host: matched
      path: matched
      params: expected query params {"nori"=>"true"}, got {"nori"=>"false"} (strict: false)
Already consumed stubs (0):
  (none)
```

Matching steps short-circuit on the first failure, so a step that was never
reached (for example `headers` above) is not shown, and a proc body matcher is
evaluated at most once per candidate — it is never run again just to build the
report. The same report is also available programmatically through
`stubs.last_diagnostics` while handling the error. Strict mode (`strict_mode`)
judgements are unchanged; diagnostics only explain them.

Instead of `true`, you can pass any object that responds to `#puts` (for
example `$stdout` or a `StringIO`). The report is then written to that object,
and the original one-line `Stubs::NotFound` message is preserved.

```ruby
stubs = Faraday::Adapter::Test::Stubs.new(debug: $stdout)
```

Finally, you can treat your stubs as mocks by verifying that all of the stubbed
calls were made. NOTE: this feature is still fairly experimental. It will not
verify the order or count of any stub.

```ruby
stubs.verify_stubbed_calls
```

After the test case is completed (possibly in an `after` hook), you should reset
the stubs so each test starts with its own set. When you hold a reference to the
`Stubs` instance, call `clear` to remove every stub, including consumed ones.

```ruby
stubs.clear
```

Alternatively, if you rely on the default connection, you can clear it to prevent
it from being cached between different tests.

```ruby
Faraday.default_connection = nil
```

## Examples

Working [RSpec] and [test/unit] examples for a fictional JSON API client are
available.

[RSpec]: https://github.com/lostisland/faraday/blob/main/examples/client_spec.rb
[test/unit]: https://github.com/lostisland/faraday/blob/main/examples/client_test.rb
