# frozen_string_literal: true

require_relative 'bad_request'
require 'uri'

module Rack
  class QueryParser
    DEFAULT_SEP = /& */n
    COMMON_SEP = { ";" => /; */n, ";," => /[;,] */n, "&" => /& */n }

    # ParameterTypeError is the error that is raised when incoming structural
    # parameters (parsed by parse_nested_query) contain conflicting types.
    class ParameterTypeError < TypeError
      include BadRequest
    end

    # InvalidParameterError is the error that is raised when incoming structural
    # parameters (parsed by parse_nested_query) contain invalid format or byte
    # sequence.
    class InvalidParameterError < ArgumentError
      include BadRequest
    end

    # QueryLimitError is for errors raised when the query provided exceeds one
    # of the query parser limits.
    class QueryLimitError < RangeError
      include BadRequest

      # Human readable names of the counted dimensions.
      COUNTER_NAMES = {
        keys: "number of parameters",
        nodes: "number of expanded parameter nodes",
        bytes: "decoded parameter size",
      }.freeze

      # Build an error that reports both the origin of the offending data
      # (e.g. :query_string or :form_body) and the parsing phase that ran over
      # the budget (e.g. :pair_parsing or :nested_expansion). The origin and
      # phase are also exposed as reader methods so callers can react
      # programmatically.
      def self.build(source, phase, counter, limit, usage)
        error = new(
          "#{source.to_s.tr('_', ' ')} parameter budget exceeded during #{phase.to_s.tr('_', ' ')}: " \
          "#{COUNTER_NAMES.fetch(counter, counter)} (#{usage}) exceeds limit (#{limit})"
        )
        error.set_details(source, phase, counter, limit, usage)
        error
      end

      attr_reader :source, :phase, :counter, :limit, :usage

      def set_details(source, phase, counter, limit, usage)
        @source = source
        @phase = phase
        @counter = counter
        @limit = limit
        @usage = usage
      end
    end

    class IncompatibleEncodingError < EncodingError
      include BadRequest
    end

    # ParamsTooDeepError is the old name for the error that is raised when params
    # are recursively nested over the specified limit. Make it the same as
    # as QueryLimitError, so that code that rescues ParamsTooDeepError error
    # to handle bad query strings also now handles other limits.
    ParamsTooDeepError = QueryLimitError

    # ParamBudget is an explicit, per-call accounting ledger that can be shared
    # across several parsing operations (for example the query string and the
    # form body of a single Rack::Request). It counts three independent
    # quantities so that a single shared budget can be enforced instead of
    # giving every parsed segment a full quota of its own:
    #
    # [+:keys+]  the number of query pairs / form fields consumed.
    # [+:nodes+] the number of structural nodes produced while expanding nested
    #            (and array) parameters, including nodes produced recursively.
    # [+:bytes+] the number of bytes in the decoded keys and values.
    #
    # A ledger is plain instance state: it is never stored globally, it is not
    # shared between requests, and parsing a single query string without one
    # keeps the historical, per-parser limit behavior.
    class ParamBudget
      # The location a charge was made for, used in limit errors.
      SOURCES = [:query_string, :form_body].freeze

      # The parsing phase a charge was made in, used in limit errors.
      PHASES = [:pair_parsing, :nested_expansion].freeze

      attr_reader :keys, :nodes, :bytes
      attr_reader :keys_limit, :nodes_limit, :bytes_limit

      # Build a ledger whose limits are derived from the per-parser limits of a
      # query parser. A nil parser limit means the ledger does not enforce that
      # dimension either.
      def self.from_query_parser(query_parser)
        new(
          keys_limit: query_parser.params_limit,
          bytes_limit: query_parser.bytesize_limit,
          nodes_limit: nil
        )
      end

      def initialize(keys_limit: nil, nodes_limit: nil, bytes_limit: nil)
        @keys_limit = validate_limit(:keys_limit, keys_limit)
        @nodes_limit = validate_limit(:nodes_limit, nodes_limit)
        @bytes_limit = validate_limit(:bytes_limit, bytes_limit)
        @keys = 0
        @nodes = 0
        @bytes = 0
      end

      # Charge one consumed query pair / form field before it is inserted into
      # the parameter structure. The decoded bytes of the key and value are
      # added to the byte counter at the same time.
      def account_pair!(source, key, value)
        decoded_bytes = key.to_s.bytesize + (value.nil? ? 0 : value.to_s.bytesize)

        charge_keys!(source, 1)
        charge_bytes!(source, decoded_bytes)
      end

      # Charge one structural node produced by nested/array expansion, before
      # the node is written into the parameter structure.
      def account_node!(source)
        charge_nodes!(source, 1)
      end

      private

      def validate_limit(name, value)
        unless value.nil? || (value.is_a?(Integer) && value >= 0)
          raise ArgumentError, "#{name} must be a non-negative integer or nil"
        end

        value
      end

      def charge_keys!(source, amount)
        charge!(source, :pair_parsing, :keys, amount, @keys_limit, @keys) do |new_value|
          @keys = new_value
        end
      end

      def charge_nodes!(source, amount)
        charge!(source, :nested_expansion, :nodes, amount, @nodes_limit, @nodes) do |new_value|
          @nodes = new_value
        end
      end

      def charge_bytes!(source, amount)
        charge!(source, :pair_parsing, :bytes, amount, @bytes_limit, @bytes) do |new_value|
          @bytes = new_value
        end
      end

      def charge!(source, phase, counter, amount, limit, current)
        usage = current + amount
        if limit && usage > limit
          raise QueryLimitError.build(source, phase, counter, limit, usage)
        end

        yield usage
      end
    end

    def self.make_default(param_depth_limit, **options)
      new(Params, param_depth_limit, **options)
    end

    attr_reader :param_depth_limit, :params_limit

    env_int = lambda do |key, val|
      # simplecov:disable
      if str_val = ENV[key]
        begin
          val = Integer(str_val, 10)
        rescue ArgumentError
          raise ArgumentError, "non-integer value provided for environment variable #{key}"
        end
      end

      val if val >= 0
      # simplecov:enable
    end

    BYTESIZE_LIMIT = env_int.call("RACK_QUERY_PARSER_BYTESIZE_LIMIT", 4194304)
    private_constant :BYTESIZE_LIMIT

    PARAMS_LIMIT = env_int.call("RACK_QUERY_PARSER_PARAMS_LIMIT", 4096)
    private_constant :PARAMS_LIMIT

    attr_reader :bytesize_limit

    def initialize(params_class, param_depth_limit, bytesize_limit: BYTESIZE_LIMIT, params_limit: PARAMS_LIMIT)
      @params_class = params_class
      @param_depth_limit = param_depth_limit
      @bytesize_limit = bytesize_limit
      @params_limit = params_limit
    end

    # Build a fresh ParamBudget whose limits are derived from this parser's
    # per-parser limits. Intended to be called once per request so that the
    # query string and the form body draw from a single shared budget.
    def make_budget
      ParamBudget.from_query_parser(self)
    end

    # Stolen from Mongrel, with some small modifications:
    # Parses a query string by breaking it up at the '&'.  You can also use this
    # to parse cookies by changing the characters used in the second parameter
    # (which defaults to '&').
    #
    # If a ParamBudget is given, the parsed pairs (and their decoded bytes) are
    # charged to it before any parameter is inserted, otherwise the parser's
    # own limits are enforced as before.
    def parse_query(qs, separator = nil, budget: nil, source: :query_string, &unescaper)
      params = make_params

      each_query_pair(qs, separator, unescaper, budget: budget, source: source) do |k, v|
        if cur = params[k]
          if cur.class == Array
            params[k] << v
          else
            params[k] = [cur, v]
          end
        else
          params[k] = v
        end
      end

      return params.to_h
    end

    # Parses a query string by breaking it up at the '&', returning all key-value
    # pairs as an array of [key, value] arrays. Unlike parse_query, this preserves
    # all duplicate keys rather than collapsing them.
    def parse_query_pairs(qs, separator = nil, budget: nil, source: :query_string)
      pairs = []

      each_query_pair(qs, separator, nil, budget: budget, source: source) do |k, v|
        pairs << [k, v]
      end

      pairs
    end

    # parse_nested_query expands a query string into structural types. Supported
    # types are Arrays, Hashes and basic value types. It is possible to supply
    # query strings with parameters of conflicting types, in this case a
    # ParameterTypeError is raised. Users are encouraged to return a 400 in this
    # case.
    #
    # If a ParamBudget is given, pair counts and decoded bytes are charged
    # during pair parsing and every expanded structural node (including the
    # elements of nested arrays) is charged during expansion, all before the
    # affected structure is mutated.
    def parse_nested_query(qs, separator = nil, budget: nil, source: :query_string)
      params = make_params

      each_query_pair(qs, separator, nil, budget: budget, source: source) do |k, v|
        _normalize_params(params, k, v, 0, budget, source)
      end

      return params.to_h
    end

    # normalize_params recursively expands parameters into structural types. If
    # the structural types represented by two different parameter names are in
    # conflict, a ParameterTypeError is raised.  The depth argument is deprecated
    # and should no longer be used, it is kept for backwards compatibility with
    # earlier versions of rack.
    #
    # When a ParamBudget is given, every structural node produced is charged
    # before the parameter structure is mutated, so a conflicting scalar/map
    # pair can never overwrite an existing value before running the budget dry.
    def normalize_params(params, name, v, _depth=nil, budget: nil, source: :form_body)
      _normalize_params(params, name, v, 0, budget, source)
    end

    private def _normalize_params(params, name, v, depth, budget, source)
      raise ParamsTooDeepError if depth >= param_depth_limit

      if !name
        # nil name, treat same as empty string (required by tests)
        k = after = ''
      elsif depth == 0
        # Start of parsing, don't treat [] or [ at start of string specially
        if start = name.index('[', 1)
          # Start of parameter nesting, use part before brackets as key
          k = name[0, start]
          after = name[start, name.length]
        else
          # Plain parameter with no nesting
          k = name
          after = ''
        end
      elsif name.start_with?('[]')
        # Array nesting
        k = '[]'
        after = name[2, name.length]
      elsif name.start_with?('[') && (start = name.index(']', 1))
        # Hash nesting, use the part inside brackets as the key
        k = name[1, start-1]
        after = name[start+1, name.length]
      else
        # Probably malformed input, nested but not starting with [
        # treat full name as key for backwards compatibility.
        k = name
        after = ''
      end

      return if k.empty?

      # Charge the structural node before any mutation below, so an
      # over-budget or conflicting pair never partially overwrites params.
      budget&.account_node!(source)

      if after == ''
        if k == '[]' && depth != 0
          return [v]
        else
          params[k] = v
        end
      elsif after == "["
        params[name] = v
      elsif after == "[]"
        params[k] ||= []
        raise ParameterTypeError, "expected Array (got #{params[k].class.name}) for param `#{k}'" unless params[k].is_a?(Array)
        params[k] << v
      elsif after.start_with?('[]')
        # Recognize x[][y] (hash inside array) parameters
        unless after[2] == '[' && after.end_with?(']') && (child_key = after[3, after.length-4]) && !child_key.empty? && !child_key.index('[') && !child_key.index(']')
          # Handle other nested array parameters
          child_key = after[2, after.length]
        end
        params[k] ||= []
        raise ParameterTypeError, "expected Array (got #{params[k].class.name}) for param `#{k}'" unless params[k].is_a?(Array)
        if params_hash_type?(params[k].last) && !params_hash_has_key?(params[k].last, child_key)
          _normalize_params(params[k].last, child_key, v, depth + 1, budget, source)
        else
          params[k] << _normalize_params(make_params, child_key, v, depth + 1, budget, source)
        end
      else
        params[k] ||= make_params
        raise ParameterTypeError, "expected Hash (got #{params[k].class.name}) for param `#{k}'" unless params_hash_type?(params[k])
        params[k] = _normalize_params(params[k], after, v, depth + 1, budget, source)
      end

      params
    rescue Encoding::CompatibilityError
      raise IncompatibleEncodingError
    end

    def make_params
      @params_class.new
    end

    def new_depth_limit(param_depth_limit)
      self.class.new @params_class, param_depth_limit
    end

    private

    def params_hash_type?(obj)
      obj.kind_of?(@params_class)
    end

    def params_hash_has_key?(hash, key)
      return false if /\[\]/.match?(key)

      key.split(/[\[\]]+/).inject(hash) do |h, part|
        next h if part == ''
        return false unless params_hash_type?(h) && h.key?(part)
        h[part]
      end

      true
    end

    def each_query_pair(qs, separator, unescaper = nil, budget: nil, source: :query_string)
      return if !qs || qs.empty?

      if @bytesize_limit && qs.bytesize > @bytesize_limit
        raise_limit(source, :pair_parsing,
          "total query size exceeds limit (#{@bytesize_limit})")
      end

      sep = separator ? (COMMON_SEP[separator] || /[#{separator}] */n) : DEFAULT_SEP
      pairs = @params_limit ? qs.split(sep, @params_limit + 1) : qs.split(sep)

      if @params_limit && pairs.size > @params_limit
        param_count = pairs.size + pairs.last.count(separator || "&")
        raise_limit(source, :pair_parsing,
          "total number of query parameters (#{param_count}) exceeds limit (#{@params_limit})")
      end

      if unescaper
        pairs.each do |p|
          next if p.empty?
          k, v = p.split('=', 2).map!(&unescaper)
          # Charge the pair (and decoded bytes) before the caller inserts it.
          budget&.account_pair!(source, k, v)
          yield k, v
        end
      else
        pairs.each do |p|
          next if p.empty?
          k, v = p.split('=', 2).map! { |s| unescape(s) }
          # Charge the pair (and decoded bytes) before the caller inserts it.
          budget&.account_pair!(source, k, v)
          yield k, v
        end
      end
    rescue ArgumentError => e
      raise InvalidParameterError, e.message, e.backtrace
    end

    # Raise a parser-level limit error, attaching the origin and parsing phase
    # when the information is available so limit errors can be attributed.
    def raise_limit(source, phase, message)
      error = QueryLimitError.new(message)
      error.set_details(source, phase, nil, nil, nil) if source && phase
      raise error
    end

    def unescape(string, encoding = Encoding::UTF_8)
      URI.decode_www_form_component(string, encoding)
    end

    class Params < Hash
      alias_method :to_params_hash, :to_h
    end
  end
end
