# Configuration

Faraday is highly configurable and allows you to customize the way requests are made.
This applies to both the connection and the request, but can also cover things like SSL and proxy settings.

Below are some examples of how to customize Faraday requests.
Configuration can be set up with the connection and/or adjusted per request.

As connection options:

```ruby
conn = Faraday.new('http://httpbingo.org', request: { timeout: 5 })
conn.get('/ip')
```

Or as per-request options:

```ruby
conn.get do |req|
  req.url '/ip'
  req.options.timeout = 5
end
```

You can also inject arbitrary data into the request using the `context` option.
This will be available in the `env` on all middleware.

```ruby
conn.get do |req|
  req.url '/get'
  req.options.context = {
    foo: 'foo',
    bar: 'bar'
  }
end
```

## Changing how parameters are serialized

Sometimes you need to send the same URL parameter multiple times with different values.
This requires manually setting the parameter encoder and can be done on
either per-connection or per-request basis.
This applies to all HTTP verbs.

Per-connection setting:

```ruby
conn = Faraday.new request: { params_encoder: Faraday::FlatParamsEncoder }
conn.get('', { roll: ['california', 'philadelphia'] })
```

Per-request setting:

```ruby
conn.get do |req|
  req.options.params_encoder = Faraday::FlatParamsEncoder
  req.params = { roll: ['california', 'philadelphia'] }
end
```

### Custom serializers

You can build your custom encoder, if you like.

The value of Faraday `params_encoder` can be any object that responds to:

* `#encode(hash) #=> String`
* `#decode(string) #=> Hash`

The encoder will affect both how Faraday processes query strings and how it
serializes POST bodies.

The default encoder is `Faraday::NestedParamsEncoder`.

### Order of parameters

By default, parameters are sorted by name while being serialized.
Since this is really useful to provide better cache management and most servers don't really care about parameters order, this is the default behaviour.
However you might find yourself dealing with a server that requires parameters to be in a specific order.
When that happens, you can configure the encoder to skip sorting them.
This configuration is supported by both the default `Faraday::NestedParamsEncoder` and `Faraday::FlatParamsEncoder`:

```ruby
Faraday::NestedParamsEncoder.sort_params = false
# or
Faraday::FlatParamsEncoder.sort_params = false
```

### Merging query parameters in `build_url`

Query parameters normally come from three sources, in increasing priority:
the connection `params` (including the connection URL's own query string), the
query string of the requested URL, and the hash passed as the second argument.
By default each source replaces the value of the previous ones for the same
key.

`Connection#build_url` accepts an optional third argument with per-key merge
policies, so a single call can reset one parameter while leaving another alone:

```ruby
conn = Faraday.new('http://httpbingo.org/api?page=1&sort[]=name&sort[]=created_at')

# reset "page" but keep the URL's own "sort" values after the connection ones
conn.build_url('items?sort[]=price&sort[]=rating', { page: 3 }, sort: :append)
# => GET http://httpbingo.org/api/items?page=3&sort%5B%5D=name&sort%5B%5D=created_at&sort%5B%5D=price&sort%5B%5D=rating
```

The supported policies are:

| Policy     | Behaviour                                                           |
|------------|---------------------------------------------------------------------|
| `:replace` | The last source defining the key wins (default).                    |
| `:append`  | Values are concatenated in source order.                            |
| `:prepend` | Same as `:append`, but later sources come first.                    |
| `:delete`  | The key is dropped from the result.                                 |

Policies are applied per key, so `{ page: :replace, sort: :append }` is valid.
Values are handed to the current `params_encoder` in a reproducible order: keys
keep the order in which they first appear across the sources, and appended
values keep their per-source order. The same rules apply to relative and
absolute request URLs; an absolute URL still ignores the connection path prefix.

An explicit empty query (a trailing `?`, as in `conn.build_url('items?')`) is
preserved and stays distinguishable from a URL with no query at all.

## Proxy

Faraday will try to automatically infer the proxy settings from your system using [`URI#find_proxy`][ruby-find-proxy].
This will retrieve them from environment variables such as http_proxy, ftp_proxy, no_proxy, etc.
If for any reason you want to disable this behaviour, you can do so by setting the global variable `ignore_env_proxy`:

```ruby
Faraday.ignore_env_proxy = true
```

You can also specify a custom proxy when initializing the connection:

```ruby
conn = Faraday.new('http://www.example.com', proxy: 'http://proxy.com')
```

[ruby-find-proxy]: https://ruby-doc.org/stdlib-2.6.3/libdoc/uri/rdoc/URI/Generic.html#method-i-find_proxy
