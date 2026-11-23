# Complete Re-recording

The `:re_record_interval` option refreshes a cassette once its earliest
  `recorded_at` timestamp is older than the interval. By default that refresh
  behaves exactly like a `record: :all` run: on eject, whatever was recorded
  during the run is written over the cassette file.

  That is a problem when the refresh does not complete. If only some of the
  requests were made (because the test failed part way through, or a dependent
  service was unavailable), the partially refreshed cassette replaces the
  previous one. The interactions that were not re-recorded are carried over
  from the old cassette, so the file looks up to date even though some
  responses are still stale, and every interaction gets a fresh
  `recorded_at`. On the next run the interval no longer triggers, so nothing
  ever notices that part of the cassette was never refreshed.

  Set `re_record_mode: :complete` to treat an interval-triggered refresh as
  all-or-nothing. The previous cassette is only replaced once every
  interaction it contains has a fresh recording from the same run; otherwise
  it keeps being used for playback and the partial attempt plus diagnostics
  about the missing interactions are written to a sibling
  `<cassette-name>-pending` file.

  Because the preserved cassette keeps its original `recorded_at` values the
  interval still looks elapsed, so the refresh is retried on the next run. On
  success the cassette is replaced -- atomically for the built-in file system
  persister -- and the pending file is removed.

  The option only applies to interval-triggered re-records; a plain
  `record: :all` run is unaffected, and `re_record_mode: :overwrite` (the
  default) keeps the historical behaviour. `record_on_error: false` still wins:
  when the run failed, nothing is persisted at all and the attempt is reported
  as `aborted`.

## Diagnostics

The staged file contains a `re_record_diagnostics` section:

```yaml
re_record_diagnostics:
  status: incomplete
  cassette: example
  re_record_interval: 604800
  attempted_at: Wed, 09 Nov 2011 10:00:00 GMT
  previous_recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
  expected_interaction_count: 2
  recorded_interaction_count: 1
  missing_interaction_count: 1
  missing_interactions:
  - method: get
    uri: http://localhost/two
    recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
  run_failed: true
  previous_cassette_preserved: true
```

`status` is one of `complete`, `incomplete`, `aborted` or `empty`. The same
  hash is available at runtime as
  `VCR.current_cassette.re_record_diagnostics`, and
  `VCR.current_cassette.re_record_pending_file` gives the staged file's path.

## Background

_Given_ a file named "vcr_config.rb" with:

```ruby
$server = start_sinatra_app do
  get('/one') { 'New Response One' }
  get('/two') { 'New Response Two' }
end

require 'vcr'

VCR.configure do |c|
  c.hook_into :webmock
  c.cassette_library_dir = 'cassettes'
end
```

_And_ a previously recorded cassette file "cassettes/example.yml" with:

```yaml
---
http_interactions:
- request:
    method: get
    uri: http://localhost/one
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
      - "16"
    body:
      encoding: UTF-8
      string: Old Response One
    http_version: "1.1"
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
- request:
    method: get
    uri: http://localhost/two
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
      - "16"
    body:
      encoding: UTF-8
      string: Old Response Two
    http_version: "1.1"
  recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
recorded_with: VCR 3.0.0
```

_And_ a file named "re_record.rb" with:

```ruby
require 'vcr_config'

begin
  VCR.use_cassette('example',
    re_record_interval: 7.days,
    re_record_mode: :complete,
    match_requests_on: [:method, :host, :path]) do

    puts Net::HTTP.get_response('localhost', '/one', $server.port).body
    # Simulate a test that fails before the second request is made.
    raise 'boom' if ENV['FAIL_PART_WAY']
    puts Net::HTTP.get_response('localhost', '/two', $server.port).body
  end
rescue StandardError => e
  puts "Error: #{e.message}"
end
```

## An interrupted refresh keeps the previous cassette

_Given_ it is Tue, 09 Nov 2011

_And_ I set the "FAIL_PART_WAY" environment variable to "1"

_When_ I run `ruby re_record.rb`

_Then_ the output should contain "New Response One"

_And_ the output should contain "Error: boom"

_And_ the file "cassettes/example.yml" should contain "Old Response One"

_And_ the file "cassettes/example.yml" should contain "Old Response Two"

_And_ the file "cassettes/example.yml" should contain "Tue, 01 Nov 2011 04:58:44 GMT"

_But_ the file "cassettes/example.yml" should not contain "New Response"

_And_ the file "cassettes/example-pending.yml" should contain "http://localhost/two"

_And_ the file "cassettes/example-pending.yml" should contain "New Response One"

## Offline playback still works after an interrupted refresh

_Given_ it is Tue, 09 Nov 2011

_And_ I set the "FAIL_PART_WAY" environment variable to "1"

_And_ a file named "replay.rb" with:

```ruby
require 'vcr'

VCR.configure do |c|
  c.hook_into :webmock
  c.cassette_library_dir = 'cassettes'
end

VCR.use_cassette('example', record: :none,
  match_requests_on: [:method, :host, :path]) do
  puts Net::HTTP.get_response('localhost', '/one').body
  puts Net::HTTP.get_response('localhost', '/two').body
end
```

_When_ I run `ruby re_record.rb`

_Then_ the output should contain "Error: boom"

_When_ I run `ruby replay.rb`

_Then_ the output should contain "Old Response One"

_And_ the output should contain "Old Response Two"

_But_ the output should not contain "New Response"

## A later complete refresh switches the cassette

_Given_ it is Tue, 09 Nov 2011

_When_ I run `ruby re_record.rb`

_Then_ the output should contain "New Response One"

_And_ the output should contain "New Response Two"

_And_ the output should not contain "Error:"

_And_ the file "cassettes/example.yml" should contain "New Response One"

_And_ the file "cassettes/example.yml" should contain "New Response Two"

_But_ the file "cassettes/example.yml" should not contain "Old Response"

_And_ the file "cassettes/example-pending.yml" should not exist
