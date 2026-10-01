# frozen_string_literal: true

#--
# Copyright (C) Bob Aman
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#++


require "addressable/version"
require "addressable/uri"

module Addressable
  ##
  # This is an implementation of a URI template based on
  # RFC 6570 (http://tools.ietf.org/html/rfc6570).
  class Template
    # Constants used throughout the template code.
    anything =
      Addressable::URI::CharacterClasses::RESERVED +
      Addressable::URI::CharacterClasses::UNRESERVED


    variable_char_class =
      Addressable::URI::CharacterClasses::ALPHA +
      Addressable::URI::CharacterClasses::DIGIT + '_'

    var_char =
      "(?>(?:[#{variable_char_class}]|%[a-fA-F0-9][a-fA-F0-9])+)"
    RESERVED =
      "(?:[#{anything}]|%[a-fA-F0-9][a-fA-F0-9])"
    RESERVED_NO_COMMA =
      "(?:[#{anything.delete(',')}]|%[a-fA-F0-9][a-fA-F0-9])"
    UNRESERVED =
      "(?:[#{
        Addressable::URI::CharacterClasses::UNRESERVED
      }]|%[a-fA-F0-9][a-fA-F0-9])"
    variable =
      "(?:#{var_char}(?:\\.?#{var_char})*)"
    varspec =
      "(?:(#{variable})(\\*|:\\d+)?)"
    VARNAME =
      /^#{variable}$/
    VARSPEC =
      /^#{varspec}$/
    VARIABLE_LIST =
      /^#{varspec}(?:,#{varspec})*$/
    operator =
      "+#./;?&=,!@|"
    EXPRESSION =
      /\{([#{operator}])?(#{varspec}(?:,#{varspec})*)\}/

    # Sentinel used to track variables that are undefined in a particular
    # binding while reversing an expansion.
    UNDEFINED_VALUE = Object.new
    private_constant :UNDEFINED_VALUE

    # Shapes that may be declared for variables when reversing an expansion.
    VARIABLE_SHAPES = [:string, :list, :map].freeze
    private_constant :VARIABLE_SHAPES

    LEADERS = {
      '?' => '?',
      '/' => '/',
      '#' => '#',
      '.' => '.',
      ';' => ';',
      '&' => '&'
    }
    JOINERS = {
      '?' => '&',
      '.' => '.',
      ';' => ';',
      '&' => '&',
      '/' => '/'
    }

    ##
    # Raised if an invalid template value is supplied.
    class InvalidTemplateValueError < StandardError
    end

    ##
    # Raised if an invalid template operator is used in a pattern.
    class InvalidTemplateOperatorError < StandardError
    end

    ##
    # Raised if an invalid template operator is used in a pattern.
    class TemplateOperatorAbortedError < StandardError
    end

    ##
    # This class represents the data that is extracted when a Template
    # is matched against a URI.
    class MatchData
      ##
      # Creates a new MatchData object.
      # MatchData objects should never be instantiated directly.
      #
      # @param [Addressable::URI] uri
      #   The URI that the template was matched against.
      def initialize(uri, template, mapping)
        @uri = uri.dup.freeze
        @template = template
        @mapping = mapping.dup.freeze
      end

      ##
      # @return [Addressable::URI]
      #   The URI that the Template was matched against.
      attr_reader :uri

      ##
      # @return [Addressable::Template]
      #   The Template used for the match.
      attr_reader :template

      ##
      # @return [Hash]
      #   The mapping that resulted from the match.
      #   Note that this mapping does not include keys or values for
      #   variables that appear in the Template, but are not present
      #   in the URI.
      attr_reader :mapping

      ##
      # @return [Array]
      #   The list of variables that were present in the Template.
      #   Note that this list will include variables which do not appear
      #   in the mapping because they were not present in URI.
      def variables
        self.template.variables
      end
      alias_method :keys, :variables
      alias_method :names, :variables

      ##
      # @return [Array]
      #   The list of values that were captured by the Template.
      #   Note that this list will include nils for any variables which
      #   were in the Template, but did not appear in the URI.
      def values
        @values ||= self.variables.inject([]) do |accu, key|
          accu << self.mapping[key]
          accu
        end
      end
      alias_method :captures, :values

      ##
      # Accesses captured values by name or by index.
      #
      # @param [String, Symbol, Fixnum] key
      #   Capture index or name. Note that when accessing by with index
      #   of 0, the full URI will be returned. The intention is to mimic
      #   the ::MatchData#[] behavior.
      #
      # @param [#to_int, nil] len
      #   If provided, an array of values will be returned with the given
      #   parameter used as length.
      #
      # @return [Array, String, nil]
      #   The captured value corresponding to the index or name. If the
      #   value was not provided or the key is unknown, nil will be
      #   returned.
      #
      #   If the second parameter is provided, an array of that length will
      #   be returned instead.
      def [](key, len = nil)
        if len
          to_a[key, len]
        elsif String === key or Symbol === key
          mapping[key.to_s]
        else
          to_a[key]
        end
      end

      ##
      # @return [Array]
      #   Array with the matched URI as first element followed by the captured
      #   values.
      def to_a
        [to_s, *values]
      end

      ##
      # @return [String]
      #   The matched URI as String.
      def to_s
        uri.to_s
      end
      alias_method :string, :to_s

      # Returns multiple captured values at once.
      #
      # @param [String, Symbol, Fixnum] *indexes
      #   Indices of the captures to be returned
      #
      # @return [Array]
      #   Values corresponding to given indices.
      #
      # @see Addressable::Template::MatchData#[]
      def values_at(*indexes)
        indexes.map { |i| self[i] }
      end

      ##
      # Returns a <tt>String</tt> representation of the MatchData's state.
      #
      # @return [String] The MatchData's state, as a <tt>String</tt>.
      def inspect
        sprintf("#<%s:%#0x RESULT:%s>",
          self.class.to_s, self.object_id, self.mapping.inspect)
      end

      ##
      # Dummy method for code expecting a ::MatchData instance
      #
      # @return [String] An empty string.
      def pre_match
        ""
      end
      alias_method :post_match, :pre_match
    end

    ##
    # This class represents the result of reversing a template expansion
    # with declared variable shapes.
    #
    # Unlike <tt>Addressable::Template#extract</tt>, which produces a single
    # best-effort mapping, an <tt>UnexpandResult</tt> distinguishes between
    # three outcomes:
    #
    # - <tt>:no_match</tt>: the URI cannot be produced by the template at all.
    # - <tt>:unique</tt>: exactly one binding can produce the URI.
    # - <tt>:ambiguous</tt>: more than one binding can produce the URI.
    #
    # @see Addressable::Template#unexpand
    class UnexpandResult
      # Status returned when the URI cannot be produced by the template.
      NO_MATCH = :no_match
      # Status returned when exactly one binding can produce the URI.
      UNIQUE = :unique
      # Status returned when more than one binding can produce the URI.
      AMBIGUOUS = :ambiguous

      ##
      # Creates a new UnexpandResult object.
      # UnexpandResult objects should never be instantiated directly.
      #
      # @param [Addressable::Template] template
      #   The Template the URI was matched against.
      # @param [Addressable::URI] uri
      #   The URI that was matched against the template.
      # @param [Array] bindings
      #   The list of distinct bindings that expand to the URI.
      # @param [Array] truncated_variables
      #   The variables whose values may be prefixes of the original values
      #   because a prefix modifier was used in the template.
      def initialize(template, uri, bindings, truncated_variables)
        @template = template
        @uri = uri.dup.freeze
        @bindings = bindings.map { |binding| binding.dup.freeze }.freeze
        @truncated_variables = truncated_variables.dup.freeze
      end

      ##
      # @return [Addressable::Template]
      #   The Template used for the match.
      attr_reader :template

      ##
      # @return [Addressable::URI]
      #   The URI that was matched against the Template.
      attr_reader :uri

      ##
      # @return [Array]
      #   The list of distinct bindings that expand back to the URI.
      #   Empty when the URI does not match the template. Bindings do not
      #   include keys for variables that are undefined in the URI.
      attr_reader :bindings

      ##
      # @return [Array]
      #   The names of variables matched against a prefix modifier at its
      #   maximum length. The values of these variables may be prefixes of
      #   the values originally used to expand the template; the truncated
      #   remainder cannot be recovered from the URI.
      attr_reader :truncated_variables

      ##
      # @return [Symbol]
      #   <tt>:no_match</tt>, <tt>:unique</tt> or <tt>:ambiguous</tt>.
      def status
        if @bindings.empty?
          NO_MATCH
        elsif @bindings.size == 1
          UNIQUE
        else
          AMBIGUOUS
        end
      end

      ##
      # @return [TrueClass, FalseClass]
      #   <code>true</code> if the URI cannot be produced by the template.
      def no_match?
        status == NO_MATCH
      end

      ##
      # @return [TrueClass, FalseClass]
      #   <code>true</code> if exactly one binding can produce the URI.
      def unique?
        status == UNIQUE
      end

      ##
      # @return [TrueClass, FalseClass]
      #   <code>true</code> if more than one binding can produce the URI.
      def ambiguous?
        status == AMBIGUOUS
      end

      ##
      # @return [Hash, NilClass]
      #   The single binding that expands back to the URI when the result
      #   is unique, <tt>nil</tt> otherwise.
      def binding
        unique? ? @bindings.first : nil
      end
      alias_method :mapping, :binding

      ##
      # Returns a <tt>String</tt> representation of the UnexpandResult's
      # state.
      #
      # @return [String] The UnexpandResult's state, as a <tt>String</tt>.
      def inspect
        sprintf("#<%s:%#0x STATUS:%s BINDINGS:%s>",
          self.class.to_s, self.object_id, status, self.bindings.inspect)
      end
    end

    ##
    # Creates a new <tt>Addressable::Template</tt> object.
    #
    # @param [#to_str] pattern The URI Template pattern.
    #
    # @return [Addressable::Template] The initialized Template object.
    def initialize(pattern)
      if !pattern.respond_to?(:to_str)
        raise TypeError, "Can't convert #{pattern.class} into String."
      end
      @pattern = pattern.to_str.dup.freeze
    end

    ##
    # Freeze URI, initializing instance variables.
    #
    # @return [Addressable::URI] The frozen URI object.
    def freeze
      self.variables
      self.variable_defaults
      self.named_captures
      unexpand_tokens
      super
    end

    ##
    # @return [String] The Template object's pattern.
    attr_reader :pattern

    ##
    # Returns a <tt>String</tt> representation of the Template object's state.
    #
    # @return [String] The Template object's state, as a <tt>String</tt>.
    def inspect
      sprintf("#<%s:%#0x PATTERN:%s>",
        self.class.to_s, self.object_id, self.pattern)
    end

    ##
    # Returns <code>true</code> if the Template objects are equal. This method
    # does NOT normalize either Template before doing the comparison.
    #
    # @param [Object] template The Template to compare.
    #
    # @return [TrueClass, FalseClass]
    #   <code>true</code> if the Templates are equivalent, <code>false</code>
    #   otherwise.
    def ==(template)
      return false unless template.kind_of?(Template)
      return self.pattern == template.pattern
    end

    ##
    # Addressable::Template makes no distinction between `==` and `eql?`.
    #
    # @see #==
    alias_method :eql?, :==

    ##
    # Extracts a mapping from the URI using a URI Template pattern.
    #
    # @param [Addressable::URI, #to_str] uri
    #   The URI to extract from.
    #
    # @param [#restore, #match] processor
    #   A template processor object may optionally be supplied.
    #
    #   The object should respond to either the <tt>restore</tt> or
    #   <tt>match</tt> messages or both. The <tt>restore</tt> method should
    #   take two parameters: `[String] name` and `[String] value`.
    #   The <tt>restore</tt> method should reverse any transformations that
    #   have been performed on the value to ensure a valid URI.
    #   The <tt>match</tt> method should take a single
    #   parameter: `[String] name`.  The <tt>match</tt> method should return
    #   a <tt>String</tt> containing a regular expression capture group for
    #   matching on that particular variable. The default value is `".*?"`.
    #   The <tt>match</tt> method has no effect on multivariate operator
    #   expansions.
    #
    # @return [Hash, NilClass]
    #   The <tt>Hash</tt> mapping that was extracted from the URI, or
    #   <tt>nil</tt> if the URI didn't match the template.
    #
    # @example
    #   class ExampleProcessor
    #     def self.restore(name, value)
    #       return value.gsub(/\+/, " ") if name == "query"
    #       return value
    #     end
    #
    #     def self.match(name)
    #       return ".*?" if name == "first"
    #       return ".*"
    #     end
    #   end
    #
    #   uri = Addressable::URI.parse(
    #     "http://example.com/search/an+example+search+query/"
    #   )
    #   Addressable::Template.new(
    #     "http://example.com/search/{query}/"
    #   ).extract(uri, ExampleProcessor)
    #   #=> {"query" => "an example search query"}
    #
    #   uri = Addressable::URI.parse("http://example.com/a/b/c/")
    #   Addressable::Template.new(
    #     "http://example.com/{first}/{second}/"
    #   ).extract(uri, ExampleProcessor)
    #   #=> {"first" => "a", "second" => "b/c"}
    #
    #   uri = Addressable::URI.parse("http://example.com/a/b/c/")
    #   Addressable::Template.new(
    #     "http://example.com/{first}/{-list|/|second}/"
    #   ).extract(uri)
    #   #=> {"first" => "a", "second" => ["b", "c"]}
    def extract(uri, processor=nil)
      match_data = self.match(uri, processor)
      return (match_data ? match_data.mapping : nil)
    end

    ##
    # Extracts match data from the URI using a URI Template pattern.
    #
    # @param [Addressable::URI, #to_str] uri
    #   The URI to extract from.
    #
    # @param [#restore, #match] processor
    #   A template processor object may optionally be supplied.
    #
    #   The object should respond to either the <tt>restore</tt> or
    #   <tt>match</tt> messages or both. The <tt>restore</tt> method should
    #   take two parameters: `[String] name` and `[String] value`.
    #   The <tt>restore</tt> method should reverse any transformations that
    #   have been performed on the value to ensure a valid URI.
    #   The <tt>match</tt> method should take a single
    #   parameter: `[String] name`. The <tt>match</tt> method should return
    #   a <tt>String</tt> containing a regular expression capture group for
    #   matching on that particular variable. The default value is `".*?"`.
    #   The <tt>match</tt> method has no effect on multivariate operator
    #   expansions.
    #
    # @return [Hash, NilClass]
    #   The <tt>Hash</tt> mapping that was extracted from the URI, or
    #   <tt>nil</tt> if the URI didn't match the template.
    #
    # @example
    #   class ExampleProcessor
    #     def self.restore(name, value)
    #       return value.gsub(/\+/, " ") if name == "query"
    #       return value
    #     end
    #
    #     def self.match(name)
    #       return ".*?" if name == "first"
    #       return ".*"
    #     end
    #   end
    #
    #   uri = Addressable::URI.parse(
    #     "http://example.com/search/an+example+search+query/"
    #   )
    #   match = Addressable::Template.new(
    #     "http://example.com/search/{query}/"
    #   ).match(uri, ExampleProcessor)
    #   match.variables
    #   #=> ["query"]
    #   match.captures
    #   #=> ["an example search query"]
    #
    #   uri = Addressable::URI.parse("http://example.com/a/b/c/")
    #   match = Addressable::Template.new(
    #     "http://example.com/{first}/{+second}/"
    #   ).match(uri, ExampleProcessor)
    #   match.variables
    #   #=> ["first", "second"]
    #   match.captures
    #   #=> ["a", "b/c"]
    #
    #   uri = Addressable::URI.parse("http://example.com/a/b/c/")
    #   match = Addressable::Template.new(
    #     "http://example.com/{first}{/second*}/"
    #   ).match(uri)
    #   match.variables
    #   #=> ["first", "second"]
    #   match.captures
    #   #=> ["a", ["b", "c"]]
    def match(uri, processor=nil)
      uri = Addressable::URI.parse(uri) unless uri.is_a?(Addressable::URI)
      mapping = {}

      # First, we need to process the pattern, and extract the values.
      expansions, expansion_regexp =
        parse_template_pattern(pattern, processor)

      return nil unless uri.to_str.match(expansion_regexp)
      unparsed_values = uri.to_str.scan(expansion_regexp).flatten

      if uri.to_str == pattern
        return Addressable::Template::MatchData.new(uri, self, mapping)
      elsif expansions.size > 0
        index = 0
        expansions.each do |expansion|
          _, operator, varlist = *expansion.match(EXPRESSION)
          varlist.split(',').each do |varspec|
            _, name, modifier = *varspec.match(VARSPEC)
            mapping[name] ||= nil
            case operator
            when nil, '+', '#', '/', '.'
              unparsed_value = unparsed_values[index]
              name = varspec[VARSPEC, 1]
              value = unparsed_value
              value = value.split(JOINERS[operator]) if value && modifier == '*'
            when ';', '?', '&'
              if modifier == '*'
                if unparsed_values[index]
                  value = unparsed_values[index].split(JOINERS[operator])
                  value = value.inject({}) do |acc, v|
                    key, val = v.split('=')
                    val = "" if val.nil?
                    acc[key] = val
                    acc
                  end
                end
              else
                if (unparsed_values[index])
                  name, value = unparsed_values[index].split('=')
                  value = "" if value.nil?
                end
              end
            end
            if processor != nil && processor.respond_to?(:restore)
              value = processor.restore(name, value)
            end
            if processor == nil
              if value.is_a?(Hash)
                value = value.inject({}){|acc, (k, v)|
                  acc[Addressable::URI.unencode_component(k)] =
                    Addressable::URI.unencode_component(v)
                  acc
                }
              elsif value.is_a?(Array)
                value = value.map{|v| Addressable::URI.unencode_component(v) }
              else
                value = Addressable::URI.unencode_component(value)
              end
            end
            if !mapping.has_key?(name) || mapping[name].nil?
              # Doesn't exist, set to value (even if value is nil)
              mapping[name] = value
            end
            index = index + 1
          end
        end
        return Addressable::Template::MatchData.new(uri, self, mapping)
      else
        return nil
      end
    end

    ##
    # Reverses a template expansion, enumerating every binding of the
    # template's variables that expands back to the given URI.
    #
    # Unlike <tt>#extract</tt>, this method does not rely on a single
    # best-effort regular expression match. It parses the template into
    # literals and expressions and searches for all variable bindings that
    # are consistent with the template's operators, the declared variable
    # shapes, and the URI. Variables that appear more than once in the
    # template must be bound to the same value in every occurrence.
    #
    # @param [Addressable::URI, #to_str] uri
    #   The URI to reverse the expansion for.
    #
    # @param [Hash] shapes
    #   Declares the expected shape of individual variables. Keys are variable
    #   names, values are one of <tt>:string</tt> (the default),
    #   <tt>:list</tt> or <tt>:map</tt>. Variables without a declared shape
    #   are treated as strings.
    #
    # @return [Addressable::Template::UnexpandResult]
    #   The result of the reverse expansion. Its <tt>status</tt> is
    #   <tt>:no_match</tt> when the URI cannot be produced by the template,
    #   <tt>:unique</tt> when exactly one binding can produce it, and
    #   <tt>:ambiguous</tt> when more than one binding can. Every returned
    #   binding expands back to the original URI. Values of variables
    #   matched against a prefix modifier at its maximum length may be
    #   prefixes of the original values; such variables are listed in
    #   <tt>truncated_variables</tt> and their truncated remainders are
    #   never fabricated.
    #
    # @example
    #   template = Addressable::Template.new(
    #     "http://example.com/{/segments*}{?q}"
    #   )
    #   result = template.unexpand(
    #     "http://example.com/a/b/c?q=hello",
    #     {"segments" => :list}
    #   )
    #   result.status
    #   #=> :unique
    #   result.binding
    #   #=> {"segments" => ["a", "b", "c"], "q" => "hello"}
    #
    #   template = Addressable::Template.new("http://example.com/{+a,b}")
    #   result = template.unexpand("http://example.com/x,y")
    #   result.status
    #   #=> :ambiguous
    #   result.bindings
    #   #=> [{"b" => "x,y"}, {"a" => "x", "b" => "y"}, {"a" => "x,y"}]
    def unexpand(uri, shapes = {})
      uri = Addressable::URI.parse(uri) unless uri.is_a?(Addressable::URI)
      shapes = normalize_variable_shapes(shapes)
      solutions = []
      match_unexpand_tokens(
        unexpand_tokens, uri.to_str, 0, {}, [], solutions, shapes
      )
      bindings = []
      truncated_variables = []
      solutions.each do |env, truncated|
        binding = {}
        env.each do |name, value|
          binding[name] = value unless value.equal?(UNDEFINED_VALUE)
        end
        next if bindings.include?(binding)
        # Only keep bindings that expand back to the original URI.
        begin
          next unless expand(binding).to_str == uri.to_str
        rescue StandardError
          next
        end
        bindings << binding
        truncated_variables |= truncated
      end
      return Addressable::Template::UnexpandResult.new(
        self, uri, bindings, truncated_variables
      )
    end

    ##
    # Expands a URI template into another URI template.
    #
    # @param [Hash] mapping The mapping that corresponds to the pattern.
    # @param [#validate, #transform] processor
    #   An optional processor object may be supplied.
    # @param [Boolean] normalize_values
    #   Optional flag to enable/disable unicode normalization. Default: true
    #
    # The object should respond to either the <tt>validate</tt> or
    # <tt>transform</tt> messages or both. Both the <tt>validate</tt> and
    # <tt>transform</tt> methods should take two parameters: <tt>name</tt> and
    # <tt>value</tt>. The <tt>validate</tt> method should return <tt>true</tt>
    # or <tt>false</tt>; <tt>true</tt> if the value of the variable is valid,
    # <tt>false</tt> otherwise. An <tt>InvalidTemplateValueError</tt>
    # exception will be raised if the value is invalid. The <tt>transform</tt>
    # method should return the transformed variable value as a <tt>String</tt>.
    # If a <tt>transform</tt> method is used, the value will not be percent
    # encoded automatically. Unicode normalization will be performed both
    # before and after sending the value to the transform method.
    #
    # @return [Addressable::Template] The partially expanded URI template.
    #
    # @example
    #   Addressable::Template.new(
    #     "http://example.com/{one}/{two}/"
    #   ).partial_expand({"one" => "1"}).pattern
    #   #=> "http://example.com/1/{two}/"
    #
    #   Addressable::Template.new(
    #     "http://example.com/{?one,two}/"
    #   ).partial_expand({"one" => "1"}).pattern
    #   #=> "http://example.com/?one=1{&two}/"
    #
    #   Addressable::Template.new(
    #     "http://example.com/{?one,two,three}/"
    #   ).partial_expand({"one" => "1", "three" => 3}).pattern
    #   #=> "http://example.com/?one=1{&two}&three=3"
    def partial_expand(mapping, processor=nil, normalize_values=true)
      result = self.pattern.dup
      mapping = normalize_keys(mapping)
      result.gsub!( EXPRESSION ) do |capture|
        transform_partial_capture(mapping, capture, processor, normalize_values)
      end
      return Addressable::Template.new(result)
    end

    ##
    # Expands a URI template into a full URI.
    #
    # @param [Hash] mapping The mapping that corresponds to the pattern.
    # @param [#validate, #transform] processor
    #   An optional processor object may be supplied.
    # @param [Boolean] normalize_values
    #   Optional flag to enable/disable unicode normalization. Default: true
    #
    # The object should respond to either the <tt>validate</tt> or
    # <tt>transform</tt> messages or both. Both the <tt>validate</tt> and
    # <tt>transform</tt> methods should take two parameters: <tt>name</tt> and
    # <tt>value</tt>. The <tt>validate</tt> method should return <tt>true</tt>
    # or <tt>false</tt>; <tt>true</tt> if the value of the variable is valid,
    # <tt>false</tt> otherwise. An <tt>InvalidTemplateValueError</tt>
    # exception will be raised if the value is invalid. The <tt>transform</tt>
    # method should return the transformed variable value as a <tt>String</tt>.
    # If a <tt>transform</tt> method is used, the value will not be percent
    # encoded automatically. Unicode normalization will be performed both
    # before and after sending the value to the transform method.
    #
    # @return [Addressable::URI] The expanded URI template.
    #
    # @example
    #   class ExampleProcessor
    #     def self.validate(name, value)
    #       return !!(value =~ /^[\w ]+$/) if name == "query"
    #       return true
    #     end
    #
    #     def self.transform(name, value)
    #       return value.gsub(/ /, "+") if name == "query"
    #       return value
    #     end
    #   end
    #
    #   Addressable::Template.new(
    #     "http://example.com/search/{query}/"
    #   ).expand(
    #     {"query" => "an example search query"},
    #     ExampleProcessor
    #   ).to_str
    #   #=> "http://example.com/search/an+example+search+query/"
    #
    #   Addressable::Template.new(
    #     "http://example.com/search/{query}/"
    #   ).expand(
    #     {"query" => "an example search query"}
    #   ).to_str
    #   #=> "http://example.com/search/an%20example%20search%20query/"
    #
    #   Addressable::Template.new(
    #     "http://example.com/search/{query}/"
    #   ).expand(
    #     {"query" => "bogus!"},
    #     ExampleProcessor
    #   ).to_str
    #   #=> Addressable::Template::InvalidTemplateValueError
    def expand(mapping, processor=nil, normalize_values=true)
      result = self.pattern.dup
      mapping = normalize_keys(mapping)
      result.gsub!( EXPRESSION ) do |capture|
        transform_capture(mapping, capture, processor, normalize_values)
      end
      return Addressable::URI.parse(result)
    end

    ##
    # Returns an Array of variables used within the template pattern.
    # The variables are listed in the Array in the order they appear within
    # the pattern.  Multiple occurrences of a variable within a pattern are
    # not represented in this Array.
    #
    # @return [Array] The variables present in the template's pattern.
    def variables
      @variables ||= ordered_variable_defaults.map { |var, val| var }.uniq
    end
    alias_method :keys, :variables
    alias_method :names, :variables

    ##
    # Returns a mapping of variables to their default values specified
    # in the template. Variables without defaults are not returned.
    #
    # @return [Hash] Mapping of template variables to their defaults
    def variable_defaults
      @variable_defaults ||=
        Hash[*ordered_variable_defaults.reject { |k, v| v.nil? }.flatten]
    end

    ##
    # Coerces a template into a `Regexp` object. This regular expression will
    # behave very similarly to the actual template, and should match the same
    # URI values, but it cannot fully handle, for example, values that would
    # extract to an `Array`.
    #
    # @return [Regexp] A regular expression which should match the template.
    def to_regexp
      _, source = parse_template_pattern(pattern)
      Regexp.new(source)
    end

    ##
    # Returns the source of the coerced `Regexp`.
    #
    # @return [String] The source of the `Regexp` given by {#to_regexp}.
    #
    # @api private
    def source
      self.to_regexp.source
    end

    ##
    # Returns the named captures of the coerced `Regexp`.
    #
    # @return [Hash] The named captures of the `Regexp` given by {#to_regexp}.
    #
    # @api private
    def named_captures
      self.to_regexp.named_captures
    end

  private
    def ordered_variable_defaults
      @ordered_variable_defaults ||= begin
        expansions, _ = parse_template_pattern(pattern)
        expansions.flat_map do |capture|
          _, _, varlist = *capture.match(EXPRESSION)
          varlist.split(',').map do |varspec|
            varspec[VARSPEC, 1]
          end
        end
      end
    end

    ##
    # Validates and normalizes the variable shape declarations used by
    # <tt>#unexpand</tt>.
    #
    # @param [Hash] shapes The declared shapes, keyed by variable name.
    #
    # @return [Hash] A hash with string keys and shape symbols as values.
    def normalize_variable_shapes(shapes)
      shapes.inject({}) do |accu, (name, shape)|
        shape = shape.to_sym
        unless VARIABLE_SHAPES.include?(shape)
          raise ArgumentError, "Unknown variable shape: #{shape.inspect}."
        end
        accu[name.to_s] = shape
        accu
      end
    end

    ##
    # Splits the template pattern into a sequence of literal strings and
    # expressions for <tt>#unexpand</tt>.
    #
    # @return [Array]
    #   A list of <tt>[:literal, string]</tt> and
    #   <tt>[:expression, operator, varspecs]</tt> tokens, where each varspec
    #   is a hash with <tt>:name</tt> and <tt>:modifier</tt> keys.
    def unexpand_tokens
      @unexpand_tokens ||= begin
        tokens = []
        position = 0
        pattern.scan(EXPRESSION) do
          match = Regexp.last_match
          literal = pattern[position...match.begin(0)]
          tokens << [:literal, literal] unless literal.empty?
          varspecs = match[2].split(',').map do |varspec|
            _, name, modifier = *varspec.match(VARSPEC)
            {:name => name, :modifier => modifier}
          end
          tokens << [:expression, match[1], varspecs]
          position = match.end(0)
        end
        tail = pattern[position..-1]
        tokens << [:literal, tail] unless tail.empty?
        tokens
      end
    end

    ##
    # Recursively matches a token sequence against the URI string,
    # collecting every consistent variable binding.
    #
    # @param [Array] tokens The remaining tokens to match.
    # @param [String] string The URI string being matched.
    # @param [Integer] position The current offset into the string.
    # @param [Hash] env The bindings accumulated so far.
    # @param [Array] truncated The truncated variables encountered so far.
    # @param [Array] solutions The collector for [env, truncated] pairs.
    # @param [Hash] shapes The declared variable shapes.
    def match_unexpand_tokens(tokens, string, position, env, truncated,
                              solutions, shapes)
      if tokens.empty?
        solutions << [env, truncated] if position == string.length
        return
      end
      token = tokens.first
      rest = tokens[1..-1]
      if token[0] == :literal
        literal = token[1]
        if string[position, literal.length] == literal
          match_unexpand_tokens(
            rest, string, position + literal.length,
            env, truncated, solutions, shapes
          )
        end
      else
        _, operator, varspecs = token
        unexpand_expression_matches(
          operator, varspecs, string, position, shapes
        ).each do |new_position, assignments, truncated_names|
          merged = merge_unexpand_assignments(env, assignments)
          next unless merged
          match_unexpand_tokens(
            rest, string, new_position,
            merged, truncated | truncated_names, solutions, shapes
          )
        end
      end
    end

    ##
    # Merges two partial bindings, requiring variables that appear in both
    # to agree. Repeated occurrences of a variable in a template must be
    # bound to the same value in every occurrence.
    #
    # @return [Hash, NilClass]
    #   The merged bindings, or <tt>nil</tt> if they conflict.
    def merge_unexpand_assignments(env, assignments)
      merged = env.dup
      assignments.each do |name, value|
        if merged.has_key?(name)
          return nil unless merged[name] == value
        else
          merged[name] = value
        end
      end
      return merged
    end

    ##
    # Enumerates every way a single expression can consume a prefix of the
    # string at the given position.
    #
    # @return [Array]
    #   A list of <tt>[new_position, assignments, truncated_names]</tt>
    #   tuples. The assignments cover every variable of the expression,
    #   using <tt>UNDEFINED_VALUE</tt> for undefined variables.
    def unexpand_expression_matches(operator, varspecs, string, position,
                                    shapes)
      results = []
      absent = {}
      varspecs.each { |varspec| absent[varspec[:name]] = UNDEFINED_VALUE }
      results << [position, absent, []]
      leader = LEADERS.fetch(operator, '')
      if string[position, leader.length] == leader
        body_start = position + leader.length
        (body_start..string.length).each do |body_end|
          body = string[body_start...body_end]
          parse_unexpand_body(operator, varspecs, body, shapes).each do
              |assignments, truncated_names|
            results << [body_end, assignments, truncated_names]
          end
        end
      end
      return results
    end

    ##
    # Parses the body of an expression (the text after its leader) into
    # every possible set of variable assignments.
    #
    # @return [Array] A list of <tt>[assignments, truncated_names]</tt>.
    def parse_unexpand_body(operator, varspecs, body, shapes)
      case operator
      when ';', '?', '&'
        pairs = body.empty? ? [] :
          body.split(JOINERS[operator], -1).map { |pair| pair.split('=', 2) }
        assign_unexpand_pairs(varspecs, pairs, shapes)
      else
        joiner = JOINERS.fetch(operator, ',')
        chunks = body.empty? ? [""] : body.split(joiner, -1)
        assign_unexpand_chunks(varspecs, chunks, operator, joiner, shapes)
      end
    end

    ##
    # Assigns consecutive runs of joiner-separated chunks to the varspecs
    # of a value-oriented expression (no operator, or one of + # / .).
    # Undefined variables consume no chunks, mirroring expansion, where
    # undefined variables are skipped along with their separators.
    #
    # @return [Array] A list of <tt>[assignments, truncated_names]</tt>.
    def assign_unexpand_chunks(varspecs, chunks, operator, joiner, shapes)
      return chunks.empty? ? [[{}, []]] : [] if varspecs.empty?
      varspec = varspecs.first
      rest = varspecs[1..-1]
      results = []
      assign_unexpand_chunks(rest, chunks, operator, joiner, shapes).each do
          |env, truncated|
        results << [
          {varspec[:name] => UNDEFINED_VALUE}.merge(env), truncated
        ]
      end
      shape = shapes[varspec[:name]] || :string
      (1..chunks.length).each do |count|
        taken = chunks.first(count)
        remaining = chunks[count..-1]
        parsed = parse_unexpand_chunk_run(
          varspec, taken, operator, joiner, shape
        )
        next unless parsed
        value, truncated_name = parsed
        assign_unexpand_chunks(rest, remaining, operator, joiner, shapes).
          each do |env, truncated|
            truncated = truncated + [varspec[:name]] if truncated_name
            results << [{varspec[:name] => value}.merge(env), truncated]
          end
      end
      return results
    end

    ##
    # Interprets a run of chunks as the value of a single varspec,
    # according to its declared shape, modifier and operator.
    #
    # @return [Array, NilClass]
    #   A <tt>[value, truncated]</tt> tuple, or <tt>nil</tt> if the chunks
    #   cannot represent a value of the declared shape.
    def parse_unexpand_chunk_run(varspec, chunks, operator, joiner, shape)
      modifier = varspec[:modifier]
      if modifier =~ /^:(\d+)$/
        # Prefix modifiers only apply to string values. The matched text
        # may be a prefix of the original value; never invent the rest.
        return nil unless shape == :string && chunks.length == 1
        return nil unless unexpand_text_valid?(chunks.first, operator)
        limit = $1.to_i
        length = unexpand_encoded_length(chunks.first)
        return nil if length > limit
        return [
          Addressable::URI.unencode_component(chunks.first),
          length == limit
        ]
      end
      case shape
      when :string
        if operator == '+' || operator == '#' || operator == '.'
          # Reserved (and, for '.', the joiner itself) characters may
          # appear unencoded in string values for these operators.
          value = chunks.join(joiner)
        else
          return nil unless chunks.length == 1
          value = chunks.first
        end
        return nil unless chunks.all? { |chunk|
          unexpand_text_valid?(chunk, operator)
        }
        [Addressable::URI.unencode_component(value), false]
      when :list
        elements =
          if varspec[:modifier] == '*' || joiner == ','
            chunks
          else
            return nil unless chunks.length == 1
            split_unexpand_list(chunks.first)
          end
        return nil unless elements.all? { |element|
          unexpand_text_valid?(element, operator)
        }
        [elements.map { |e| Addressable::URI.unencode_component(e) }, false]
      when :map
        if varspec[:modifier] == '*'
          result = {}
          chunks.each do |chunk|
            key, value = chunk.split('=', 2)
            return nil unless unexpand_text_valid?(key, operator) &&
              unexpand_text_valid?(value || '', operator)
            result[Addressable::URI.unencode_component(key)] =
              Addressable::URI.unencode_component(value || '')
          end
          [result, false]
        else
          items =
            if joiner == ','
              chunks
            else
              return nil unless chunks.length == 1
              split_unexpand_list(chunks.first)
            end
          return nil if items.empty? || items.length.odd?
          return nil unless items.all? { |item|
            unexpand_text_valid?(item, operator)
          }
          result = {}
          items.each_slice(2) do |key, value|
            result[Addressable::URI.unencode_component(key)] =
              Addressable::URI.unencode_component(value)
          end
          [result, false]
        end
      end
    end

    ##
    # Assigns consecutive runs of name/value pairs to the varspecs of a
    # parameter-oriented expression (; ? &). Undefined variables consume
    # no pairs.
    #
    # @return [Array] A list of <tt>[assignments, truncated_names]</tt>.
    def assign_unexpand_pairs(varspecs, pairs, shapes)
      return pairs.empty? ? [[{}, []]] : [] if varspecs.empty?
      varspec = varspecs.first
      rest = varspecs[1..-1]
      results = []
      assign_unexpand_pairs(rest, pairs, shapes).each do |env, truncated|
        results << [
          {varspec[:name] => UNDEFINED_VALUE}.merge(env), truncated
        ]
      end
      shape = shapes[varspec[:name]] || :string
      (1..pairs.length).each do |count|
        taken = pairs.first(count)
        remaining = pairs[count..-1]
        parsed = parse_unexpand_pair_run(varspec, taken, shape)
        next unless parsed
        value, truncated_name = parsed
        assign_unexpand_pairs(rest, remaining, shapes).each do
            |env, truncated|
          truncated = truncated + [varspec[:name]] if truncated_name
          results << [{varspec[:name] => value}.merge(env), truncated]
        end
      end
      return results
    end

    ##
    # Interprets a run of name/value pairs as the value of a single
    # varspec of a parameter-oriented expression (; ? &).
    #
    # @return [Array, NilClass]
    #   A <tt>[value, truncated]</tt> tuple, or <tt>nil</tt> if the pairs
    #   cannot represent a value of the declared shape.
    def parse_unexpand_pair_run(varspec, pairs, shape)
      name = varspec[:name]
      modifier = varspec[:modifier]
      return nil unless pairs.all? { |key, value|
        unexpand_text_valid?(key, nil)
      }
      if modifier =~ /^:(\d+)$/
        # Prefix modifiers only apply to string values. The matched text
        # may be a prefix of the original value; never invent the rest.
        return nil unless shape == :string && pairs.length == 1
        key, value = pairs.first
        return nil unless Addressable::URI.unencode_component(key) == name
        value ||= ''
        return nil unless unexpand_text_valid?(value, nil)
        limit = $1.to_i
        length = unexpand_encoded_length(value)
        return nil if length > limit
        return [Addressable::URI.unencode_component(value), length == limit]
      end
      case shape
      when :string
        return nil unless pairs.length == 1
        key, value = pairs.first
        return nil unless Addressable::URI.unencode_component(key) == name
        return nil unless unexpand_text_valid?(value || '', nil)
        [Addressable::URI.unencode_component(value || ''), false]
      when :list
        if modifier == '*'
          return nil unless pairs.all? { |key, _|
            Addressable::URI.unencode_component(key) == name
          }
          return nil unless pairs.all? { |_, value|
            unexpand_text_valid?(value || '', nil)
          }
          [
            pairs.map { |_, value|
              Addressable::URI.unencode_component(value || '')
            },
            false
          ]
        else
          return nil unless pairs.length == 1
          key, value = pairs.first
          return nil unless Addressable::URI.unencode_component(key) == name
          elements = split_unexpand_list(value || '')
          return nil unless elements.all? { |element|
            unexpand_text_valid?(element, nil)
          }
          [
            elements.map { |e| Addressable::URI.unencode_component(e) },
            false
          ]
        end
      when :map
        if modifier == '*'
          result = {}
          pairs.each do |key, value|
            return nil unless unexpand_text_valid?(value || '', nil)
            result[Addressable::URI.unencode_component(key)] =
              Addressable::URI.unencode_component(value || '')
          end
          [result, false]
        else
          return nil unless pairs.length == 1
          key, value = pairs.first
          return nil unless Addressable::URI.unencode_component(key) == name
          items = split_unexpand_list(value || '')
          return nil if items.empty? || items.length.odd?
          return nil unless items.all? { |item|
            unexpand_text_valid?(item, nil)
          }
          result = {}
          items.each_slice(2) do |item_key, item_value|
            result[Addressable::URI.unencode_component(item_key)] =
              Addressable::URI.unencode_component(item_value)
          end
          [result, false]
        end
      end
    end

    ##
    # Splits a comma-separated value, keeping a single empty element for
    # empty values (which represent a one-element list, not an empty one).
    #
    # @return [Array] The split elements.
    def split_unexpand_list(value)
      value.empty? ? [""] : value.split(',', -1)
    end

    ##
    # Counts the characters of a percent-encoded string, treating each
    # percent-encoded octet triplet as a single character. Used to apply
    # prefix modifiers while reversing an expansion.
    #
    # @return [Integer] The number of encoded characters.
    def unexpand_encoded_length(value)
      value.gsub(/%[a-fA-F0-9]{2}/, "\0").length
    end

    ##
    # Checks whether matched text only contains characters that the given
    # operator would have left unencoded during expansion.
    #
    # @return [TrueClass, FalseClass]
    def unexpand_text_valid?(text, operator)
      allowed = (operator == '+' || operator == '#') ? RESERVED : UNRESERVED
      !!(text =~ /\A(?:#{allowed})*\z/)
    end

    ##
    # Loops through each capture and expands any values available in mapping
    #
    # @param [Hash] mapping
    #   Set of keys to expand
    # @param [String] capture
    #   The expression to expand
    # @param [#validate, #transform] processor
    #   An optional processor object may be supplied.
    # @param [Boolean] normalize_values
    #   Optional flag to enable/disable unicode normalization. Default: true
    #
    # The object should respond to either the <tt>validate</tt> or
    # <tt>transform</tt> messages or both. Both the <tt>validate</tt> and
    # <tt>transform</tt> methods should take two parameters: <tt>name</tt> and
    # <tt>value</tt>. The <tt>validate</tt> method should return <tt>true</tt>
    # or <tt>false</tt>; <tt>true</tt> if the value of the variable is valid,
    # <tt>false</tt> otherwise. An <tt>InvalidTemplateValueError</tt> exception
    # will be raised if the value is invalid. The <tt>transform</tt> method
    # should return the transformed variable value as a <tt>String</tt>. If a
    # <tt>transform</tt> method is used, the value will not be percent encoded
    # automatically. Unicode normalization will be performed both before and
    # after sending the value to the transform method.
    #
    # @return [String] The expanded expression
    def transform_partial_capture(mapping, capture, processor = nil,
                                  normalize_values = true)
      _, operator, varlist = *capture.match(EXPRESSION)

      vars = varlist.split(",")

      if operator == "?"
        # partial expansion of form style query variables sometimes requires a
        # slight reordering of the variables to produce a valid url.
        first_to_expand = vars.find { |varspec|
          _, name, _ =  *varspec.match(VARSPEC)
          mapping.key?(name) && !mapping[name].nil?
        }

        vars = [first_to_expand] + vars.reject {|varspec| varspec == first_to_expand}  if first_to_expand
      end

      vars.
        inject("".dup) do |acc, varspec|
          _, name, _ =  *varspec.match(VARSPEC)
          next_val = if mapping.key? name
                       transform_capture(mapping, "{#{operator}#{varspec}}",
                                         processor, normalize_values)
                     else
                       "{#{operator}#{varspec}}"
                     end
          # If we've already expanded at least one '?' operator with non-empty
          # value, change to '&'
          operator = "&" if (operator == "?") && (next_val != "")
          acc << next_val
      end
    end

    ##
    # Transforms a mapped value so that values can be substituted into the
    # template.
    #
    # @param [Hash] mapping The mapping to replace captures
    # @param [String] capture
    #   The expression to replace
    # @param [#validate, #transform] processor
    #   An optional processor object may be supplied.
    # @param [Boolean] normalize_values
    #   Optional flag to enable/disable unicode normalization. Default: true
    #
    #
    # The object should respond to either the <tt>validate</tt> or
    # <tt>transform</tt> messages or both. Both the <tt>validate</tt> and
    # <tt>transform</tt> methods should take two parameters: <tt>name</tt> and
    # <tt>value</tt>. The <tt>validate</tt> method should return <tt>true</tt>
    # or <tt>false</tt>; <tt>true</tt> if the value of the variable is valid,
    # <tt>false</tt> otherwise. An <tt>InvalidTemplateValueError</tt> exception
    # will be raised if the value is invalid. The <tt>transform</tt> method
    # should return the transformed variable value as a <tt>String</tt>. If a
    # <tt>transform</tt> method is used, the value will not be percent encoded
    # automatically. Unicode normalization will be performed both before and
    # after sending the value to the transform method.
    #
    # @return [String] The expanded expression
    def transform_capture(mapping, capture, processor=nil,
                          normalize_values=true)
      _, operator, varlist = *capture.match(EXPRESSION)
      return_value = varlist.split(',').inject([]) do |acc, varspec|
        _, name, modifier = *varspec.match(VARSPEC)
        value = mapping[name]
        unless value == nil || value == {}
          allow_reserved = %w(+ #).include?(operator)
          # Common primitives where the .to_s output is well-defined
          if Numeric === value || Symbol === value ||
              value == true || value == false
            value = value.to_s
          end
          length = modifier.gsub(':', '').to_i if modifier =~ /^:\d+/

          unless (Hash === value) ||
            value.respond_to?(:to_ary) || value.respond_to?(:to_str)
            raise TypeError,
              "Can't convert #{value.class} into String or Array."
          end

          value = normalize_value(value) if normalize_values

          if processor == nil || !processor.respond_to?(:transform)
            # Handle percent escaping
            if allow_reserved
              encode_map =
                Addressable::URI::CharacterClasses::RESERVED +
                Addressable::URI::CharacterClasses::UNRESERVED
            else
              encode_map = Addressable::URI::CharacterClasses::UNRESERVED
            end
            if value.kind_of?(Array)
              transformed_value = value.map do |val|
                if length
                  Addressable::URI.encode_component(val[0...length], encode_map)
                else
                  Addressable::URI.encode_component(val, encode_map)
                end
              end
              unless modifier == "*"
                transformed_value = transformed_value.join(',')
              end
            elsif value.kind_of?(Hash)
              transformed_value = value.map do |key, val|
                if modifier == "*"
                  "#{
                    Addressable::URI.encode_component( key, encode_map)
                  }=#{
                    Addressable::URI.encode_component( val, encode_map)
                  }"
                else
                  "#{
                    Addressable::URI.encode_component( key, encode_map)
                  },#{
                    Addressable::URI.encode_component( val, encode_map)
                  }"
                end
              end
              unless modifier == "*"
                transformed_value = transformed_value.join(',')
              end
            else
              if length
                transformed_value = Addressable::URI.encode_component(
                  value[0...length], encode_map)
              else
                transformed_value = Addressable::URI.encode_component(
                  value, encode_map)
              end
            end
          end

          # Process, if we've got a processor
          if processor != nil
            if processor.respond_to?(:validate)
              if !processor.validate(name, value)
                display_value = value.kind_of?(Array) ? value.inspect : value
                raise InvalidTemplateValueError,
                  "#{name}=#{display_value} is an invalid template value."
              end
            end
            if processor.respond_to?(:transform)
              transformed_value = processor.transform(name, value)
              if normalize_values
                transformed_value = normalize_value(transformed_value)
              end
            end
          end
          acc << [name, transformed_value]
        end
        acc
      end
      return "" if return_value.empty?
      join_values(operator, return_value)
    end

    ##
    # Takes a set of values, and joins them together based on the
    # operator.
    #
    # @param [String, Nil] operator One of the operators from the set
    #   (?,&,+,#,;,/,.), or nil if there wasn't one.
    # @param [Array] return_value
    #   The set of return values (as [variable_name, value] tuples) that will
    #   be joined together.
    #
    # @return [String] The transformed mapped value
    def join_values(operator, return_value)
      leader = LEADERS.fetch(operator, '')
      joiner = JOINERS.fetch(operator, ',')
      case operator
      when '&', '?'
        leader + return_value.map{|k,v|
          if v.is_a?(Array) && v.first =~ /=/
            v.join(joiner)
          elsif v.is_a?(Array)
            v.map{|inner_value| "#{k}=#{inner_value}"}.join(joiner)
          else
            "#{k}=#{v}"
          end
        }.join(joiner)
      when ';'
        return_value.map{|k,v|
          if v.is_a?(Array) && v.first =~ /=/
            ';' + v.join(";")
          elsif v.is_a?(Array)
            ';' + v.map{|inner_value| "#{k}=#{inner_value}"}.join(";")
          else
            v && v != '' ?  ";#{k}=#{v}" : ";#{k}"
          end
        }.join
      else
        leader + return_value.map{|k,v| v}.join(joiner)
      end
    end

    ##
    # Takes a set of values, and joins them together based on the
    # operator.
    #
    # @param [Hash, Array, String] value
    #   Normalizes unicode keys and values with String#unicode_normalize (NFC)
    #
    # @return [Hash, Array, String] The normalized values
    def normalize_value(value)
      # Handle unicode normalization
      if value.respond_to?(:to_ary)
        value.to_ary.map! { |val| normalize_value(val) }
      elsif value.kind_of?(Hash)
        value = value.inject({}) { |acc, (k, v)|
          acc[normalize_value(k)] = normalize_value(v)
          acc
        }
      else
        value = value.to_s if !value.kind_of?(String)
        if value.encoding != Encoding::UTF_8
          value = value.dup.force_encoding(Encoding::UTF_8)
        end
        value = value.unicode_normalize(:nfc)
      end
      value
    end

    ##
    # Generates a hash with string keys
    #
    # @param [Hash] mapping A mapping hash to normalize
    #
    # @return [Hash]
    #   A hash with stringified keys
    def normalize_keys(mapping)
      return mapping.inject({}) do |accu, pair|
        name, value = pair
        if Symbol === name
          name = name.to_s
        elsif name.respond_to?(:to_str)
          name = name.to_str
        else
          raise TypeError,
            "Can't convert #{name.class} into String."
        end
        accu[name] = value
        accu
      end
    end

    ##
    # Generates the <tt>Regexp</tt> that parses a template pattern. Memoizes the
    # value if template processor not set (processors may not be deterministic)
    #
    # @param [String] pattern The URI template pattern.
    # @param [#match] processor The template processor to use.
    #
    # @return [Array, Regexp]
    #   An array of expansion variables nad a regular expression which may be
    #   used to parse a template pattern
    def parse_template_pattern(pattern, processor = nil)
      if processor.nil? && pattern == @pattern
        @cached_template_parse ||=
          parse_new_template_pattern(pattern, processor)
      else
        parse_new_template_pattern(pattern, processor)
      end
    end

    ##
    # Generates the <tt>Regexp</tt> that parses a template pattern.
    #
    # @param [String] pattern The URI template pattern.
    # @param [#match] processor The template processor to use.
    #
    # @return [Array, Regexp]
    #   An array of expansion variables nad a regular expression which may be
    #   used to parse a template pattern
    def parse_new_template_pattern(pattern, processor = nil)
      # Escape the pattern. The two gsubs restore the escaped curly braces
      # back to their original form. Basically, escape everything that isn't
      # within an expansion.
      escaped_pattern = Regexp.escape(
        pattern
      ).gsub(/\\\{(.*?)\\\}/) do |escaped|
        escaped.gsub(/\\(.)/, "\\1")
      end

      expansions = []

      # Create a regular expression that captures the values of the
      # variables in the URI.
      regexp_string = escaped_pattern.gsub( EXPRESSION ) do |expansion|
        post_expansion = $'

        expansions << expansion
        _, operator, varlist = *expansion.match(EXPRESSION)
        leader = Regexp.escape(LEADERS.fetch(operator, ''))
        joiner = Regexp.escape(JOINERS.fetch(operator, ','))
        varspecs = varlist.split(',')
        # For multi-variable +/# expressions with no explode modifier, use atomic
        # joiners and remove outer optionality on non-last variables to prevent
        # O(2^k) backtracking from the combination of optional variables and
        # optional separators.
        has_explode = varspecs.any? { |vs| vs.match(VARSPEC)&.captures&.last == '*' }
        atomic_multi = varspecs.size > 1 &&
                       (operator == '+' || operator == '#') &&
                       !has_explode
        joiner_sep = atomic_multi ? "(?>#{joiner}?)" : "#{joiner}?"
        combined = varspecs.map do |varspec|
          _, name, modifier = *varspec.match(VARSPEC)

          result = processor && processor.respond_to?(:match) ? processor.match(name) : nil
          if result
            "(?<#{name}>#{ result })"
          else
            group = case operator
            when '+'
              "#{ RESERVED }*?"
            when '#'
              "#{ RESERVED }*?"
            when '/'
              "#{ UNRESERVED }*?"
            when '.'
              "#{ UNRESERVED.gsub('\.', '') }*?"
            when ';'
              "#{ UNRESERVED }*=?#{ UNRESERVED }*?"
            when '?'
              "#{ UNRESERVED }*=#{ UNRESERVED }*?"
            when '&'
              "#{ UNRESERVED }*=#{ UNRESERVED }*?"
            else
              "#{ UNRESERVED }*?"
            end
            if modifier == '*'
              seg = case operator
                    when '+', '#' then "(?>#{RESERVED_NO_COMMA}*)"
                    else group
                    end
              joiner_pattern = operator.nil? ? joiner : "#{joiner}?"
              "(?<#{name}>#{seg}(?:#{joiner_pattern}#{seg})*)?"
            elsif atomic_multi && varspec != varspecs.last
              "(?<#{name}>(?>#{RESERVED_NO_COMMA}*))"
            elsif operator == '+' && varspecs.size == 1 &&
                  post_expansion.start_with?('{+')
              "(?<#{name}>(?>#{RESERVED}*))?"
            else
              "(?<#{name}>#{group})?"
            end
          end
        end.join(joiner_sep)
        "(?:|#{leader}#{combined})"
      end

      # Ensure that the regular expression matches the whole URI.
      regexp_string = "\\A#{regexp_string}\\z"
      return expansions, Regexp.new(regexp_string)
    end

  end
end
