# Raising Errors

The `RaiseError` middleware raises a `Faraday::Error` exception if an HTTP
response returns with a 4xx or 5xx status code.
This greatly increases the ease of use of Faraday, as you don't have to check
the response status code manually.
These errors add to the list of default errors [raised by Faraday](getting-started/errors.md).

All exceptions are initialized with a hash containing the response `status`, `headers`, and `body`.

```ruby
conn = Faraday.new(url: 'http://httpbingo.org') do |faraday|
  faraday.response :raise_error # raise Faraday::Error on status code 4xx or 5xx
end

begin
  conn.get('/wrong-url') # => Assume this raises a 404 response
rescue Faraday::ResourceNotFound => e
  e.response_status   #=> 404
  e.response_headers  #=> { ... }
  e.response_body     #=> "..."
end
```

Specific exceptions are raised based on the HTTP Status code of the response.

## 4xx Errors

An HTTP status in the 400-499 range typically represents an error
by the client. They raise error classes inheriting from `Faraday::ClientError`.

| Status Code                                                         | Exception Class                     |
|---------------------------------------------------------------------|-------------------------------------|
| [400](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/400) | `Faraday::BadRequestError`          |
| [401](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/401) | `Faraday::UnauthorizedError`        |
| [403](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/403) | `Faraday::ForbiddenError`           |
| [404](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/404) | `Faraday::ResourceNotFound`         |
| [407](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/407) | `Faraday::ProxyAuthError`           |
| [408](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/408) | `Faraday::RequestTimeoutError`      |
| [409](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409) | `Faraday::ConflictError`            |
| [422](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/422) | `Faraday::UnprocessableContentError` |
| [429](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/429) | `Faraday::TooManyRequestsError` |
| 4xx (any other)                                                     | `Faraday::ClientError`              |

## 5xx Errors

An HTTP status in the 500-599 range represents a server error, and raises a
`Faraday::ServerError` exception.

It's important to note that this exception is only returned if we receive a response and the
HTTP status in such response is in the 500-599 range.
Other kind of errors normally attributed to errors in the 5xx range (such as timeouts, failure to connect, etc...)
are raised as specific exceptions inheriting from `Faraday::Error`.
See [Faraday Errors](getting-started/errors.md) for more information on these.

### Missing HTTP status

The HTTP response status may be nil due to a malformed HTTP response from the
server, or a bug in the underlying HTTP library. This is considered a server error
and raised as `Faraday::NilStatusError`, which inherits from `Faraday::ServerError`.

## Middleware Options

The behavior of this middleware can be customized with the following options:

| Option               | Default | Description |
|----------------------|---------|-------------|
| **include_request**  | true    | When true, exceptions are initialized with request information including `method`, `url`, `url_path`, `params`, `headers`, and `body`. |
| **allowed_statuses** | []      | An array of status codes that should not raise an error. |
| **response_stages**  | false   | When true, exceptions carry an optional two-stage view of the response, separating the adapter-fresh response from the response after middleware transformations. |

### Example Usage

```ruby
conn = Faraday.new(url: 'http://httpbingo.org') do |faraday|
  faraday.response :raise_error, include_request: true, allowed_statuses: [404]
end

begin
  conn.get('/wrong-url')           # => Assume this raises a 404 response
  conn.get('/protected-url')       # => Assume this raises a 401 response
rescue Faraday::UnauthorizedError => e
  e.response[:status]              # => 401
  e.response[:headers]             # => { ... }
  e.response[:body]                # => "..."
  e.response[:request][:url_path]  # => "/protected-url"
end
```

In this example, a `Faraday::UnauthorizedError` exception is raised for the `/protected-url` request, while the
`/wrong-url` request does not raise an error because the status code `404` is in the `allowed_statuses` array.

### Excluding request information from exceptions

When `include_request` is set to `false`, request information is omitted not only from the errors raised by this
middleware, but also from errors raised for the same request by other middlewares (such as `Faraday::ParsingError`
from the `json` middleware) and from errors initialized with the request's `Faraday::Env`. Response information
(status, headers, body) is still included, and logging middlewares are not affected by this option.

When sanitizing errors initialized with a `Faraday::Response` or `Faraday::Env`, `Faraday::Error#response` is a new
`Faraday::Response`. Its Env contains only the response status, headers, and body, so APIs such as `on_complete` remain
available without retaining request fields or custom members from the original Env.

### Two-stage response view

The same HTTP error can look different depending on where `RaiseError` sits in the middleware stack: a response
middleware such as `:json` may have already parsed the body, or the error may be raised before that transformation
runs. Enabling `response_stages: true` attaches a two-stage view to the raised exception so troubleshooting can tell
which fields came straight from the adapter and which were changed by middleware.

```ruby
conn = Faraday.new do |faraday|
  faraday.response :json
  faraday.response :raise_error, response_stages: true
  faraday.adapter Faraday.default_adapter
end

begin
  conn.get('/failing-resource')
rescue Faraday::Error => e
  e.response            # unchanged: the response at the point the error was raised
  e.response_stages     # #<Faraday::ResponseStages ...>, or nil when the option is off
  e.raw_response        # frozen snapshot of the adapter response (String body, original headers)
  e.adapter_response    # alias of raw_response
  e.current_response    # the response at raise time, after earlier middleware ran
end
```

`raw_response` (and its `adapter_response` alias) is a frozen snapshot taken when the adapter finished, before any
response middleware transformed the response. `current_response` is the same object returned by the legacy
`response` reader. When the option is not enabled, `response_stages`, `raw_response`, `adapter_response`, and
`current_response` are all `nil`, and `response` behaves exactly as before. The private transport key used to carry
the raw snapshot is consumed by `Faraday::Error` and never appears in `response`, and it is removed from successful
responses that do not raise.

The two stages make stack ordering visible. Middleware complete responses in reverse registration order, so the last
registered response middleware sees the adapter-fresh response first:

```ruby
# RaiseError registered first => Json completes before the error is raised,
# so current_response holds the parsed body.
Faraday.new do |b|
  b.response :raise_error, response_stages: true
  b.response :json
  b.adapter :test
end

# Json registered first => RaiseError completes before Json ever runs,
# so both stages hold the raw, unparsed adapter response.
Faraday.new do |b|
  b.response :json
  b.response :raise_error, response_stages: true
  b.adapter :test
end
```

The view honors `include_request: false`: request information is omitted from both stages and from the error message,
while the response status, headers, and body remain available in each stage.
