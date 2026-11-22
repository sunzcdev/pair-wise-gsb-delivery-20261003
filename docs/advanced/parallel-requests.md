# Parallel Requests

Some adapters support running requests in parallel.
This can be achieved using the `#in_parallel` method on the connection object.

```ruby
# Install the Typhoeus adapter with `gem install faraday-typhoeus` first.
require 'faraday/typhoeus'

conn = Faraday.new('http://httpbingo.org') do |faraday|
  faraday.adapter :typhoeus
end

now = Time.now

conn.in_parallel do
  conn.get('/delay/3')
  conn.get('/delay/3')
end

# This should take about 3 seconds, not 6.
puts "Time taken: #{Time.now - now}"
```

## Nested parallel blocks

Helper methods called from inside an `in_parallel` block sometimes open a
parallel block of their own. Such nested calls must say which group their
requests belong to:

* with no arguments (or `scope: :inherit`), the nested block joins the
  enclosing group. Its requests stay deferred, and the group is submitted once,
  when the outermost block that opened it ends.
* passing a `manager`, or `scope: :new`, opens an independent group. That group
  is submitted when the nested block ends, while the enclosing group keeps
  collecting its own requests.

```ruby
conn.in_parallel do
  conn.get('/one')                        # joins the outer group

  conn.in_parallel do                     # joins the outer group as well
    conn.get('/two')
  end

  conn.in_parallel(scope: :new) do        # submitted on its own right here
    conn.get('/three')
  end

  conn.get('/four')                       # still part of the outer group
end
# Submitted: ['/three'], then ['/one', '/two', '/four'].
```

Requests are only submitted at the boundary that owns their group, so an
inherited nested block never flushes the surrounding group early.

If a nested block raises, the group it opened is aborted and not submitted, and
the connection state of the enclosing block is restored. Once the error is
handled, the connection is no longer in parallel mode, so a later plain request
is sent on its own instead of being attached to the aborted group. The next
`in_parallel` block also starts with a fresh manager, so requests collected by
the aborted group can never be submitted later.

## A note on Async

You might have heard about [Async] and its native integration with Ruby 3.0.
The good news is that you can already use Async with Faraday (thanks to the [async-http-faraday] gem)
and this does not require the use of `#in_parallel` to run parallel requests.
Instead, you only need to wrap your Faraday code into an Async block:

```ruby
# Install the Async adapter with `gem install async-http-faraday` first.
require 'async/http/faraday'

conn = Faraday.new('http://httpbingo.org') do |faraday|
  faraday.adapter :async_http
end

now = Time.now

# NOTE: This is not limited to a single connection anymore!
# You can run parallel requests spanning multiple connections.
Async do
  Async { conn.get('/delay/3') }
  Async { conn.get('/delay/3') }
end

# This should take about 3 seconds, not 6.
puts "Time taken: #{Time.now - now}"

```

The big advantage of using Async is that you can now run parallel requests *spanning multiple connections*,
whereas the `#in_parallel` method only works for requests that are made through the same connection.

[Async]: https://github.com/socketry/async
[async-http-faraday]: https://github.com/socketry/async-http-faraday
