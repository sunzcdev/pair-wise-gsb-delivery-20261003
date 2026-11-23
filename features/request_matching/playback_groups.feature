Feature: Playback groups

  When tests run in parallel (for example, in multiple threads or processes)
  and share a single cassette file, the same request can represent a
  different playback sequence in each test. VCR supports this with the
  `:playback_group` cassette option:

      playback_group: 'my-test-group'

  Newly recorded HTTP interactions are tagged with the selected group, and
  the group attribution is kept in the cassette file. Once a group is
  selected, only interactions belonging to that group are eligible for
  playback: both unused interactions and, when `:allow_playback_repeats` is
  set, already-used ones. Interactions recorded without a group (such as
  those in historical cassettes) and cassette usages that do not select a
  group are handled by the original rules.

  @exclude-jruby
  Scenario: Grouped interactions play back independently
    Given a previously recorded cassette file "cassettes/example.yml" with:
      """yaml
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
            - "9"
          body:
            encoding: UTF-8
            string: Group A 1
          http_version: "1.1"
        recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
        playback_group: a
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
            - "9"
          body:
            encoding: UTF-8
            string: Group B 1
          http_version: "1.1"
        recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
        playback_group: b
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
            - "9"
          body:
            encoding: UTF-8
            string: Group A 2
          http_version: "1.1"
        recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
        playback_group: a
      recorded_with: VCR 2.0.0
      """
    And a file named "playback_groups.rb" with:
      """ruby
      include_http_adapter_for("net/http")

      require 'vcr'

      VCR.configure do |c|
        c.hook_into :webmock
        c.cassette_library_dir = 'cassettes'
      end

      VCR.use_cassette('example', playback_group: 'a') do
        puts response_body_for(:get, 'http://example.com/foo')
        puts response_body_for(:get, 'http://example.com/foo')
      end
      VCR.use_cassette('example', playback_group: 'b') do
        puts response_body_for(:get, 'http://example.com/foo')
      end
      """
    When I run `ruby playback_groups.rb`
    Then it should pass with:
      """
      Group A 1
      Group A 2
      Group B 1
      """
