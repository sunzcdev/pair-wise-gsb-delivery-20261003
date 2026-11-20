# Read only cassette

By default a cassette both plays back previously recorded HTTP interactions and
records new ones, according to its [record mode](/record_modes). Sometimes you
need a cassette to be used strictly for playback: for example, a shared
"parent" cassette that another thread has inserted and that must stay immutable
for every context linking to it.

Passing `read_only: true` keeps the cassette's recorded interactions available
for request matching (and as a fallback for nested cassettes), while refusing
to store any new interaction and never rewriting the cassette file on eject.

Cassettes [linked from another thread](/test_frameworks) are always read-only,
regardless of this option, so requests that fall through to the shared parent
cassette cannot be written into its fixture.

## Background

_Given_ a previously recorded cassette file "cassettes/shared.yml" with:

```yaml
---
http_interactions:
- request:
    method: get
    uri: http://example.com/recorded
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
      - "17"
    body:
      encoding: UTF-8
      string: Recorded response
    http_version: "1.1"
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
recorded_with: VCR 2.0.0
```

## A read-only cassette replays but never records

_Given_ a file named "read_only.rb" with:

```ruby
require 'vcr'

VCR.configure do |c|
  c.hook_into :webmock
  c.cassette_library_dir = 'cassettes'
  c.allow_http_connections_when_no_cassette = false
end

VCR.use_cassette('shared', record: :new_episodes, read_only: true) do |cassette|
  puts "recording? => #{cassette.recording?}"

  # Recorded interactions still play back as usual.
  puts WebMock::API.stub_request(:get, 'http://example.com/recorded')
end
```

_When_ I run `ruby read_only.rb`

_Then_ it should pass with:

```
recording? => false
```

_And_ the file "cassettes/shared.yml" should contain "Recorded response".

## Linked cassettes are implicitly read-only

_Given_ a file named "linked.rb" with:

```ruby
require 'vcr'

VCR.configure do |c|
  c.hook_into :webmock
  c.cassette_library_dir = 'cassettes'
end

VCR.insert_cassette('shared', record: :new_episodes)

Thread.new do
  # A new thread links its VCR context to the inserting thread, so the shared
  # cassette is wrapped in a VCR::LinkedCassette and becomes read-only.
  VCR.turned_on?
  puts "linked?    => #{VCR.current_cassette.linked?}"
  puts "read_only? => #{VCR.current_cassette.read_only?}"

  # New interactions offered to the linked cassette are silently dropped;
  # the shared fixture file is never rewritten.
  request  = VCR::Request.new(:get, 'http://example.com/new')
  response = VCR::Response.new(VCR::ResponseStatus.new(200), nil, 'new', '1.1')
  VCR.record_http_interaction(VCR::HTTPInteraction.new(request, response))
end.join

VCR.eject_cassette
```

_When_ I run `ruby linked.rb`

_Then_ it should pass with:

```
linked?    => true
read_only? => true
```

_And_ the file "cassettes/shared.yml" should not contain "http://example.com/new".
