# frozen_string_literal: true
require 'mustermann/capture'

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

    # @return [Array<Mustermann::Capture>] information about every capture
    #   group in left-to-right order. Includes entries for captures that did not
    #   participate in the match (e.g. absent optional segments) and entries for
    #   repeated group names. Offsets are byte offsets relative to {#string}.
    attr_reader :capture_occurrences

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
    # @option options [Array<Mustermann::Capture>] :capture_occurrences per-capture byte spans in the original string
    def initialize(pattern_or_match, string_or_match = nil, matched: nil, params: nil, post_match: nil, pre_match: nil, captures: nil, named_captures: nil, regexp: nil, capture_occurrences: nil)
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
      @matched        = matched        || match&.to_s           || @string

      @capture_occurrences = capture_occurrences ||
        (match.respond_to?(:capture_occurrences) ? match.capture_occurrences : nil) ||
        (match.is_a?(MatchData) ? build_occurrences(match) : nil) ||
        EMPTY_ARRAY

      unless @regexp = regexp
        @regexp = match.regexp if match.respond_to?(:regexp)
        @regexp ||= pattern.respond_to?(:regexp) ? pattern.regexp : nil
      end
    end

    # @return [Array<String>] the names of the named captures
    def names = named_captures.keys

    # The raw, URI-encoded capture values in left-to-right order.
    # @return [Array<String, nil>] same order as {#capture_occurrences}
    def raw_captures
      capture_occurrences.map(&:bytes)
    end

    # Raw capture values grouped by name, without URI decoding and without type
    # conversion. Names repeating in the pattern yield arrays, just like in
    # {#params}; splat captures are always arrays as well. Absent optional
    # segments are nil.
    # @return [Hash{String => String, Array<String>, nil}]
    def raw_params
      @raw_params ||= capture_occurrences.each_with_object({}) do |capture, hash|
        value = capture.bytes
        if hash.key?(capture.name)
          existing = hash[capture.name]
          hash[capture.name] = existing.is_a?(Array) ? existing << value : [existing, value]
        elsif %w[splat captures].include?(capture.name)
          hash[capture.name] = [value]
        else
          hash[capture.name] = value
        end
      end
    end

    # Accesses the raw capture information for a given capture name.
    #
    # @overload capture(name)
    #   @param name [String, Symbol] capture name
    #   @return [Mustermann::Capture, nil] the single occurrence, or nil if the
    #     name did not occur in the pattern
    #
    # @overload capture(index)
    #   @param index [Integer] zero-based occurrence index
    #   @return [Mustermann::Capture, nil]
    def capture(name_or_index)
      case name_or_index
      when String, Symbol
        list = capture_occurrences.select { |c| c.name == name_or_index.to_s }
        list.size > 1 ? list : list.first
      when Integer
        capture_occurrences[name_or_index]
      else raise ArgumentError, "name must be a String, Symbol, or Integer, not #{name_or_index.class}"
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

    # Builds per-capture spans directly from MatchData offsets.
    # Offsets are relative to the string passed to the regular expression;
    # callers matching substrings (e.g. Concat) remap them afterwards.
    def build_occurrences(match_data)
      regexp = match_data.regexp
      groups = Array.new(regexp.named_captures.values.sum { |positions| positions.size })
      regexp.named_captures.each do |name, positions|
        positions.each { |position| groups[position - 1] = name }
      end
      groups.map.with_index(1) do |name, position|
        start, stop = match_data.byteoffset(position)
        Capture.new(name, start, stop, match_data.string)
      end
    end
  end
end
