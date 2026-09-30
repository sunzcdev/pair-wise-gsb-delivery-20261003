# frozen_string_literal: true
require 'mustermann/ast/translator'
require 'mustermann/set/match'

module Mustermann
  class Set
    class Trie
      class Translator < AST::Translator
        translate(:node) { |trie, **o| trie[t.compile(node)] }
        translate(:separator) { |trie, **options| trie[payload] }

        translate(:root) do |trie, **options|
          leaves = t(payload, trie, **options)
          if leaves.is_a? Array
            leaves.each do |leaf|
              leaf.patterns ||= []
              leaf.patterns << t.pattern
            end
          else
            leaves.patterns ||= []
            leaves.patterns << t.pattern
          end
          leaves
        end

        translate(:char) do |trie, **options|
          return trie if payload.empty?
          trie[payload]
        end

        translate(:optional) do |trie, **options|
          [*t(payload, trie, **options), trie]
        end

        translate(Array) do |trie, **options|
          i = 0
          while i < size
            element = self[i]
            if element.is_a? :char or element.is_a? :separator
              trie = t(element, trie, **options)
              i += 1
            elsif element.is_a? :splat and self[i + 1]&.is_a? :separator
              # Compile splat+separator together so the splat is bounded by the separator,
              # then continue building the trie for the remaining elements.
              trie = trie[t.compile(self[i..i + 1])]
              i += 2
            elsif element.is_a? :splat or !self[i + 1]&.is_a? :separator
              return trie[t.compile(self[i..-1])]
            else
              trie = t(element, trie, **options)
              return trie.flat_map { |node| t(self[i + 1..-1], node, **options) } if trie.is_a? Array
              i += 1
            end
          end
          trie
        end

        attr_reader :pattern

        def initialize(pattern)
          @pattern  = pattern
          @compiler = pattern.compiler.new
          @options  = pattern.options
          super()
        end

        # \G anchors to a position passed to String#match, avoiding substring allocation.
        def compile(node, **options) = /\G#{@compiler.translate(node, **@options, **options)}/
      end

      attr_accessor :patterns, :set, :static, :dynamic

      def initialize(set, patterns = nil)
        @set             = set
        @patterns        = nil
        @dynamic         = nil
        @static          = nil
        @stride          = nil
        @fast_static     = nil
        @byte_lookup     = nil
        @dynamic_entries = nil
        patterns&.each { |pattern| add(pattern) }
      end

      def [](key)
        case key = Mustermann.dedup(key)
        when String
          @static      ||= {}
          @static[key] ||= Trie.new(@set)
        when Regexp
          @dynamic      ||= {}
          @dynamic[key] ||= Trie.new(@set)
        end
      end

      def match(string, all: false, peek: false, char_position: 0, byte_position: 0, params: {}, spans: [])
        optimize! if @stride.nil?
        # Positions are tracked in bytes; character offsets are derived only
        # where Ruby's regexp/string APIs require them.
        result = [] if all

        if @fast_static
          stride = @stride
          chunk = char_position <= string.size ? string[char_position, stride] : nil
          if node = chunk && @fast_static[chunk]
            byte_stride = chunk.bytesize
            if nested_result = node.match(string, all:, peek:, char_position: char_position + stride,
                                          byte_position: byte_position + byte_stride, params:, spans:)
              return nested_result unless all
              result.concat(nested_result)
            end
          end
        elsif @byte_lookup && byte_position < string.bytesize
          if node = @byte_lookup[string.getbyte(byte_position)]
            if nested_result = node.match(string, all:, peek:, char_position: char_position + 1,
                                          byte_position: byte_position + 1, params:, spans:)
              return nested_result unless all
              result.concat(nested_result)
            end
          end
        end

        if @dynamic_entries&.any?
          base_params = all ? params : nil
          base_spans  = all ? spans  : nil
          @dynamic_entries.each do |matcher, node, capture_names, fast_name|
            if fast_name
              # Fast path: unconstrained single-segment capture — no regex, no MatchData.
              remainder = string.byteslice(byte_position, string.bytesize - byte_position)
              slash     = remainder.each_byte.find_index(47)
              end_byte  = slash ? byte_position + slash : string.bytesize
              end_char  = slash ? char_position + remainder.byteslice(0, slash).length : string.size
              next if end_byte == byte_position
              edge_params = all ? base_params.dup : params
              edge_spans  = all ? base_spans.dup  : spans
              raw = string.byteslice(byte_position, end_byte - byte_position)
              existing = edge_params[fast_name]
              edge_params[fast_name] = existing ? (existing.is_a?(Array) ? existing << raw : [existing, raw]) : raw
              edge_spans << [fast_name, Mustermann::Span.new(string, byte_position, end_byte)]
              nested_result = node.match(string, all:, params: edge_params, spans: edge_spans, peek:,
                                         char_position: end_char, byte_position: end_byte)
              return nested_result unless all
              result.concat(nested_result)
              next
            end

            regexp_match = matcher.match(string, char_position)
            # Non-greedy patterns (e.g. splat .*?) can match 0 chars on non-empty input, making
            # no progress. Keep the zero-width match for empty trailing captures, but also
            # prepare an end-anchored alternative to fall back on (it consumes the remainder).
            zero_width = regexp_match && regexp_match.end(0) == char_position
            if zero_width
              @@anchored ||= {}
              anchored_matcher = @@anchored[matcher] ||= Mustermann.dedup(Regexp.new(matcher.source + '\z'))
              anchored_match = anchored_matcher.match(string, char_position)
            end
            next unless regexp_match

            attempts = zero_width ? [regexp_match, anchored_match].compact : [regexp_match]
            attempts.each do |attempt|
              independent  = all || attempts.size > 1
              edge_params  = all ? base_params.dup : (independent ? params.dup : params)
              edge_spans   = all ? base_spans.dup  : (independent ? spans.dup  : spans)
              capture_names.each do |name|
                positions = attempt.regexp.named_captures.fetch(name)
                value     = nil
                positions.each do |group_index|
                  raw = attempt[group_index]
                  next unless raw
                  start_byte, stop_byte = attempt.byteoffset(group_index)
                  edge_spans << [name, Mustermann::Span.new(string, start_byte, stop_byte)]
                  value = value ? (value.is_a?(Array) ? value << raw : [value, raw]) : raw
                end
                edge_spans << [name, nil] if value.nil?
                next if value.nil?
                existing = edge_params[name]
                edge_params[name] = existing ? (existing.is_a?(Array) ? existing << value : [existing, value]) : value
              end

              nested_result = node.match(string, all:, params: edge_params, spans: edge_spans, peek:,
                                         char_position: attempt.end(0),
                                         byte_position: attempt.byteoffset(0).last)
              found = nested_result && (all ? nested_result.any? : true)
              if found
                return nested_result unless all
                result.concat(nested_result)
              end
            end
          end
        end

        if char_position >= string.size
          matches = build_matches(string, params, spans, all:)
          return matches unless all
          result.concat(matches)
          return result
        end

        if peek
          matches = build_matches(string, params, spans, all:, matched_length: char_position,
                                  matched_bytes: byte_position, post_match: string[char_position..],
                                  pre_match: EMPTY_STRING)
          return matches unless all
          result.concat(matches)
        end

        result
      end

      NIL_VALUES = [nil].freeze

      def build_matches(string, params, spans, all: false, matched_length: string.size,
                        matched_bytes: string.bytesize, post_match: EMPTY_STRING, pre_match: EMPTY_STRING)
        result = [] if all
        matched = string.byteslice(0, matched_bytes) || string[0, matched_length]

        @patterns&.each do |pattern|
          next if pattern.except_regexp&.match?(matched)

          pattern_params = build_pattern_params(pattern, params)
          pattern_spans  = align_spans(pattern, spans)
          pattern_captures = pattern_spans.map { |span| span && span.raw }

          values = @set.values_for_pattern(pattern) || NIL_VALUES
          values.each do |value|
            match = Set::Match.new(pattern, string, matched:, params: pattern_params, value:,
                                   post_match:, pre_match:, spans: pattern_spans,
                                   captures: pattern_captures)
            return match unless all
            result << match
          end
        end

        result
      end

      # Aligns the [name, span] entries collected along the traversal with the
      # pattern's capture groups (in group order). Groups without a collected
      # span are optional segments that did not participate and stay nil.
      def align_spans(pattern, collected)
        names = pattern.regexp.named_captures
          .flat_map { |name, positions| positions.map { |position| [position, name] } }
          .sort_by(&:first)
          .map(&:last)
        entries = collected.dup
        names.map do |name|
          index = entries.index { |entry_name, _| entry_name == name }
          index ? entries.delete_at(index).last : nil
        end
      end

      def build_pattern_params(pattern, params)
        return params if pattern.identity_params?(params)

        result = {}
        params.each do |key, raw|
          if raw.is_a?(Array)
            val = raw.flat_map { |v| pattern.map_param(key, v) }
            val = val.first if val.size < 2 && !pattern.always_array?(key)
          else
            val = pattern.map_param(key, raw)
            val = [val] if pattern.always_array?(key)
          end
          result[key] = val
        end
        result
      end

      def add(pattern)
        @stride          = nil
        @fast_static     = nil
        @byte_lookup     = nil
        @dynamic_entries = nil
        Translator.new(pattern).translate(pattern.to_ast, self)
      end

      # Compacts the trie by replacing sequential single-char static lookups with a
      # single stride-length hash lookup. The stride is the minimum number of static
      # steps all paths from this node share before hitting a dynamic edge or branch.
      def optimize!
        depth = min_static_depth
        if depth > 1
          @fast_static = build_stride_hash(depth)
          @byte_lookup = nil
          @stride      = depth
          @fast_static.each_value(&:optimize!)
        elsif !@static&.any?
          @fast_static = nil
          @byte_lookup = nil
          @stride      = 1
          # no children to recurse into
        else
          @fast_static = nil
          @byte_lookup = Array.new(256)
          @stride      = 1
          @static&.each { |k, v| @byte_lookup[k.getbyte(0)] = v }
          @static&.each_value(&:optimize!)
        end

        @dynamic&.each_value(&:optimize!)
        @dynamic_entries = @dynamic&.map do |matcher, node|
          names = matcher.names.each(&:freeze)
          # Detect unconstrained single-segment captures: can use fast string.index instead of regex.
          # Two conditions: (1) the edge is a bare capture (source starts with \G(?<name>), no leading
          # static chars) and (2) the capture's character class excludes '/' (PATH_INFO never has '?' or '#').
          fast = if names.size == 1
            name = names.first
            matcher.source.start_with?("\\G(?<#{name}>") && !matcher.match?('/') ? name : nil
          end
          [matcher, node, names, fast]
        end

        @fast_static     = Mustermann.dedup(@fast_static)
        @dynamic_entries = Mustermann.dedup(@dynamic_entries)
        @byte_lookup     = Mustermann.dedup(@byte_lookup)
      end

      protected

      # Returns the minimum number of guaranteed static steps from this node across
      # all possible paths, before encountering a dynamic edge, a terminal pattern,
      # or an empty node. Branching is allowed; only the minimum depth matters.
      def min_static_depth
        return 0 if @dynamic&.any?
        return 0 if @patterns&.any?
        return 0 if @static&.empty?
        1 + @static.values.map { |node| node.min_static_depth }.min
      end

      private

      # Builds a hash whose keys are +stride+-character strings and whose values are
      # the trie nodes reached after consuming exactly those characters.
      def build_stride_hash(stride)
        stride.times.reduce({ EMPTY_STRING => self }) do |frontier, _|
          frontier.each_with_object({}) do |(prefix, node), nxt|
            node.static.each { |char, child| nxt[prefix + char] = child }
          end
        end
      end

    end

    private_constant :Trie
  end
end
