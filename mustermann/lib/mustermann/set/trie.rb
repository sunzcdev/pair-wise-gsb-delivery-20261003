# frozen_string_literal: true
require 'mustermann/ast/translator'
require 'mustermann/capture'
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

      def match(string, all: false, peek: false, position: 0, char_position: nil, params: {}, occurrences: [])
        optimize! if @stride.nil?
        # +position+ is a byte offset; +char_position+ is the matching character offset.
        # Both are threaded through recursion since Regexp#match takes character offsets.
        char_position ||= position
        return build_matches(string, params, occurrences, all:) if position >= string.bytesize
        result = [] if all

        if @fast_static
          stride = @stride
          segment = string[char_position, stride]
          if node = @fast_static[segment]
            byte_step = segment.bytesize
            char_step = segment.length
            if nested_result = node.match(string, all:, peek:, position: position + byte_step,
                                          char_position: char_position + char_step,
                                          params:, occurrences:)
              return nested_result unless all
              result.concat(nested_result)
            end
          end
        elsif @byte_lookup
          if node = @byte_lookup[string.getbyte(position)]
            step = string[char_position].bytesize
            if nested_result = node.match(string, all:, peek:, position: position + 1,
                                          char_position: char_position + step,
                                          params:, occurrences:)
              return nested_result unless all
              result.concat(nested_result)
            end
          end
        end

        if @dynamic_entries&.any?
          @dynamic_entries.each do |matcher, node, capture_names, fast_name|
            if fast_name
              # Fast path: unconstrained single-segment capture — no regex, no MatchData.
              stop_char = string.index('/', char_position)
              if stop_char
                next_stop_byte = position + string[char_position...stop_char].bytesize
                next_stop_char = stop_char
              else
                next_stop_byte = string.bytesize
                next_stop_char = string.length
              end
              next if next_stop_byte == position
              raw     = string.byteslice(position, next_stop_byte - position)
              edge_params   = all ? params.dup : params
              edge_params[fast_name] = raw
              edge_occurrences = occurrences + [Capture.new(fast_name, position, next_stop_byte, string)]
              nested_result = node.match(string, all:, params: edge_params, peek:,
                                         position: next_stop_byte,
                                         char_position: next_stop_char,
                                         occurrences: edge_occurrences)
              return nested_result unless all
              result.concat(nested_result)
              next
            end

            edge_match     = matcher.match(string, char_position)
            edge_byte_end  = edge_match&.byteoffset(0)&.last
            edge_char_end  = edge_match&.end(0)
            offset_by      = 0
            # Non-greedy patterns (e.g. splat .*?) can match 0 chars on non-empty input, making
            # no progress. Retry with an end-of-string anchor so they consume the full remainder.
            if edge_match && edge_byte_end == position && position < string.bytesize
              @@anchored ||= {}
              anchored_source = matcher.source.sub(/\A\\G/, '\A') + '\z'
              anchored_matcher = @@anchored[matcher] ||= Mustermann.dedup(Regexp.new(anchored_source))
              remainder = string.byteslice(position..)
              retry_match = anchored_matcher.match(remainder)
              if retry_match
                edge_match    = retry_match
                edge_byte_end = position + retry_match.byteoffset(0).last
                edge_char_end = string.byteslice(0, edge_byte_end).length
                offset_by     = position
              else
                edge_match = nil
              end
            end
            next unless edge_match

            edge_params = all ? params.dup : params
            edge_captures = []
            edge_match.regexp.names.each do |name|
              edge_match.regexp.named_captures.fetch(name).each do |group_position|
                start_pos, stop_pos = edge_match.byteoffset(group_position)
                start_pos += offset_by if start_pos
                stop_pos  += offset_by if stop_pos
                edge_captures << Capture.new(name, start_pos, stop_pos, string)
                next if start_pos.nil?
                raw = string.byteslice(start_pos, stop_pos - start_pos)
                existing = edge_params[name]
                edge_params[name] = existing ? (existing.is_a?(Array) ? existing << raw : [existing, raw]) : raw
              end
            end
            edge_occurrences = occurrences + edge_captures

            nested_result = node.match(string, all:, params: edge_params, peek:,
                                       position: edge_byte_end,
                                       char_position: edge_char_end,
                                       occurrences: edge_occurrences)
            return nested_result unless all
            result.concat(nested_result)
          end
        end

        if peek
          matches = build_matches(string, params, occurrences, all:, matched_byte_length: position,
                                  post_match: string.byteslice(position..), pre_match: EMPTY_STRING)
          return matches unless all
          result.concat(matches)
        end

        result
      end

      NIL_VALUES = [nil].freeze

      def build_matches(string, params, occurrences, all: false, matched_byte_length: string.bytesize, post_match: EMPTY_STRING, pre_match: EMPTY_STRING)
        result = [] if all
        matched = string.byteslice(0, matched_byte_length)

        @patterns&.each do |pattern|
          next if pattern.except_regexp&.match?(matched)

          pattern_params, pattern_occurrences = build_pattern_params(pattern, occurrences)

          values = @set.values_for_pattern(pattern) || NIL_VALUES
          values.each do |value|
            match = Set::Match.new(pattern, string, matched:, params: pattern_params, value:, post_match:,
                                   pre_match:, captures: pattern_occurrences.map(&:bytes),
                                   named_captures: build_named_captures(pattern_occurrences),
                                   capture_occurrences: pattern_occurrences)
            return match unless all
            result << match
          end
        end

        result
      end

      def build_pattern_params(pattern, occurrences)
        result = {}
        occurrences.group_by(&:name).each do |key, list|
          if list.size > 1
            values = list.map { |capture| pattern.map_param(key, capture.bytes) }
            result[key] = pattern.always_array?(key) || values.size > 1 ? values : values.first
          elsif pattern.always_array?(key)
            result[key] = [pattern.map_param(key, list.first.bytes)]
          else
            result[key] = pattern.map_param(key, list.first.bytes)
          end
        end
        [result, occurrences]
      end

      def build_named_captures(occurrences)
        occurrences.each_with_object({}) do |capture, hash|
          if hash.key?(capture.name)
            existing = hash[capture.name]
            hash[capture.name] = existing.is_a?(Array) ? existing << capture.bytes : [existing, capture.bytes]
          else
            hash[capture.name] = capture.bytes
          end
        end
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
