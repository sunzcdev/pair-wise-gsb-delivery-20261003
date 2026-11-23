# Read-only rehearsal of an expected request sequence

Before releasing a client, it is useful to check that a cassette's recorded
  interactions cover the requests the client is expected to make. Replaying
  them for real would consume the records (and, with `allow_playback_repeats`
  disabled, make the real test run fail), so VCR offers a read-only rehearsal.

  A rehearsal follows the same rules as real playback:

    - the cassette's `:match_requests_on` matchers (or matchers given to the
      rehearsal) decide whether an interaction matches,
    - a nested cassette falls back to its outer cassette when it has no
      matching record left, and
    - `:allow_playback_repeats` decides whether an already played back
      interaction may be used again.

  Each rehearsed request consumes the matching interaction only virtually, so
  a sequence of rehearsals reports exactly what real playback would do, in
  order. Playback counters, `before_playback`/`before_record` hooks,
  `before_http_request`/`after_http_request` hooks and the connection state are
  left untouched, and no request is ever sent over the network. The virtual
  number of interactions left after a rehearsal is available from the
  rehearsal, so a whole expected sequence can be checked at once.

## A child cassette falls back to its parent, once

_Given_ a file named "rehearsal.rb" with:

```ruby
require 'vcr'

VCR.configure do |c|
  c.hook_into :webmock
end

def interaction(code, body, uri = 'http://example.com/release')
  VCR::HTTPInteraction.new(
    VCR::Request.new(:get, uri, '', {}),
    VCR::Response.new(VCR::ResponseStatus.new(code, 'OK'), {}, body, '1.1')
  )
end

request = interaction(200, '').request
parent  = VCR::Cassette::HTTPInteractionList.new(
  [interaction(503, 'parent body')], [:method, :uri], false,
  VCR::Cassette::HTTPInteractionList::NullList, '', label: 'shared'
)
child   = VCR::Cassette::HTTPInteractionList.new(
  [interaction(202, 'child body')], [:method, :uri], false, parent, '', label: 'local'
)

puts "real: #{child.response_for(request).status.code}"

transcript = VCR::Cassette::Rehearsal.new(child).rehearse_all([request, request])
transcript.each_with_index do |result, index|
  puts "#{index + 1}: #{result.outcome} from #{result.source} (#{result.label})"
end
puts "virtual remaining: #{transcript.remaining_unused_interaction_count}"

puts "real after rehearsal: #{child.response_for(request).status.code}"
```

_When_ I run `ruby rehearsal.rb`

_Then_ it should pass with:

```
real: 202
1: available from parent (shared)
2: exhausted from none ()
virtual remaining: 0
real after rehearsal: 503
```

## The parent may repeat when playback repeats are allowed

_Given_ a file named "rehearsal_repeats.rb" with:

```ruby
require 'vcr'

VCR.configure do |c|
  c.hook_into :webmock
end

def interaction(code, body, uri = 'http://example.com/release')
  VCR::HTTPInteraction.new(
    VCR::Request.new(:get, uri, '', {}),
    VCR::Response.new(VCR::ResponseStatus.new(code, 'OK'), {}, body, '1.1')
  )
end

request = interaction(200, '').request
parent  = VCR::Cassette::HTTPInteractionList.new(
  [interaction(503, 'parent body')], [:method, :uri], true,
  VCR::Cassette::HTTPInteractionList::NullList, '', label: 'shared'
)
child   = VCR::Cassette::HTTPInteractionList.new(
  [interaction(202, 'child body')], [:method, :uri], false, parent, '', label: 'local'
)

puts "real: #{child.response_for(request).status.code}"

transcript = VCR::Cassette::Rehearsal.new(child).rehearse_all([request, request])
transcript.each_with_index do |result, index|
  puts "#{index + 1}: #{result.outcome} from #{result.source} repeated=#{result.repeated?}"
end

puts "real after rehearsal: #{child.response_for(request).status.code}"
```

_When_ I run `ruby rehearsal_repeats.rb`

_Then_ it should pass with:

```
real: 202
1: available from parent repeated=false
2: available from parent repeated=true
real after rehearsal: 503
```

## A rehearsal leaves the cassette, its hooks and the connection state alone

_Given_ a file named "vcr_config.rb" with:

```ruby
require 'vcr'

$before_playback_runs = 0
$after_http_request_runs = 0

VCR.configure do |c|
  c.hook_into :webmock
  c.cassette_library_dir = 'cassettes'

  c.before_playback { |interaction| $before_playback_runs += 1 }
  c.after_http_request { |request, response| $after_http_request_runs += 1 }
end
```

_And_ a previously recorded cassette file "cassettes/example.yml" with:

```yaml
---
http_interactions:
- request:
    method: get
    uri: http://example.com/foo
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
      - "5"
    body:
      encoding: UTF-8
      string: Hello
    http_version: "1.1"
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
recorded_with: VCR 2.0.0
```

_And_ a file named "rehearsal_cassette.rb" with:

```ruby
require 'vcr_config'

VCR.use_cassette("example", record: :none) do |cassette|
  request = VCR::Request.new(:get, 'http://example.com/foo', '', {})

  cassette.http_interactions # force the cassette to load
  hooks_before        = [$before_playback_runs, $after_http_request_runs]
  recording_before    = cassette.recording?
  connections_before  = VCR.real_http_connections_allowed?

  transcript = cassette.rehearsal.rehearse_all([request, request])
  puts "outcomes: #{transcript.map(&:outcome).inspect}"
  puts "virtual remaining: #{transcript.remaining_unused_interaction_count}"

  puts "hooks unchanged: #{[$before_playback_runs, $after_http_request_runs] == hooks_before}"
  puts "recording unchanged: #{cassette.recording? == recording_before}"
  puts "connections unchanged: #{VCR.real_http_connections_allowed? == connections_before}"
  puts "playback counters unchanged: #{cassette.http_interactions.remaining_unused_interaction_count == 1}"
  puts "nothing recorded: #{cassette.new_recorded_interactions.empty?}"

  puts "real playback: #{Net::HTTP.get_response(URI('http://example.com/foo')).body}"
  puts "after real playback: #{cassette.http_interactions.remaining_unused_interaction_count}"
end
```

_When_ I run `ruby rehearsal_cassette.rb`

_Then_ it should pass with:

```
outcomes: [:available, :exhausted]
virtual remaining: 0
hooks unchanged: true
recording unchanged: true
connections unchanged: true
playback counters unchanged: true
nothing recorded: true
real playback: Hello
after real playback: 0
```