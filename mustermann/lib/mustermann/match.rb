# frozen_string_literal: true

module Mustermann
  # The return value of {Mustermann::Pattern#match}, {Mustermann::Pattern#peek_match}, {Mustermann::Set#match}, and similar methods.
  # Mimics large parts of the MatchData API, but also provides access to the pattern and params hash.
  class Match
    # @return [Mustermann::Pattern] the pattern that produced the match
    attr_reader :pattern

    # @return [String] the string that was matched
    attr_reader :string

    # @return [Hash] the params hash
    attr_reader :params

    # @return [Array] the captures array
    attr_reader :captures

    # @return [Hash] the named captures hash, usually identical to {#params}
    attr_reader :named_captures

    # @return [Hash{String => Span, Array<Span>}] captures mapped to their {Span}
    #   in the original input. Missing optional segments are absent from the hash,
    #   while captures matching the empty string have a zero-length {Span}.
    attr_reader :named_spans

    # @return [String] the post match string
    attr_reader :post_match

    # @return [String] the pre match string
    attr_reader :pre_match

    # @return [Regexp, nil] the regular expression that produced the match, if available
    attr_reader :regexp

    # @overload initialize(pattern, string, **options)
    #   @param pattern [Mustermann::Pattern] the pattern that produced the match
    #   @param string [String] the string that was matched
    #
    # @overload initialize(match, **options)
    #   @param match [Mustermann::Match] the match to copy pattern and string from
    #
    # @overload initialize(pattern, match, **options)
    #   @param match [Mustermann::Match, MatchData] the match to copy string from
    #
    # @option options [Array]  :captures the captures array
    # @option options [Hash]   :named_captures the named captures hash
    # @option options [String] :matched the matched substring (defaults to string for full matches)
    # @option options [Hash]   :params the params hash
    # @option options [Regexp] :regexp the regular expression that produced the match
    # @option options [String] :post_match the post match string
    # @option options [String] :pre_match the pre match string
    # @option options [Array<Span, nil>] :spans spans for every capture group, in group order
    # @option options [Hash] :named_spans the named captures mapped to their spans
    def initialize(pattern_or_match, string_or_match = nil, matched: nil, params: nil, post_match: nil, pre_match: nil, captures: nil, named_captures: nil, regexp: nil, spans: nil, named_spans: nil)
      case pattern_or_match
      when Mustermann::Match, MatchData then match = pattern_or_match
      when Mustermann::Pattern          then pattern = pattern_or_match
      else raise ArgumentError, "first argument must be a Mustermann::Pattern or a MatchData, not #{pattern_or_match.class}"
      end

      case string_or_match
      when Mustermann::Match, MatchData then match ||= string_or_match
      when String                       then string = string_or_match
      when nil # ignore
      else raise ArgumentError, "second argument must be a String or a MatchData, not #{string_or_match.class}"
      end

      @pattern        = pattern        || match&.pattern
      @string         = string         || match&.string         || EMPTY_STRING
      @params         = params         || match&.params         || EMPTY_HASH
      @post_match     = post_match     || match&.post_match     || EMPTY_STRING
      @pre_match      = pre_match      || match&.pre_match      || EMPTY_STRING
      @captures       = captures       || match&.captures       || @params.values
      @named_captures = named_captures || match&.named_captures || @params
      @spans          = spans          || match_spans(match)   || EMPTY_ARRAY
      @matched        = matched        || match&.to_s           || @string

      unless @regexp = regexp
        @regexp = match.regexp if match.respond_to?(:regexp)
        @regexp ||= pattern.respond_to?(:regexp) ? pattern.regexp : nil
      end

      @named_spans = named_spans || derive_named_spans
    end

    # @return [Array<Span, nil>, nil] spans copied from another Mustermann::Match
    def match_spans(match)
      match.spans if match.is_a?(Mustermann::Match)
    end

    # @return [Array<String>] the names of the named captures
    def names = named_captures.keys

    # @return [Array<Span, nil>] one span per capture group, in group order.
    #   A nil entry marks an optional group that did not participate in the match,
    #   while a zero-length {Span} marks a group that captured the empty string.
    def spans = @spans.dup

    # Access the {Span} of a named or positional capture.
    #
    # @overload span(key)
    #   @param key [String, Symbol] name of the capture
    #   @return [Span, Array<Span>, nil] the span (or spans for repeated captures),
    #     or nil if the capture did not participate
    #
    # @overload span(index)
    #   @param index [Integer] capture group index (0-based, like {#[]})
    #   @return [Span, nil] the span, or nil for a missing optional capture
    def span(key = nil)
      case key
      when nil, String, Symbol
        key = key.to_s if key.is_a?(Symbol)
        named_spans[key]
      when Integer
        @spans[key]
      else raise ArgumentError, "key must be a String, Symbol, or Integer, not #{key.class}"
      end
    end

    # @overload [](key)
    #   Access named captures by key.
    #   @param key [String, Symbol] the key to access
    #   @return the value of the named capture, or nil if not found
    #
    # @overload [](index)
    #   Access captures by index.
    #   @param index [Integer] the index to access
    #   @return the value of the capture, or nil if not found
    #
    # @overload [](start, length)
    #   Access multiple captures by index and length.
    #   @param start [Integer] the starting index to access
    #   @param length [Integer] the number of captures to access
    #   @return [Array] the values of the captures
    #
    # @overload [](range)
    #   Access multiple captures by range.
    #   @param range [Range] the range of indices to access
    #   @return [Array] the values of the captures
    def [](key, length = nil)
      case key
      when String  then named_captures[key]
      when Symbol  then named_captures[key.to_s]
      when Integer then length ? captures[key, length] : captures[key]
      when Range   then captures[key]
      else raise ArgumentError, "key must be a String, Symbol, Integer, or Range, not #{key.class}"
      end
    end

    # Deconstructs the match into a hash of the given keys. Useful for pattern matching.
    # @param keys [Array] the keys to deconstruct
    # @return [Hash] a hash of the given keys and their corresponding values
    # @see https://docs.ruby-lang.org/en/4.0/syntax/pattern_matching_rdoc.html
    def deconstruct_keys(keys) = keys.to_h { |key| [key, self[key]] }

    # @see Object#hash
    def hash = pattern.hash ^ string.hash ^ params.hash

    # @see Object#eql?
    def eql?(other)
      return false unless other.is_a? self.class
      pattern == other.pattern && string == other.string && params == other.params
    end

    # Returns the values of the given keys as an array.
    # @params keys [Array<Symbol, String>] the keys to access
    # @return [Array] the values of the given keys
    def values_at(*keys) = keys.map { |key| self[key] }

    # Returns the {Span Span} objects for the given keys.
    # @params keys [Array<Symbol, String, Integer>] the keys to access
    # @return [Array<Span, Array<Span>, nil>] the spans
    def spans_at(*keys) = keys.map { |key| span(key) }

    # @return [String] the matched substring (like MatchData#to_s)
    def to_s = @matched

    alias == eql?
    alias to_h params

    # @!visibility private
    def inspect
      params_str = params.map { |k, v| " #{k}:#{v.inspect}" }.join
      "#<#{self.class.name}: #{@matched.inspect}#{params_str}>"
    end

    # @!visibility private
    def pretty_print(q)
      q.group(1, "#<#{self.class.name}:", ">") do
        q.breakable
        q.pp @matched
        params.each do |key, value|
          q.breakable
          q.text("#{key}:")
          q.pp value
        end
      end
    end

    private

    # Builds the name => span(s) mapping from the positional spans, using the
    # ordering and multiplicity of the regexp's named capture groups. Spans are
    # taken straight from the regexp match positions, never located by searching
    # the string for a decoded value.
    def derive_named_spans
      return EMPTY_HASH unless @regexp
      @regexp.named_captures.each_with_object({}) do |(name, positions), result|
        always_array = @pattern.respond_to?(:always_array?) && @pattern.always_array?(name)
        if positions.size < 2 && !always_array
          span = @spans[positions.first - 1]
          result[name] = span if span
        else
          present = positions.map { |position| @spans[position - 1] }.compact
          result[name] = present if always_array || !present.empty?
        end
      end
    end
  end

  # Locates a capture inside the original string passed to the matcher.
  #
  # Offsets are half-open byte offsets, relative to the original string passed to
  # {Mustermann::Pattern#match}, {Mustermann::Pattern#peek_match},
  # {Mustermann::Set#match}, and friends — even when the match resulted from
  # {Mustermann::Pattern#peek} or a {Mustermann::Concat} of several patterns.
  #
  # The {#raw} text is the exact byte slice of the original input, before any URI
  # decoding. A capture matching the empty string has a zero-length span
  # ({#begin_byte} == {#end_byte}), whereas an optional segment that did not
  # participate in the match is represented by +nil+ instead of a span.
  class Span
    include Comparable

    # @return [String] the original string the capture was taken from
    attr_reader :string

    # @return [Integer] byte offset of the first byte of the capture (inclusive)
    attr_reader :begin_byte

    # @return [Integer] byte offset just past the last byte of the capture (exclusive)
    attr_reader :end_byte

    # @param string [String] the original input string
    # @param begin_byte [Integer] inclusive byte offset
    # @param end_byte [Integer] exclusive byte offset
    def initialize(string, begin_byte, end_byte)
      @string     = string
      @begin_byte = begin_byte
      @end_byte   = end_byte
    end

    # @return [Range<Integer>] half-open byte range of the capture
    def byte_range = begin_byte...end_byte

    # @return [Integer] number of bytes captured
    def bytesize = end_byte - begin_byte

    # @return [String] the captured substring as it appeared in the original input,
    #   without URI decoding
    def raw
      string.byteslice(begin_byte, bytesize) || EMPTY_STRING
    end

    # @return [Boolean] whether the capture matched the empty string
    def empty? = bytesize.zero?

    # @return [Span] a copy of the span shifted by the given byte offset. Used when
    #   joining spans from sub-matches ({Mustermann::Concat}) back onto the
    #   original input.
    # @param bytes [Integer] byte offset to add to both ends
    # @param string [String] the original input to rebind the span to
    def shift(bytes, string: self.string)
      self.class.new(string, begin_byte + bytes, end_byte + bytes)
    end

    def <=>(other)
      return unless other.is_a?(self.class) && string.equal?(other.string)
      [begin_byte, end_byte] <=> [other.begin_byte, other.end_byte]
    end

    def ==(other)
      other.is_a?(self.class) && string == other.string &&
        begin_byte == other.begin_byte && end_byte == other.end_byte
    end

    alias eql? ==

    def hash = [string, begin_byte, end_byte].hash

    # @!visibility private
    def inspect
      "#<#{self.class.name}: #{begin_byte}...#{end_byte} #{raw.inspect}>"
    end

    # @!visibility private
    def pretty_print(q)
      q.group(1, "#<#{self.class.name}:", ">") do
        q.breakable
        q.text("#{begin_byte}...#{end_byte}")
        q.breakable
        q.pp raw
      end
    end
  end
end
