Feature: Composable request matchers

  Sometimes a request can be matched in more than one way: a legacy API may
  need the body compared for one HTTP method only, while the URL is enough
  for the other methods. `match_requests_on` accepts arrays of matchers only
  and every entry of the array has to match, so declaring such a rule used to
  require one cassette option per case (and a separate test run to try the
  other one).

  `VCR.request_matchers.any_of` and `VCR.request_matchers.all_of` build a
  composable declaration instead. A branch of `any_of` matches as soon as one
  of the given combinations matches, and `all_of` groups matchers that all
  have to match:

      match_requests_on: [
        VCR.request_matchers.any_of(
          VCR.request_matchers.all_of(post_request, :uri, :body),
          VCR.request_matchers.all_of(other_request, :uri)
        )
      ]

  Branches are tried in the order in which they are declared, evaluation
  short-circuits, and a matcher shared by several branches is evaluated only
  once per comparison, so a matcher with side effects is never re-run just to
  try the next combination.

  An array still means "all of these matchers have to match", exactly like it
  always has, so existing cassettes and `:match_requests_on` options keep
  working, and custom matchers keep their `(request_1, request_2)` signature.
  Whichever branch matches, the interaction that is consumed is still the
  first one, in cassette order, so the same-path records below are selected
  the same way whatever the order of the branches.

  Background:
    Given a previously recorded cassette file "cassettes/example.yml" with:
      """yaml
      ---
      http_interactions:
      - request:
          method: post
          uri: http://example.com/legacy/search
          body:
            encoding: UTF-8
            string: q=alpha
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
            string: alpha body record
          http_version: "1.1"
        recorded_at: Tue, 01 Nov 2011 04:58:44 GMT
      - request:
          method: post
          uri: http://example.com/legacy/search
          body:
            encoding: UTF-8
            string: q=beta
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
            string: beta body record
          http_version: "1.1"
        recorded_at: Tue, 01 Nov 2011 04:58:45 GMT
      - request:
          method: get
          uri: http://example.com/legacy/search
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
            - "14"
          body:
            encoding: UTF-8
            string: get url record
          http_version: "1.1"
        recorded_at: Tue, 01 Nov 2011 04:58:46 GMT
      recorded_with: VCR 2.0.0
      """

  Scenario: Only match the body for the HTTP method that needs it
    And a file named "composable_matchers.rb" with:
      """ruby
      include_http_adapter_for("net/http")

      require 'vcr'

      VCR.configure do |c|
        c.hook_into :webmock
        c.cassette_library_dir = 'cassettes'
      end

      post_request  = ->(r1, r2) { r1.method == :post && r2.method == :post }
      other_request = ->(r1, r2) { r1.method != :post && r2.method != :post }

      matchers = [
        VCR.request_matchers.any_of(
          VCR.request_matchers.all_of(post_request, :uri, :body),
          VCR.request_matchers.all_of(other_request, :uri)
        )
      ]

      VCR.use_cassette('example', match_requests_on: matchers) do
        puts "POST q=beta: " + response_body_for(:post, "http://example.com/legacy/search", "q=beta")
      end

      VCR.use_cassette('example', match_requests_on: matchers) do
        puts "POST q=alpha: " + response_body_for(:post, "http://example.com/legacy/search", "q=alpha")
      end

      VCR.use_cassette('example', match_requests_on: matchers) do
        puts "GET: " + response_body_for(:get, "http://example.com/legacy/search")
      end
      """
    When I run `ruby composable_matchers.rb`
    Then it should pass with:
      """
      POST q=beta: beta body record
      POST q=alpha: alpha body record
      GET: get url record
      """

  Scenario: The legacy array keeps meaning that every matcher has to match
    And a file named "legacy_array_matchers.rb" with:
      """ruby
      include_http_adapter_for("net/http")

      require 'vcr'

      VCR.configure do |c|
        c.hook_into :webmock
        c.cassette_library_dir = 'cassettes'
      end

      VCR.use_cassette('example', match_requests_on: [:method, :uri]) do
        puts "POST q=beta: " + response_body_for(:post, "http://example.com/legacy/search", "q=beta")
      end
      """
    When I run `ruby legacy_array_matchers.rb`
    Then it should pass with:
      """
      POST q=beta: alpha body record
      """
