# frozen_string_literal: true

module Faraday
  # RoundtripParamsEncoder is an optional params encoder that makes
  # encode/decode a lossless round-trip.
  #
  # The default NestedParamsEncoder and FlatParamsEncoder collapse several
  # Ruby values onto the same query representation, so a downstream service
  # (and {.decode}) can no longer tell them apart:
  #
  #   * +nil+    -> +a+      (a bare key, no ++=)
  #   * +""+     -> +a=+     (an empty string)
  #   * +[]+     -> +a[]+    (an empty array)
  #   * +["x"]+  -> +a[0]=x+ (a single-element array)
  #
  # This encoder keeps all of them distinct. It also preserves the order of
  # repeated values (including duplicate keys in an Array input) and lets user
  # keys containing the structural characters +[+ and +]+ survive the trip:
  # structural brackets are emitted literally, while every user-supplied key
  # segment is percent-escaped, so a key such as +a[b]+ is encoded as
  # +a%5Bb%5D+ and never mistaken for nesting.
  #
  # Wire format:
  #
  #   encode('a' => nil)        # => 'a'
  #   encode('a' => '')         # => 'a='
  #   encode('a' => [])         # => 'a[]'
  #   encode('a' => ['x'])      # => 'a[0]=x'
  #   encode('a' => { 'b' => 1 }) # => 'a[b]=1'
  #   encode('a[b]' => 1)       # => 'a%5Bb%5D=1'
  #
  # The default NestedParamsEncoder and FlatParamsEncoder are unchanged.
  module RoundtripParamsEncoder
    class << self
      extend Forwardable

      def_delegators :'Faraday::Utils', :escape, :unescape

      attr_accessor :sort_params
    end

    # Useful default for OAuth and caching, matching the other encoders.
    @sort_params = true

    # Marker appended to a key to represent an empty Array. It never appears
    # for any other value because an empty key segment is not otherwise
    # produced.
    EMPTY_ARRAY_SUFFIX = '[]'
    # Structural bracket delimiters, emitted literally (unlike user key
    # segments, which are percent-escaped).
    OPEN_BRACKET = '['
    CLOSE_BRACKET = ']'
    NUMERIC_SEGMENT_REGEX = /\A\d+\z/

    # Encode converts the given params into a URI querystring in which empty
    # arrays, nil fields and empty strings remain distinguishable, repeated
    # values keep their order, and special (bracket-containing) keys are
    # escaped.
    #
    # @param params [nil, Array, #to_hash] parameters to be encoded
    #
    # @return [String, nil] the encoded params
    #
    # @raise [TypeError] if params can not be converted to a Hash
    def self.encode(params)
      return nil if params.nil?

      unless params.is_a?(Array)
        unless params.respond_to?(:to_hash)
          raise TypeError, "Can't convert #{params.class} into Hash."
        end

        params = params.to_hash
        params = params.map do |key, value|
          key = key.to_s if key.is_a?(Symbol)
          [key, value]
        end

        # Only to be used for non-Array inputs. Arrays should preserve order.
        params.sort! if sort_params
      end

      buffer = +''
      params.each do |key, value|
        buffer << "#{encode_pair(escape(key), value)}&"
      end
      buffer.chop
    end

    # Decode converts a querystring produced by {.encode} back into the
    # original structure.
    #
    # @param query [nil, String] query arguments to parse
    #
    # @return [Hash, nil] the decoded params
    #
    # @raise [TypeError] if the nesting is inconsistent (e.g. the same path is
    #   used as both a Hash and an Array)
    def self.decode(query)
      return nil if query.nil?

      root = Node.new
      query.split('&').each do |pair|
        next if pair.empty?

        raw_key, raw_value = pair.split('=', 2)
        segments = parse_key(raw_key)
        value = raw_value.nil? ? nil : unescape(raw_value.tr('+', ' '))
        root.add(segments, value)
      end
      root.materialize
    end

    # Intermediate tree node built while decoding.
    #
    # A node is in one of six states:
    #   * :unset    - nothing seen yet;
    #   * :scalar   - holds one terminal value (String or nil);
    #   * :repeated - several flat pairs share the key, kept in order;
    #   * :hash     - holds named child nodes;
    #   * :array    - holds numerically indexed child nodes;
    #   * :empty    - an empty Array produced by the +a[]+ marker (it becomes a
    #                 real :array if indexed entries arrive later).
    #
    # Tracking the state explicitly is what keeps a bare scalar, an empty
    # Array and a nested structure at the same key distinguishable, and turns
    # contradictory reuse (e.g. +a=1&a[b]=2+) into a clear TypeError.
    class Node
      def initialize
        @kind = :unset
        @scalar = nil
        @children = {}
      end

      # Records +value+ at the path described by +segments+. The terminal
      # action (scalar / empty-array marker / further nesting) always applies
      # to the node addressed by the LAST segment.
      def add(segments, value)
        segment = segments.first
        remaining = segments.drop(1)

        # A root-level '[]' or other ownerless marker is malformed; mirror the
        # other encoders by ignoring it.
        return if segment.nil? || segment.empty?

        if remaining == ['']
          # 'a[]': the empty-array marker applies to this segment's node.
          open(segment).as_empty_array!
        elsif remaining.empty?
          # Last real segment: store the scalar on this segment's node.
          node = open(segment)
          # A repeated numeric index overwrites, matching the array semantics
          # of the other encoders; a repeated named flat key accumulates.
          node.as_scalar!(value, NUMERIC_SEGMENT_REGEX.match?(segment))
        else
          # Intermediate segment: create it and keep walking down.
          open(segment).add(remaining, value)
        end
      end

      # Converts the intermediate tree into plain Hashes and Arrays.
      def materialize
        case @kind
        when :scalar, :repeated
          @scalar
        when :empty
          []
        when :array
          @children.sort_by { |key, _| key.to_i }.map { |_, node| node.materialize }
        else
          # :unset and :hash
          @children.transform_values(&:materialize)
        end
      end

      protected

      # Stores a terminal value. Repeated named flat pairs are accumulated
      # into an order-preserving Array, so Array inputs carrying duplicate keys
      # round-trip with their order intact. A repeated numeric index instead
      # overwrites, matching the array semantics of the other encoders.
      def as_scalar!(value, indexed)
        if indexed
          become_scalar!(value)
          return
        end

        case @kind
        when :unset
          @kind = :scalar
          @scalar = value
        when :scalar
          @kind = :repeated
          @scalar = [@scalar, value]
        when :repeated
          @scalar << value
        else
          raise TypeError, "conflicting parameter structure: #{@kind} cannot be a scalar"
        end
      end

      def as_empty_array!
        unless @kind == :unset || @kind == :empty || @kind == :array
          raise TypeError, "conflicting parameter structure: #{@kind} cannot be an empty Array"
        end

        @kind = :empty
      end

      private

      # Forces a single terminal value (used for indexed array entries, where
      # writing the same index again replaces the previous value).
      def become_scalar!(value)
        unless @kind == :unset || @kind == :scalar || @kind == :array
          raise TypeError, "conflicting parameter structure: #{@kind} cannot be a scalar"
        end

        @kind = :scalar
        @scalar = value
      end

      # Opens (or creates) the child at +segment+. The segment's numeric-ness
      # pins THIS (parent) node's shape once; the returned child node itself
      # stays unset until its own role is decided.
      def open(segment)
        kind = NUMERIC_SEGMENT_REGEX.match?(segment) ? :array : :hash
        pin_kind!(kind, segment)
        @children[segment] ||= Node.new
      end

      def pin_kind!(kind, segment)
        return if @kind == kind
        return @kind = kind if @kind == :unset
        # An empty-array marker accepts the indexed entries that populate it.
        return @kind = :array if @kind == :empty && kind == :array

        raise TypeError, "expected #{kind} (got #{@kind}) for param `#{segment}'"
      end
    end

    private_constant :Node

    class << self
      private

      def encode_pair(parent, value)
        if value.is_a?(Hash)
          encode_hash(parent, value)
        elsif value.is_a?(Array)
          encode_array(parent, value)
        elsif value.nil?
          parent
        else
          "#{parent}=#{escape(value)}"
        end
      end

      def encode_hash(parent, value)
        pairs = value.map { |key, val| [escape(key), val] }
        pairs.sort! if sort_params

        buffer = +''
        pairs.each do |key, val|
          new_parent = "#{parent}#{OPEN_BRACKET}#{key}#{CLOSE_BRACKET}"
          buffer << "#{encode_pair(new_parent, val)}&"
        end
        buffer.chop
      end

      def encode_array(parent, value)
        return "#{parent}#{EMPTY_ARRAY_SUFFIX}" if value.empty?

        buffer = +''
        value.each_with_index do |val, index|
          new_parent = "#{parent}#{OPEN_BRACKET}#{index}#{CLOSE_BRACKET}"
          buffer << "#{encode_pair(new_parent, val)}&"
        end
        buffer.chop
      end

      # Splits an encoded key into its unescaped structural segments. A
      # trailing empty segment marks an empty array. Brackets that were part
      # of a user key are percent-escaped and therefore never matched here.
      def parse_key(raw_key)
        head, rest = raw_key.split('[', 2)
        segments = [unescape(head)]
        return segments if rest.nil?

        # After splitting off the head, each bracket pair looks like '0]' or
        # 'b]'; the closing bracket is the segment terminator.
        rest.split('[').each do |chunk|
          segment = chunk.sub(/\]\z/, '')
          segments << unescape(segment)
        end
        segments
      end
    end
  end
end
