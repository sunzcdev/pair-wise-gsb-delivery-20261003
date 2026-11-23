Feature: Complete Re-recording

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

  Background:
    Given a file named "vcr_config.rb" with:
      """ruby
      $server = start_sinatra_app do
        get('/one') { 'New Response One' }
        get('/two') { 'New Response Two' }
      end

      require 'vcr'

      VCR.configure do |c|
        c.hook_into :webmock
        c.cassette_library_dir = 'cassettes'
      end
      """
    And a previously recorded cassette file "cassettes/example.yml" with:
      """yaml
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
      """
    And a file named "re_record.rb" with:
      """ruby
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
      """

  Scenario: An interrupted refresh keeps the previous cassette
    Given it is Tue, 09 Nov 2011
    And I set the "FAIL_PART_WAY" environment variable to "1"
    When I run `ruby re_record.rb`
    Then the output should contain "New Response One"
    And the output should contain "Error: boom"
    And the file "cassettes/example.yml" should contain "Old Response One"
    And the file "cassettes/example.yml" should contain "Old Response Two"
    And the file "cassettes/example.yml" should contain "Tue, 01 Nov 2011 04:58:44 GMT"
    But the file "cassettes/example.yml" should not contain "New Response"
    And the file "cassettes/example-pending.yml" should contain "http://localhost/two"
    And the file "cassettes/example-pending.yml" should contain "New Response One"

  Scenario: Offline playback still works after an interrupted refresh
    Given it is Tue, 09 Nov 2011
    And I set the "FAIL_PART_WAY" environment variable to "1"
    And a file named "replay.rb" with:
      """ruby
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
      """
    When I run `ruby re_record.rb`
    Then the output should contain "Error: boom"
    When I run `ruby replay.rb`
    Then the output should contain "Old Response One"
    And the output should contain "Old Response Two"
    But the output should not contain "New Response"

  Scenario: A later complete refresh switches the cassette
    Given it is Tue, 09 Nov 2011
    When I run `ruby re_record.rb`
    Then the output should contain "New Response One"
    And the output should contain "New Response Two"
    And the output should not contain "Error:"
    And the file "cassettes/example.yml" should contain "New Response One"
    And the file "cassettes/example.yml" should contain "New Response Two"
    But the file "cassettes/example.yml" should not contain "Old Response"
    And the file "cassettes/example-pending.yml" should not exist
