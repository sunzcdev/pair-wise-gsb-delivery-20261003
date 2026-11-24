# Preserve body end

HTTP response bodies are delimited in different ways: most carry an
explicit `Content-Length`, some (e.g. HEAD or 304 Not Modified responses)
carry no body at all, and some carry no `Content-Length` because the body
ended when the connection closed.

When the `:preserve_body_end` option is set to a truthy value, VCR records
how each response body was delimited and restores those semantics when the
cassette is played back:

  - A response whose body ended when the connection closed is played back
    without a `Content-Length` header, even if a `before_playback` hook or
    the HTTP adapter added a temporary one.
  - A response with a declared `Content-Length` gets it updated to match
    the played back body, like `:update_content_length_header` does.
  - A response that cannot carry a body keeps its recorded `Content-Length`
    untouched.

The semantics are stored in the cassette, so they survive the cassette
being saved and reopened.

## Background

_Given_ a previously recorded cassette file "cassettes/example.yml" with:

```yaml
---
http_interactions:
- request:
    method: get
    uri: http://example.com/connection-close
    body:
      encoding: UTF-8
      string: ""
    headers: {}
  response:
    status:
      code: 200
      message: OK
    headers: {}
    body:
      encoding: UTF-8
      string: close-delimited
    http_version: "1.1"
    adapter_metadata:
      vcr_body_end: connection_close
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
- request:
    method: get
    uri: http://example.com/zero
    body:
      encoding: UTF-8
      string: ""
    headers: {}
  response:
    status:
      code: 200
      message: OK
    headers:
      Content-Length:
      - "0"
    body:
      encoding: UTF-8
      string: ""
    http_version: "1.1"
    adapter_metadata:
      vcr_body_end: content_length
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
- request:
    method: head
    uri: http://example.com/head
    body:
      encoding: UTF-8
      string: ""
    headers: {}
  response:
    status:
      code: 200
      message: OK
    headers:
      Content-Length:
      - "42"
    body:
      encoding: UTF-8
      string: ""
    http_version: "1.1"
    adapter_metadata:
      vcr_body_end: no_body
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
- request:
    method: get
    uri: http://example.com/normal
    body:
      encoding: UTF-8
      string: ""
    headers: {}
  response:
    status:
      code: 200
      message: OK
    headers:
      Content-Length:
      - "4"
    body:
      encoding: UTF-8
      string: body
    http_version: "1.1"
    adapter_metadata:
      vcr_body_end: content_length
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
recorded_with: VCR 2.0.0
```

_And_ a file named "common_stuff.rb" with:

```ruby
require 'vcr'

VCR.configure do |c|
  c.cassette_library_dir = 'cassettes'
  c.hook_into :webmock

  c.before_playback do |interaction|
    response = interaction.response

    case interaction.request.uri
    when /connection-close/
      response.body = 'a much longer close-delimited body'
      # Simulate the HTTP adapter adding a temporary Content-Length.
      response.headers['Content-Length'] = ['999']
    when /normal/
      response.body = 'a much longer normal body'
    end
  end
end

def print_result(method, path)
  response = Net::HTTP.start('example.com', 80) { |http| http.send(method, path) }
  puts "#{method} #{path}: body=#{response.body.to_s.inspect} content-length=#{response['Content-Length'].inspect}"
end
```

## Default :preserve_body_end setting

_Given_ a file named "default.rb" with:

```ruby
require 'common_stuff'

VCR.use_cassette('example') do
  print_result('get', '/connection-close')
  print_result('get', '/zero')
  print_result('head', '/head')
  print_result('get', '/normal')
end
```

_When_ I run `ruby default.rb`

_Then_ the output should contain:

```
get /connection-close: body="a much longer close-delimited body" content-length="999"
get /zero: body="" content-length="0"
head /head: body="" content-length="42"
get /normal: body="a much longer normal body" content-length="4"
```

## preserve_body_end: true

_Given_ a file named "true.rb" with:

```ruby
require 'common_stuff'

VCR.use_cassette('example', preserve_body_end: true) do
  print_result('get', '/connection-close')
  print_result('get', '/zero')
  print_result('head', '/head')
  print_result('get', '/normal')
end
```

_When_ I run `ruby true.rb`

_Then_ the output should contain:

```
get /connection-close: body="a much longer close-delimited body" content-length=nil
get /zero: body="" content-length="0"
head /head: body="" content-length="42"
get /normal: body="a much longer normal body" content-length="25"
```
