# encoding: utf-8
# frozen_string_literal: true

module I18n
  module Backend
    module Transliterator
      DEFAULT_REPLACEMENT_CHAR = "?"

      # Given a locale and a UTF-8 string, return the locale's ASCII
      # approximation for the string.
      def transliterate(locale, string, replacement = nil)
        @transliterators ||= {}
        @transliterators[locale] ||= Transliterator.get I18n.t(:'i18n.transliterate.rule',
          :locale => locale, :resolve => false, :default => {})
        @transliterators[locale].transliterate(string, replacement)
      end

      # Get a transliterator instance.
      def self.get(rule = nil)
        if !rule || rule.kind_of?(Hash)
          HashTransliterator.new(rule)
        elsif rule.kind_of? Proc
          ProcTransliterator.new(rule)
        elsif rule.kind_of? RuleSet
          rule
        else
          raise I18n::ArgumentError, "Transliteration rule must be a proc, a hash, or a RuleSet."
        end
      end

      # A transliterator which accepts a Proc as its transliteration rule.
      class ProcTransliterator
        def initialize(rule)
          @rule = rule
        end

        def transliterate(string, replacement = nil)
          @rule.call(string)
        end
      end

      # A transliterator which accepts a Hash of characters as its translation
      # rule.
      class HashTransliterator
        DEFAULT_APPROXIMATIONS = {
          "À"=>"A", "Á"=>"A", "Â"=>"A", "Ã"=>"A", "Ä"=>"A", "Å"=>"A", "Æ"=>"AE",
          "Ç"=>"C", "È"=>"E", "É"=>"E", "Ê"=>"E", "Ë"=>"E", "Ì"=>"I", "Í"=>"I",
          "Î"=>"I", "Ï"=>"I", "Ð"=>"D", "Ñ"=>"N", "Ò"=>"O", "Ó"=>"O", "Ô"=>"O",
          "Õ"=>"O", "Ö"=>"O", "×"=>"x", "Ø"=>"O", "Ù"=>"U", "Ú"=>"U", "Û"=>"U",
          "Ü"=>"U", "Ý"=>"Y", "Þ"=>"Th", "ß"=>"ss", "ẞ"=>"SS", "à"=>"a",
          "á"=>"a", "â"=>"a", "ã"=>"a", "ä"=>"a", "å"=>"a", "æ"=>"ae", "ç"=>"c",
          "è"=>"e", "é"=>"e", "ê"=>"e", "ë"=>"e", "ì"=>"i", "í"=>"i", "î"=>"i",
          "ï"=>"i", "ð"=>"d", "ñ"=>"n", "ò"=>"o", "ó"=>"o", "ô"=>"o", "õ"=>"o",
          "ö"=>"o", "ø"=>"o", "ù"=>"u", "ú"=>"u", "û"=>"u", "ü"=>"u", "ý"=>"y",
          "þ"=>"th", "ÿ"=>"y", "Ā"=>"A", "ā"=>"a", "Ă"=>"A", "ă"=>"a", "Ą"=>"A",
          "ą"=>"a", "Ć"=>"C", "ć"=>"c", "Ĉ"=>"C", "ĉ"=>"c", "Ċ"=>"C", "ċ"=>"c",
          "Č"=>"C", "č"=>"c", "Ď"=>"D", "ď"=>"d", "Đ"=>"D", "đ"=>"d", "Ē"=>"E",
          "ē"=>"e", "Ĕ"=>"E", "ĕ"=>"e", "Ė"=>"E", "ė"=>"e", "Ę"=>"E", "ę"=>"e",
          "Ě"=>"E", "ě"=>"e", "Ĝ"=>"G", "ĝ"=>"g", "Ğ"=>"G", "ğ"=>"g", "Ġ"=>"G",
          "ġ"=>"g", "Ģ"=>"G", "ģ"=>"g", "Ĥ"=>"H", "ĥ"=>"h", "Ħ"=>"H", "ħ"=>"h",
          "Ĩ"=>"I", "ĩ"=>"i", "Ī"=>"I", "ī"=>"i", "Ĭ"=>"I", "ĭ"=>"i", "Į"=>"I",
          "į"=>"i", "İ"=>"I", "ı"=>"i", "Ĳ"=>"IJ", "ĳ"=>"ij", "Ĵ"=>"J", "ĵ"=>"j",
          "Ķ"=>"K", "ķ"=>"k", "ĸ"=>"k", "Ĺ"=>"L", "ĺ"=>"l", "Ļ"=>"L", "ļ"=>"l",
          "Ľ"=>"L", "ľ"=>"l", "Ŀ"=>"L", "ŀ"=>"l", "Ł"=>"L", "ł"=>"l", "Ń"=>"N",
          "ń"=>"n", "Ņ"=>"N", "ņ"=>"n", "Ň"=>"N", "ň"=>"n", "ŉ"=>"'n", "Ŋ"=>"NG",
          "ŋ"=>"ng", "Ō"=>"O", "ō"=>"o", "Ŏ"=>"O", "ŏ"=>"o", "Ő"=>"O", "ő"=>"o",
          "Œ"=>"OE", "œ"=>"oe", "Ŕ"=>"R", "ŕ"=>"r", "Ŗ"=>"R", "ŗ"=>"r", "Ř"=>"R",
          "ř"=>"r", "Ś"=>"S", "ś"=>"s", "Ŝ"=>"S", "ŝ"=>"s", "Ş"=>"S", "ş"=>"s",
          "Š"=>"S", "š"=>"s", "Ţ"=>"T", "ţ"=>"t", "Ť"=>"T", "ť"=>"t", "Ŧ"=>"T",
          "ŧ"=>"t", "Ũ"=>"U", "ũ"=>"u", "Ū"=>"U", "ū"=>"u", "Ŭ"=>"U", "ŭ"=>"u",
          "Ů"=>"U", "ů"=>"u", "Ű"=>"U", "ű"=>"u", "Ų"=>"U", "ų"=>"u", "Ŵ"=>"W",
          "ŵ"=>"w", "Ŷ"=>"Y", "ŷ"=>"y", "Ÿ"=>"Y", "Ź"=>"Z", "ź"=>"z", "Ż"=>"Z",
          "ż"=>"z", "Ž"=>"Z", "ž"=>"z", "Ǫ"=>"O", "ǫ"=>"o", "Ǭ"=>"O",
          "ǭ"=>"o"
        }.freeze

        def initialize(rule = nil)
          @rule = rule
          add_default_approximations
          add rule if rule
        end

        def transliterate(string, replacement = nil)
          replacement ||= DEFAULT_REPLACEMENT_CHAR
          string.gsub(/[^\x00-\x7f]/u) do |char|
            approximations[char] || (replacement == :none ? char : replacement)
          end
        end

        private

        def approximations
          @approximations ||= {}
        end

        def add_default_approximations
          DEFAULT_APPROXIMATIONS.each do |key, value|
            approximations[key] = value
          end
        end

        # Add transliteration rules to the approximations hash.
        def add(hash)
          hash.each do |key, value|
            approximations[key.to_s] = value.to_s
          end
        end
      end

      # A transliterator driven by an explicit set of rules mapping character
      # sequences (of one or more characters) to replacement strings.
      #
      # Rules are applied with a longest-match strategy: at each position the
      # longest rule matching the input wins. Same-length rules can never
      # collide on literal keys, and duplicate keys supplied as an Array of
      # pairs are resolved through the +on_conflict+ strategy, so the result
      # never depends on the insertion order of the rule hash.
      #
      #   rules = I18n::Backend::Transliterator::RuleSet.new(
      #     { "sch" => "sh", "ü" => "ue" },
      #     :unknown => :replace,
      #     :normalize => :nfc
      #   )
      #   rules.transliterate("ü") # => "ue"
      #
      # +transliterate_with_map+ additionally returns the mapping from each
      # output fragment back to the character range of the original input
      # string that produced it. When +normalize+ is used the mapping still
      # refers to the string exactly as passed by the caller.
      class RuleSet
        # A fragment of transliterated output. +input_range+ and
        # +output_range+ are character offset ranges (exclusive end) into the
        # original input string and the transliterated output string.
        Segment = Struct.new(:output, :input_range, :output_range)

        UNKNOWN_STRATEGIES = %i[replace keep raise].freeze
        CONFLICT_STRATEGIES = %i[lexicographic first last raise].freeze
        NORMALIZATION_FORMS = %i[nfc nfd nfkc nfkd].freeze

        # Characters that may canonically combine or reorder with the
        # preceding character during Unicode normalization: combining marks,
        # plus Hangul V/T jamo which algorithmically compose with starters.
        SEGMENT_CONTINUATION = /[\p{M}\u1161-\u1175\u11A8-\u11C2]/.freeze

        attr_reader :rules, :unknown, :normalize, :replacement

        # +rules+ is a Hash or an Array of [key, replacement] pairs. Keys must
        # be non-empty; replacements may be empty to delete the matched
        # sequence.
        #
        # Options:
        # [:unknown]
        #   What to do with characters no rule matches: +:replace+ (emit the
        #   replacement string, the default), +:keep+ (pass the character
        #   through) or +:raise+ (raise I18n::ArgumentError).
        # [:on_conflict]
        #   How to resolve duplicate keys given as an Array of pairs:
        #   +:lexicographic+ (the default; pick the smallest replacement so
        #   the result is independent of insertion order), +:first+, +:last+
        #   or +:raise+.
        # [:normalize]
        #   Normalize the input with String#unicode_normalize (+:nfc+, +:nfd+,
        #   +:nfkc+ or +:nfkd+) before matching, e.g. to make decomposed
        #   combining marks match composed rule keys. Mappings returned by
        #   +transliterate_with_map+ still point at the original string.
        # [:replacement]
        #   Default replacement used for unmatched characters when the
        #   +unknown+ strategy is +:replace+. Defaults to "?".
        def initialize(rules = {}, unknown: :replace, on_conflict: :lexicographic, normalize: nil, replacement: DEFAULT_REPLACEMENT_CHAR)
          unless UNKNOWN_STRATEGIES.include?(unknown)
            raise I18n::ArgumentError, "Unknown strategy must be one of #{UNKNOWN_STRATEGIES.inspect}."
          end
          unless CONFLICT_STRATEGIES.include?(on_conflict)
            raise I18n::ArgumentError, "Conflict strategy must be one of #{CONFLICT_STRATEGIES.inspect}."
          end
          if normalize && !NORMALIZATION_FORMS.include?(normalize)
            raise I18n::ArgumentError, "Normalization form must be one of #{NORMALIZATION_FORMS.inspect}."
          end

          @unknown = unknown
          @normalize = normalize
          @replacement = replacement
          @rules = compile_rules(rules, on_conflict).freeze
          @candidates = compile_candidates(@rules)
        end

        def transliterate(string, replacement = nil)
          transliterate_with_map(string, replacement).first
        end

        # Transliterate +string+ and return a pair of the output string and an
        # array of Segment structs mapping each output fragment back to the
        # character range of the original input string that produced it.
        def transliterate_with_map(string, replacement = nil)
          replacement ||= @replacement
          source, index_of = prepare(string)
          output = +""
          segments = []
          pos = 0
          while pos < source.length
            if (match = match_at(source, pos))
              key, value = match
              emit(output, segments, value, index_of.call(pos, key.length))
              pos += key.length
            else
              char = source[pos]
              emit(output, segments, unknown_fragment(char, replacement), index_of.call(pos, 1))
              pos += 1
            end
          end
          [output, segments]
        end

        private

        def compile_rules(rules, on_conflict)
          pairs = rules.respond_to?(:each_pair) ? rules.each_pair.to_a : rules.to_a
          pairs.each_with_object({}) do |(key, value), table|
            key = key.to_s
            value = value.to_s
            if key.empty?
              raise I18n::ArgumentError, "Transliteration rule keys must not be empty."
            end
            if table.key?(key) && table[key] != value
              case on_conflict
              when :raise
                raise I18n::ArgumentError, "Conflicting transliteration rules for #{key.inspect}."
              when :last
                table[key] = value
              when :lexicographic
                table[key] = value if value < table[key]
              end
            else
              table[key] = value
            end
          end
        end

        # Group rules by their first character, longest first. Sorting by key
        # as a tie-breaker keeps the structure deterministic regardless of
        # the insertion order of the source hash.
        def compile_candidates(rules)
          grouped = Hash.new { |hash, char| hash[char] = [] }
          rules.each { |key, value| grouped[key[0]] << [key, value] }
          grouped.each_with_object({}) do |(char, list), candidates|
            list.sort_by! { |key, _| [-key.length, key] }
            candidates[char] = list.freeze
          end.freeze
        end

        def match_at(source, pos)
          return nil unless (list = @candidates[source[pos]])
          list.each do |key, value|
            return [key, value] if source[pos, key.length] == key
          end
          nil
        end

        def unknown_fragment(char, replacement)
          case @unknown
          when :keep
            char
          when :raise
            raise I18n::ArgumentError, "No transliteration rule for #{char.inspect}."
          else
            replacement == :none ? char : replacement.to_s
          end
        end

        def emit(output, segments, fragment, input_range)
          start = output.length
          output << fragment
          segments << Segment.new(fragment, input_range, start...output.length)
        end

        # Returns the string to match against and a callable mapping a
        # (start, length) position in that string back to a character range
        # in the caller's original string.
        def prepare(string)
          if @normalize
            index = NormalizationIndex.new(string, @normalize)
            [index.normalized, ->(start, length) { index.input_range(start, length) }]
          else
            [string, ->(start, length) { start...(start + length) }]
          end
        end

        # Maps character offsets of a normalized string back to character
        # offsets of the original string. The original string is split into
        # independent normalization segments (a starter followed by any
        # combining marks or Hangul V/T jamo), each normalized on its own, so
        # every normalized character can be attributed to the original
        # segment it came from.
        class NormalizationIndex
          attr_reader :normalized

          def initialize(string, form)
            @segments = [] # [input_start, input_end, normalized_start]
            @normalized = +""
            chars = string.each_char.to_a
            pos = 0
            while pos < chars.length
              stop = pos + 1
              stop += 1 while stop < chars.length && chars[stop].match?(SEGMENT_CONTINUATION)
              @segments << [pos, stop, @normalized.length]
              @normalized << chars[pos...stop].join.unicode_normalize(form)
              pos = stop
            end
            @normalized.freeze
          end

          def input_range(normalized_start, length)
            first = segment_at(normalized_start)
            last = segment_at(normalized_start + length - 1)
            @segments[first][0]...@segments[last][1]
          end

          private

          # Index of the segment containing the given normalized offset.
          def segment_at(offset)
            idx = @segments.bsearch_index { |segment| segment[2] > offset }
            idx ? idx - 1 : @segments.length - 1
          end
        end
      end
    end
  end
end
