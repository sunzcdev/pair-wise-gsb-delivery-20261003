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
        elsif rule.respond_to?(:transliterate)
          rule
        else
          raise I18n::ArgumentError, "Transliteration rule must be a proc, a hash, or respond to #transliterate."
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

      # A transliterator driven by an explicit set of rules whose keys may
      # be one or more characters long and whose values may be any string,
      # including an empty string (i.e. a deletion rule).
      #
      # Rules are applied with a longest match strategy: at any position in
      # the input the longest matching rule wins, so overlapping rules such
      # as "sch" and "ch" behave deterministically. If two distinct rule
      # keys compile to the same key (e.g. through Unicode normalization)
      # the +on_conflict+ policy decides which rule is used:
      #
      # * +:error+ - raise an I18n::ArgumentError (default)
      # * +:first+ - use the rule whose source key is lexicographically first
      # * +:last+  - use the rule whose source key is lexicographically last
      #
      # Conflict resolution only depends on the rule keys themselves, never
      # on the insertion order of the rules hash.
      #
      # Non-ASCII characters not matched by any rule are handled according
      # to the +on_unknown+ policy:
      #
      # * +:replace+ - replaced with +replacement+ (default)
      # * +:keep+    - passed through unchanged
      # * +:error+   - raise an I18n::ArgumentError
      #
      # ASCII characters (0x00-0x7F) without a matching rule are always
      # passed through unchanged, mirroring HashTransliterator.
      #
      # If +normalize+ is set to one of +:nfc+, +:nfd+, +:nfkc+ or +:nfkd+,
      # both the rule keys and the input string are normalized before
      # matching, so composed and decomposed representations of the same
      # text transliterate identically. Mappings returned by
      # #transliterate_with_mapping always refer to character offsets of
      # the original, un-normalized string passed by the caller.
      class RuleSet
        # A piece of transliterated output together with the ranges it
        # occupies in the output and in the original input string, as
        # end-exclusive character offset ranges.
        Fragment = Struct.new(:text, :output_range, :source_range)

        CONFLICT_POLICIES = [:error, :first, :last].freeze
        UNKNOWN_POLICIES = [:replace, :keep, :error].freeze
        NORMALIZATION_FORMS = [:nfc, :nfd, :nfkc, :nfkd].freeze

        attr_reader :replacement, :on_unknown, :on_conflict, :normalize

        def initialize(rules = {}, replacement: DEFAULT_REPLACEMENT_CHAR, on_unknown: :replace, on_conflict: :error, normalize: nil)
          unless UNKNOWN_POLICIES.include?(on_unknown)
            raise I18n::ArgumentError, "Unknown on_unknown policy: #{on_unknown.inspect}. Must be one of #{UNKNOWN_POLICIES.inspect}."
          end
          unless CONFLICT_POLICIES.include?(on_conflict)
            raise I18n::ArgumentError, "Unknown on_conflict policy: #{on_conflict.inspect}. Must be one of #{CONFLICT_POLICIES.inspect}."
          end
          if normalize && !NORMALIZATION_FORMS.include?(normalize)
            raise I18n::ArgumentError, "Unknown normalization form: #{normalize.inspect}. Must be one of #{NORMALIZATION_FORMS.inspect}."
          end

          @replacement = replacement
          @on_unknown = on_unknown
          @on_conflict = on_conflict
          @normalize = normalize
          compile(rules)
        end

        def transliterate(string, replacement = nil)
          transliterate_with_mapping(string, replacement).first
        end

        # Returns [output, fragments] where fragments is an array of
        # Fragment mapping each emitted piece of output back to the range
        # of the caller's original string it was produced from. Deleted
        # input produces no fragments.
        def transliterate_with_mapping(string, replacement = nil)
          source, source_index = prepare(string)
          output = +""
          fragments = []
          position = 0

          while position < source.length
            rule = match_at(source, position)
            if rule
              key, value = rule
              emit(output, fragments, value, position, position + key.length, source_index) unless value.empty?
              position += key.length
            else
              char = source[position]
              if char.ord < 128
                emit(output, fragments, char, position, position + 1, source_index)
              else
                handle_unknown(output, fragments, char, position, source_index, replacement)
              end
              position += 1
            end
          end

          [output, fragments]
        end

        private

        def compile(rules)
          candidates = Hash.new { |hash, key| hash[key] = [] }
          rules.each do |key, value|
            source_key = key.to_s
            if source_key.empty?
              raise I18n::ArgumentError, "Transliteration rule keys must not be empty."
            end
            compiled_key = @normalize ? source_key.unicode_normalize(@normalize) : source_key
            candidates[compiled_key] << [source_key, value.to_s]
          end

          @rules = {}
          candidates.each do |compiled_key, entries|
            entries = [resolve_conflict(compiled_key, entries)] if entries.map(&:last).uniq.length > 1
            @rules[compiled_key] = entries.first.last
          end

          @by_first_char = {}
          @rules.each_key do |key|
            (@by_first_char[key[0]] ||= []) << key
          end
          # Longest match first; secondary sort by key keeps the order
          # deterministic regardless of hash insertion order.
          @by_first_char.each_value do |keys|
            keys.sort_by! { |key| [-key.length, key] }
          end
        end

        def resolve_conflict(compiled_key, entries)
          case @on_conflict
          when :error
            keys = entries.map(&:first).sort
            raise I18n::ArgumentError, "Conflicting transliteration rules for #{compiled_key.inspect}: #{keys.map(&:inspect).join(', ')}."
          when :first
            entries.min_by(&:first)
          when :last
            entries.max_by(&:first)
          end
        end

        def match_at(source, position)
          keys = @by_first_char[source[position]]
          return unless keys
          keys.each do |key|
            return [key, @rules[key]] if source[position, key.length] == key
          end
          nil
        end

        def handle_unknown(output, fragments, char, position, source_index, replacement)
          case @on_unknown
          when :keep
            emit(output, fragments, char, position, position + 1, source_index)
          when :error
            raise I18n::ArgumentError, "No transliteration rule for #{char.inspect}."
          else # :replace
            replacement = @replacement if replacement.nil?
            if replacement == :none
              emit(output, fragments, char, position, position + 1, source_index)
            else
              emit(output, fragments, replacement.to_s, position, position + 1, source_index)
            end
          end
        end

        # Normalizes the input if required, returning the string to match
        # against plus an index mapping each of its characters back to a
        # character offset in the caller's original string (nil when no
        # normalization is applied).
        def prepare(string)
          if @normalize
            normalized = +""
            source_index = []
            string.each_char.with_index do |char, char_index|
              part = char.unicode_normalize(@normalize)
              normalized << part
              part.length.times { source_index << char_index }
            end
            [normalized, source_index]
          else
            [string, nil]
          end
        end

        def emit(output, fragments, text, from, to, source_index)
          output_range = output.length...(output.length + text.length)
          if source_index
            source_range = source_index[from]...(source_index[to - 1] + 1)
          else
            source_range = from...to
          end
          output << text
          fragments << Fragment.new(text, output_range, source_range)
        end
      end
    end
  end
end
