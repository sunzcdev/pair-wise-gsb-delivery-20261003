require 'uri'
require 'vcr/errors'

module VCR
  # Keeps track of the different request matchers.
  class RequestMatcherRegistry

    # The default request matchers used for any cassette that does not
    # specify request matchers.
    DEFAULT_MATCHERS = [:method, :uri]

    # @private
    class Matcher < Struct.new(:callable)
      def matches?(request_1, request_2)
        callable.call(request_1, request_2)
      end
    end

    # A composable request matcher declaration.
    #
    # Build one with {RequestMatcherRegistry#any_of} or
    # {RequestMatcherRegistry#all_of}, then use it wherever a request matcher
    # is accepted: in the `:match_requests_on` cassette option, inside another
    # combination (combinations nest freely), or as the block of
    # {Configuration#register_request_matcher}.
    #
    # @example Only require the body for one HTTP method
    #   post_request = ->(r1, r2) { r1.method == :post && r2.method == :post }
    #   other_request = ->(r1, r2) { r1.method != :post && r2.method != :post }
    #
    #   VCR.use_cassette('legacy_api', match_requests_on: [
    #     VCR.request_matchers.any_of(
    #       VCR.request_matchers.all_of(post_request, :uri, :body),
    #       VCR.request_matchers.all_of(other_request, :uri)
    #     )
    #   ]) do
    #     # ...
    #   end
    #
    # The children are tried in the order in which they were declared and
    # evaluation short-circuits: an `:all` combination stops at the first child
    # that does not match and an `:any` combination stops at the first child
    # that does. The matchers are still called with the usual two requests, so
    # custom matchers keep their signature, and a matcher mentioned by several
    # branches is called at most once per comparison -- a matcher with side
    # effects is never re-run just to try another branch.
    #
    # Which recorded interaction gets consumed is decided by
    # {Cassette::HTTPInteractionList}, in cassette order, regardless of the
    # order of the branches: the composition only answers "does this pair of
    # requests match?".
    class Composition
      # @return [:all, :any] whether every child, or any child, has to match.
      attr_reader :operator

      # @return [Array<Symbol, Array, #call, Composition>] the combined declarations.
      attr_reader :children

      # @return [RequestMatcherRegistry] the registry used to resolve matcher names.
      attr_reader :registry

      # @return [Array<Symbol, #call>] the leaves of the tree, in declaration order.
      attr_reader :declarations

      # @private
      OPERATORS = [:all, :any].freeze

      # Builds a combination. Prefer {RequestMatcherRegistry#any_of} and
      # {RequestMatcherRegistry#all_of} over calling this directly.
      #
      # @param registry [RequestMatcherRegistry] the registry used to resolve matcher names.
      # @param operator [:all, :any] whether every child, or any child, has to match.
      # @param children [Array<Symbol, Array, #call, Composition>] the combined declarations.
      def initialize(registry, operator, children)
        unless OPERATORS.include?(operator)
          raise ArgumentError, "invalid request matcher composition operator: #{operator.inspect}"
        end

        @registry     = registry
        @operator     = operator
        @children     = children.freeze
        @declarations = children.flat_map { |child| RequestMatcherRegistry.declarations_in(child) }.freeze
        freeze
      end

      # A composition behaves like any other request matcher: it takes the same
      # two request arguments, so it can be nested or registered by name.
      def call(request_1, request_2)
        Evaluation.new(registry).evaluate(self, request_1, request_2)
      end

      # Lets a composition be passed as the block of
      # {Configuration#register_request_matcher}. The returned proc remembers
      # the composition it was built from, so that a composition registered by
      # name still shares matcher results with the branches around it.
      def to_proc
        composition = self

        lambda { |request_1, request_2| composition.call(request_1, request_2) }.tap do |callable|
          callable.define_singleton_method(:request_matcher_composition) { composition }
        end
      end

      # @return [Composition, nil] the composition this callable wraps, if any.
      def self.from_callable(callable)
        if callable.is_a?(Composition)
          callable
        elsif callable.respond_to?(:request_matcher_composition)
          callable.request_matcher_composition
        end
      end

      def inspect
        "#{operator == :all ? 'all_of' : 'any_of'}(#{children.map(&:inspect).join(', ')})"
      end
      alias to_s inspect
    end

    # @private
    # Evaluates request matcher declarations against a single pair of requests.
    #
    # One instance is used for one comparison, which is what allows a matcher
    # to be evaluated at most once even when it appears in several branches:
    # results are remembered by matcher, and combinations short-circuit.
    class Evaluation
      def initialize(registry, leaf_callback = nil)
        @registry      = registry
        @leaf_callback = leaf_callback
        # Matchers are remembered by identity: two distinct callables are two
        # matchers, even if one defines `==` to accept the other.
        @results       = {}.compare_by_identity
      end

      # Matches a single declaration, which may be a matcher name, a callable,
      # an array (meaning all of its matchers must match) or a {Composition}.
      def matches?(declaration, request_1, request_2)
        case declaration
        when Composition
          evaluate(declaration, request_1, request_2)
        when Array
          declaration.all? { |child| matches?(child, request_1, request_2) }
        else
          evaluate_leaf(declaration, request_1, request_2)
        end
      end

      # Matches a list of declarations the way the `:match_requests_on` option
      # always has: every declaration has to match. A single declaration
      # (rather than a list) is accepted as well.
      def matches_all?(declarations, request_1, request_2)
        Array(declarations).all? { |declaration| matches?(declaration, request_1, request_2) }
      end

      def evaluate(composition, request_1, request_2)
        @results.fetch(composition) do
          expected = composition.operator == :all

          matched = composition.children.all? do |child|
            matches?(child, request_1, request_2) == expected
          end

          @results[composition] = expected ? matched : !matched
        end
      end

    private

      def evaluate_leaf(leaf, request_1, request_2)
        @results.fetch(leaf) do
          callable = @registry[leaf].callable
          composition = Composition.from_callable(callable)

          matched = if composition
            # A composition registered under a name is evaluated in place, so
            # that its branches share results with the surrounding ones.
            evaluate(composition, request_1, request_2)
          else
            !!callable.call(request_1, request_2)
          end

          @leaf_callback.call(leaf, matched) if @leaf_callback
          @results[leaf] = matched
        end
      end
    end

    # @private
    class URIWithoutParamsMatcher < Struct.new(:params_to_ignore)
      def partial_uri_from(request)
        request.parsed_uri.tap do |uri|
          return uri unless uri.query # ignore uris without params, e.g. "http://example.com/"

          uri.query = uri.query.split('&').tap { |params|
            params.map! do |p|
              key, value = p.split('=')
              key.gsub!(/\[\]\z/, '') # handle params like tag[]=
              [key, value]
            end

            params.reject! { |p| ignored_param?(p.first) }
            params.map!    { |p| p.join('=') }
          }.join('&')

          uri.query = nil if uri.query.empty?
        end
      end

      # URI::DEFAULT_PARSER.unescape warns as obsolete since uri 1.0 (Ruby 3.4)
      PARAM_NAME_UNESCAPER = URI::RFC2396_Parser.new

      # Recorded URIs are usually percent-encoded (WebMock writes `tag[]` as
      # `tag%5B%5D`), so names are compared once decoded, on both sides.
      def self.normalized_param_name(name)
        PARAM_NAME_UNESCAPER.unescape(name.to_s).delete_suffix('[]')
      end

      def ignored_param?(key)
        params_to_ignore.include?(self.class.normalized_param_name(key))
      end

      def call(request_1, request_2)
        partial_uri_from(request_1) == partial_uri_from(request_2)
      end

      def to_proc
        lambda { |r1, r2| call(r1, r2) }
      end
    end

    # @private
    def initialize
      @registry = {}
      register_built_ins
    end

    # @private
    def register(name, &block)
      if @registry.has_key?(name)
        warn "WARNING: There is already a VCR request matcher registered for #{name.inspect}. Overriding it."
      end

      @registry[name] = Matcher.new(block)
    end

    # @private
    def [](matcher)
      @registry.fetch(matcher) do
        matcher.respond_to?(:call) ?
          Matcher.new(matcher) :
          raise_unregistered_matcher_error(matcher)
      end
    end

    # Builds a dynamic request matcher that matches on a URI while ignoring the
    # named query parameters. This is useful for dealing with non-deterministic
    # URIs (i.e. that have a timestamp or request signature parameter).
    #
    # @example
    #   without_timestamp = VCR.request_matchers.uri_without_param(:timestamp)
    #
    #   # use it directly...
    #   VCR.use_cassette('example', match_requests_on: [:method, without_timestamp]) { }
    #
    #   # ...or register it as a named matcher
    #   VCR.configure do |c|
    #     c.register_request_matcher(:uri_without_timestamp, &without_timestamp)
    #   end
    #
    #   VCR.use_cassette('example', match_requests_on: [:method, :uri_without_timestamp]) { }
    #
    # @param ignores [Array<#to_s>] The names of the query parameters to ignore
    # @return [#call] the request matcher
    def uri_without_params(*ignores)
      uri_without_param_matchers[ignores]
    end
    alias uri_without_param uri_without_params

    # Combines request matchers so that the result matches as soon as one of
    # the branches matches. Use it in `:match_requests_on` to declare that
    # several matching rules are acceptable, e.g. to require the body for one
    # HTTP method only:
    #
    #   match_requests_on: [
    #     VCR.request_matchers.any_of([:method, :uri, :body], [:method, :uri])
    #   ]
    #
    # An array nested in a branch means that every matcher of the array has to
    # match, exactly like the top level `:match_requests_on` array does. Names,
    # callables (including matchers returned by {#uri_without_param}) and other
    # combinations can all be mixed in a branch.
    #
    # Branches are tried in declaration order and evaluation short-circuits, so
    # the first branch that matches wins and the remaining branches are not
    # evaluated. A matcher that appears in several branches is still evaluated
    # at most once for a given pair of requests.
    #
    # @param branches [Array<Symbol, Array, #call, Composition>] the branches.
    # @return [Composition] the composable matcher.
    def any_of(*branches)
      Composition.new(self, :any, branches)
    end

    # Combines request matchers so that the result matches only when every
    # one of them matches. This is the explicit form of the implicit
    # combination the `:match_requests_on` array has always performed; it is
    # mostly useful to nest a group of matchers inside an {#any_of} branch:
    #
    #   match_requests_on: [
    #     VCR.request_matchers.any_of(
    #       VCR.request_matchers.all_of(:method, :uri, :body),
    #       VCR.request_matchers.all_of(:method, :uri)
    #     )
    #   ]
    #
    # @param matchers [Array<Symbol, Array, #call, Composition>] the matchers.
    # @return [Composition] the composable matcher.
    def all_of(*matchers)
      Composition.new(self, :all, matchers)
    end

    # Builds the object that evaluates one set of declarations against a pair
    # of requests, remembering each matcher's result.
    #
    # @private
    # @param leaf_callback [#call, nil] called with a matcher and its result
    #  the first time that matcher is evaluated.
    # @return [Evaluation]
    def evaluation(&leaf_callback)
      Evaluation.new(self, leaf_callback)
    end

    # Matches a `:match_requests_on`-style list of declarations, all of which
    # have to match, the way VCR has always matched: every entry of the list
    # is checked.
    #
    # @private
    # @param declarations [Array<Symbol, Array, #call, Composition>]
    # @param leaf_callback [#call, nil] called with a matcher and its result
    #  each time a matcher is evaluated.
    # @return [Boolean]
    def matches_all?(declarations, request_1, request_2, &leaf_callback)
      evaluation(&leaf_callback).matches_all?(declarations, request_1, request_2)
    end

    # Normalizes one declaration into the matchers it is built from, in
    # declaration order. Nested combinations and arrays are flattened; a
    # matcher name or callable is returned as is.
    #
    # A `:match_requests_on` array keeps working the way it always has, so
    # this is all that is needed to know whether a declaration will, say,
    # compare request bodies.
    #
    # @private
    # @param declaration [Symbol, Array, #call, Composition]
    # @return [Array<Symbol, #call>]
    def self.declarations_in(declaration)
      case declaration
      when Composition
        declaration.declarations
      when Array
        declaration.flat_map { |child| declarations_in(child) }
      else
        [declaration]
      end
    end

    # @private
    # @param declarations [Array<Symbol, Array, #call, Composition>]
    # @return [Array<Symbol, #call>]
    def self.declarations_of(declarations)
      Array(declarations).flat_map { |declaration| declarations_in(declaration) }
    end

  private

    def uri_without_param_matchers
      @uri_without_param_matchers ||= Hash.new do |hash, params|
        hash[params] = URIWithoutParamsMatcher.new(params.map { |param| URIWithoutParamsMatcher.normalized_param_name(param) })
      end
    end

    def raise_unregistered_matcher_error(name)
      raise Errors::UnregisteredMatcherError.new \
        "There is no matcher registered for #{name.inspect}. " +
        "Did you mean one of #{@registry.keys.map(&:inspect).join(', ')}?"
    end

    def register_built_ins
      register(:method)  { |r1, r2| r1.method == r2.method }
      register(:uri)     { |r1, r2| r1.parsed_uri == r2.parsed_uri }
      register(:body)    { |r1, r2| r1.body == r2.body }
      register(:headers) { |r1, r2| r1.headers == r2.headers }

      register(:host) do |r1, r2|
        r1.parsed_uri.host.chomp('.') == r2.parsed_uri.host.chomp('.')
      end
      register(:path) do |r1, r2|
        r1.parsed_uri.path == r2.parsed_uri.path
      end

      register(:query) do |r1, r2|
        VCR.configuration.query_parser.call(r1.parsed_uri.query.to_s) ==
          VCR.configuration.query_parser.call(r2.parsed_uri.query.to_s)
      end

      try_to_register_body_as_json
    end

    def try_to_register_body_as_json
      begin
        require 'json'
      rescue LoadError
        return
      end

      register(:body_as_json) do |r1, r2|
        begin
          r1.body == r2.body || JSON.parse(r1.body) == JSON.parse(r2.body)
        rescue JSON::ParserError
          false
        end
      end
    end
  end
end

