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

    class ReverseExpansionResult
      NO_MATCH = :no_match
      UNIQUE = :unique
      AMBIGUOUS = :ambiguous

      def initialize(status, binding = nil)
        @status = status
        @binding = binding && binding.dup.freeze
      end

      attr_reader :status, :binding
      alias_method :mapping, :binding

      def no_match?
        status == NO_MATCH
      end

      def unique?
        status == UNIQUE
      end

      def ambiguous?
        status == AMBIGUOUS
      end

      def inspect
        if binding
          sprintf(
            "#<%s:%#0x %s %s>",
            self.class, object_id, status, binding.inspect
          )
        else
          sprintf("#<%s:%#0x %s>", self.class, object_id, status)
        end
      end
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

    # Reverses an expansion using explicit variable shape declarations.
    #
    # @param [Addressable::URI, #to_str] uri
    #   The URI to extract from.
    # @param [Hash] shapes
    #   A mapping from variable names to +:string+, +:list+, +:hash+, or an
    #   Array containing several of those values.
    #
    # @return [Addressable::Template::ReverseExpansionResult]
    #   A result whose status is +:no_match+, +:unique+, or +:ambiguous+.
    #   A unique result's binding can be expanded back to +uri+.
    #
    # @example
    #   template = Addressable::Template.new("/{x}{/y*}")
    #   result = template.reverse_expand("/a/b/c", "x" => :string, "y" => :list)
    #   result.status
    #   #=> :unique
    #   result.binding
    #   #=> {"x" => "a", "y" => ["b", "c"]}
    def reverse_expand(uri, shapes={})
      ReverseExpander.new(self, shapes).expand(uri)
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
        unless value == nil ||
          (Hash === value && value.empty?) ||
          (value.respond_to?(:to_ary) && value.to_ary.empty?)
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
          encoded_pairs = value.kind_of?(Hash)
          acc << [name, transformed_value, encoded_pairs]
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
        leader + return_value.map{|k,v,pairs|
          if pairs && v.is_a?(Array)
            v.join(joiner)
          elsif v.is_a?(Array)
            v.map{|inner_value| "#{k}=#{inner_value}"}.join(joiner)
          else
            "#{k}=#{v}"
          end
        }.join(joiner)
      when ';'
        return_value.map{|k,v,pairs|
          if pairs && v.is_a?(Array)
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

    class ReverseExpander
      UNRESERVED = Addressable::URI::CharacterClasses::UNRESERVED
      RESERVED =
        Addressable::URI::CharacterClasses::RESERVED +
        Addressable::URI::CharacterClasses::UNRESERVED
      TOKEN = /(?:%[a-fA-F0-9]{2}|[^%]|%(?![a-fA-F0-9]{2}))/

      def initialize(template, shapes)
        @template = template
        @shapes = normalize_shapes(shapes)
      end

      def expand(uri)
        uri = Addressable::URI.parse(uri) unless uri.is_a?(Addressable::URI)
        @target = uri.to_str.dup.force_encoding(Encoding::UTF_8)
        states = [fresh_state]

        template_segments.each_with_index do |(kind, payload), index|
          states =
            if kind == :literal
              advance_literal(states, payload)
            else
              advance_expression(states, payload, template_segments, index)
            end
          states.uniq!
          break if states.empty?
        end

        binding_states = states.select do |state|
          state[:position] == @target.length &&
            state[:prefixes].empty?
        end
        prefix_states = states.select do |state|
          state[:position] == @target.length &&
            !state[:prefixes].empty?
        end.select do |state|
          state[:prefixes].keys.all? do |name|
            variable_occurrences[name] == 1
          end
        end
        bindings = valid_bindings(binding_states)
        has_prefix_match = bindings.empty? && !prefix_states.empty?
        encoding_ambiguous = binding_states.any? do |state|
          state[:encoding_ambiguous]
        end
        status, binding =
          if bindings.length > 1
            ReverseExpansionResult::AMBIGUOUS
          elsif bindings.length == 1 && !has_prefix_match &&
              !encoding_ambiguous
            [ReverseExpansionResult::UNIQUE, bindings.first]
          elsif bindings.length == 1 || has_prefix_match ||
              encoding_ambiguous
            ReverseExpansionResult::AMBIGUOUS
          else
            ReverseExpansionResult::NO_MATCH
          end
        ReverseExpansionResult.new(status, binding)
      end

      private

      def normalize_shapes(shapes)
        unless shapes.respond_to?(:each_pair)
          raise TypeError, "Can't convert #{shapes.class} into Hash."
        end

        normalized = {}
        shapes.each_pair do |name, shape|
          name = name.to_s
          unless @template.variables.include?(name)
            raise ArgumentError, "Unknown variable: #{name}"
          end
          normalized[name] = Array(shape).map do |candidate|
            candidate = candidate.to_s
            unless %w[string list hash].include?(candidate)
              raise ArgumentError, "Unknown shape: #{candidate}"
            end
            candidate
          end.uniq
        end
        allowed_shapes.each do |name, allowed|
          normalized[name] ||= ["string"]
          compatible = normalized[name] & allowed
          if compatible.empty?
            raise ArgumentError,
              "Shape #{normalized[name].inspect} is incompatible with #{name}."
          end
          normalized[name] = compatible
        end
        normalized
      end

      def allowed_shapes
        allowed = {}
        @template.pattern.scan(EXPRESSION) do
          _, varlist = Regexp.last_match.captures
          varlist.split(",").each do |varspec|
            name, modifier = varspec.match(VARSPEC).captures
            shapes =
              if modifier =~ /\A:\d+\z/
                ["string"]
              elsif modifier == "*"
                %w[list hash]
              else
                %w[string list hash]
              end
            allowed[name] =
              if allowed.key?(name)
                allowed[name] & shapes
              else
                shapes
              end
          end
        end
        allowed
      end

      def variable_occurrences
        @variable_occurrences ||= begin
          counts = Hash.new(0)
          @template.pattern.scan(EXPRESSION) do
            _, varlist = Regexp.last_match.captures
            varlist.split(",").each do |varspec|
              name, = varspec.match(VARSPEC).captures
              counts[name] += 1
            end
          end
          counts
        end
      end

      def template_segments
        @template_segments ||= begin
          segments = []
          position = 0
          @template.pattern.scan(EXPRESSION) do
            match = Regexp.last_match
            if match.begin(0) > position
              segments << [
                :literal, @template.pattern[position...match.begin(0)]
              ]
            end
            operator, varlist = match.captures
            vars = varlist.split(",").map do |varspec|
              name, modifier = varspec.match(VARSPEC).captures
              {
                :name => name,
                :explode => modifier == "*",
                :prefix => modifier =~ /\A:(\d+)\z/ ? $1.to_i : nil
              }
            end
            segments << [
              :expression, { :operator => operator, :vars => vars }
            ]
            position = match.end(0)
          end
          segments << [
            :literal, @template.pattern[position..-1]
          ] if position < @template.pattern.length
          segments
        end
      end

      def advance_literal(states, literal)
        states.select do |state|
          @target[state[:position], literal.length] == literal
        end.map do |state|
          state.merge(:position => state[:position] + literal.length)
        end
      end

      def advance_expression(states, expression, segments, index)
        operator = expression[:operator]
        states.flat_map do |state|
          stops = expression_stops(
            segments, index, state[:position], expression[:operator]
          )
          stops.flat_map do |stop|
            if %w[; ? &].include?(operator)
              parse_form(expression, state, stop)
            else
              parse_path(expression, state, stop)
            end
          end
        end
      end

      def expression_stops(segments, index, position, operator)
        next_segment = segments[index + 1]
        return [@target.length] unless next_segment
        if next_segment.first == :literal
          literal = next_segment.last
          found = @target.index(literal, position)
          found ? [found] : []
        elsif next_segment.first == :expression
          next_operator = next_segment.last[:operator]
          next_leader = LEADERS.fetch(next_operator, "")
          if !next_leader.empty?
            stops = []
            while (found = @target.index(next_leader, position))
              stops << found
              position = found + next_leader.length
            end
            stops
          else
            [position, @target.length].uniq
          end
        else
          expression_stops(segments, index + 1, position, operator)
        end
      end

      def parse_path(expression, state, stop)
        results = []
        parse_path_vars(
          expression[:operator], expression[:vars], 0,
          state[:position], state, stop
        ) { |result| results << result }
        results
      end

      def parse_path_vars(operator, vars, index, position, state, stop)
        if index >= vars.length
          if position == stop
            state[:position] = position
            yield state
          end
          return
        end

        if index > 0 || stop == @target.length ||
            !LEADERS.fetch(operator, "").empty?
          parse_path_vars(
            operator, vars, index + 1, position, copy_state(state), stop
          ) { |result| yield result }
        end

        starts = path_starts(operator, index, position)
        starts.each do |start|
          parse_path_value(operator, vars, index, start, stop, state) do |result|
            yield result
          end
        end
      end

      def path_starts(operator, index, position)
        if index.zero?
          leader = LEADERS.fetch(operator, "")
          if leader.empty?
            [position]
          elsif @target[position, leader.length] == leader
            [position + leader.length]
          else
            []
          end
        else
          joiner = JOINERS.fetch(operator, ",")
          starts = []
          if @target[position, joiner.length] == joiner
            starts << position + joiner.length
          end
          if operator == "+" || operator == "#"
            starts << position
          end
          starts
        end
      end

      def parse_path_value(operator, vars, index, start, stop, state)
        var = vars[index]
        separator = var[:explode] ? JOINERS.fetch(operator, ",") : ","
        endings = variable_endings(start, stop, separator)
        endings.each do |ending|
          body = @target[start...ending]
          parse_body(var, operator, body) do |value, encoded_ambiguous|
            assigned = assign_variable(state, var, value)
            next unless assigned
            assigned[:encoding_ambiguous] = true if encoded_ambiguous
            parse_path_vars(
              operator, vars, index + 1, ending, assigned, stop
            ) { |result| yield result }
          end
        end
      end

      def variable_endings(start, stop, separator)
        endings = [start, stop]
        found = @target.index(separator, start)
        endings << found if found && found < stop
        endings.uniq
      end

      def each_path_ending(start, stop, separator)
        position = start
        while (found = @target.index(separator, position))
          break if found >= stop
          yield found
          position = found + separator.length
        end
        yield stop
      end

      def parse_form(expression, state, stop)
        results = []
        parse_form_vars(
          expression[:operator], expression[:vars], 0,
          state[:position], state, stop
        ) { |result| results << result }
        results
      end

      def parse_form_vars(operator, vars, index, position, state, stop)
        if index >= vars.length
          if position == stop
            state[:position] = position
            yield state
          end
          return
        end

        var = vars[index]
        parse_form_vars(
          operator, vars, index + 1, position, copy_state(state), stop
        ) { |result| yield result }

        leader = form_leader(operator, index)
        return unless @target[position, leader.length] == leader

        body_start = position + leader.length
        if var[:explode]
          parse_form_exploded(
            operator, vars, index, body_start, state, stop
          ) { |result| yield result }
          return
        end

        next_name = vars[index + 1] && vars[index + 1][:name]
        ending =
          if next_name
            found =
              if operator == ";"
                @target.index(";" + next_name, body_start)
              else
                @target.index("&" + next_name, body_start)
              end
            found && found <= stop ? found : stop
          else
            stop
          end
        segment = @target[body_start...ending]
        expected_name = var[:name]

        if operator == ";"
          if segment == expected_name
            value = ""
          elsif segment.start_with?(expected_name + "=")
            content = segment[(expected_name.length + 1)..-1]
            value = [:raw, content]
          else
              return
          end
        else
          return unless segment.start_with?(expected_name + "=")
          content = segment[(expected_name.length + 1)..-1]
          value = [:raw, content]
        end

        parse_scalar_form_value(var, operator, value) do |parsed, ambiguous|
          assigned = assign_variable(state, var, parsed)
          next unless assigned
          assigned[:encoding_ambiguous] = true if ambiguous
          parse_form_vars(
            operator, vars, index + 1, ending, assigned, stop
          ) { |result| yield result }
        end
      end

      def parse_scalar_form_value(var, operator, value)
        shapes = compatible_shapes(var)
        shapes.each do |shape|
          if shape == "string"
            value = value[1] if value.is_a?(Array)
            decoded, ambiguous = decode_with_flag(value, false)
            next if decoded.nil?
            yield decoded, ambiguous
          elsif shape == "list"
            raw = value.is_a?(Array) ? value[1] : value
            parts = raw.empty? ? [] : split(raw, ",", false)
            yield parts, false if parts
          elsif shape == "hash"
            raw = value.is_a?(Array) ? value[1] : value
            parsed = parse_non_exploded_hash(raw, false)
            yield parsed, false if parsed
          end
        end
      end

      def form_leader(operator, index)
        if index.zero?
          LEADERS.fetch(operator, "")
        else
          JOINERS.fetch(operator, "&")
        end
      end

      def parse_form_exploded(operator, vars, index, start, state, stop)
        var = vars[index]
        joiner = JOINERS.fetch(operator, "&")
        each_form_segment(start, stop, joiner) do |parts, consumed|
          parse_form_composite(var, parts) do |value, ambiguous|
            assigned = assign_variable(state, var, value)
            next unless assigned
            assigned[:encoding_ambiguous] = true if ambiguous
            parse_form_vars(
              operator, vars, index + 1, consumed, assigned, stop
            ) { |result| yield result }
          end
        end
      end

      def each_form_segment(start, stop, joiner)
        starts = [start]
        starts.concat(indexes_between(start, stop, joiner).map do |position|
          position + joiner.length
        end)

        starts.each do |part_start|
          boundaries = indexes_between(part_start, stop, joiner)
          boundaries << stop
          boundaries.uniq.each do |segment_stop|
            parts = []
            position = part_start
            indexes_between(part_start, segment_stop, joiner).each do |boundary|
              parts << @target[position...boundary]
              position = boundary + joiner.length
            end
            parts << @target[position...segment_stop]
            consumed = segment_stop
            yield parts, consumed
          end
        end
      end

      def indexes_between(start, stop, separator)
        indexes = []
        position = start
        while (found = @target.index(separator, position)) && found < stop
          indexes << found
          position = found + separator.length
        end
        indexes
      end

      def parse_form_composite(var, parts)
        shapes = compatible_shapes(var)
        shapes.each do |shape|
          if shape == "list"
            value = parse_form_list_parts(var, parts)
            yield value, false
          elsif shape == "hash"
            value = parse_form_hash_parts(parts)
            yield value, false
          end
        end
      end

      def parse_form_list_parts(var, parts)
        prefix = var[:name] + "="
        values = []
        parts.each do |part|
          return nil unless part.start_with?(prefix)
          value = decode(part[prefix.length..-1], false)
          return nil if value.nil?
          values << value
        end
        values
      end

      def parse_form_hash_parts(parts)
        value = {}
        parts.each do |part|
          key, separator, val = part.partition("=")
          return nil if separator.empty?
          key = decode(key, false)
          val = decode(val, false)
          return nil if key.nil? || val.nil? || value.key?(key)
          value[key] = val
        end
        value.empty? ? nil : value
      end

      def parse_body(var, operator, body)
        shapes = compatible_shapes(var)
        shapes.each do |shape|
          parse_typed_body(shape, var[:explode], operator, body) do |value, ambiguous|
            yield value, ambiguous || false
          end
        end
      end

      def compatible_shapes(var)
        shapes = @shapes.fetch(var[:name])
        if var[:prefix]
          shapes & ["string"]
        elsif var[:explode]
          shapes & %w[list hash]
        else
          shapes
        end
      end

      def parse_typed_body(shape, explode, operator, body)
        reserved = operator == "+" || operator == "#"
        if shape == "string"
          value, ambiguous = decode_with_flag(body, reserved)
          yield value, ambiguous if value
        elsif shape == "list"
          separator = explode ? JOINERS.fetch(operator, ",") : ","
          parts = split(body, separator, reserved)
          yield parts, false if parts
        elsif shape == "hash"
          pair_separator = explode ? JOINERS.fetch(operator, ",") : ","
          if explode
            values = parse_exploded_hash(body, pair_separator, reserved)
            values.each do |value|
              yield value, values.length > 1
            end
          elsif reserved
            values = parse_reserved_non_exploded_hash(body)
            values.each do |value|
              yield value, values.length > 1
            end
          else
            value = parse_non_exploded_hash(body, false)
            yield value, false
          end
        end
      end

      def parse_exploded_hash(body, pair_separator, reserved)
        return [] if body.empty?
        parse_exploded_hash_values(body, pair_separator, reserved)
      end

      def parse_exploded_hash_values(body, separator, reserved)
        parse_exploded_hash_pairs(
          body, 0, separator, reserved, {}
        ).uniq.first(2)
      end

      def parse_exploded_hash_pairs(body, position, separator, reserved, seen)
        return [seen.dup] if position >= body.length

        equals = body.index("=", position)
        first_separator = body.index(separator, position)
        return [] if equals.nil?
        return [] if first_separator && equals > first_separator

        key = decode(body[position...equals], reserved)
        return [] if key.nil? || seen.key?(key)

        value_start = equals + 1
        boundaries = [value_start, body.length]
        search = value_start
        while (found = body.index(separator, search))
          boundaries << found
          search = found + separator.length
        end

        values = []
        boundaries.sort.uniq.each do |value_end|
          value = decode(body[value_start...value_end], reserved)
          next if value.nil?
          next_seen = seen.merge(key => value)
          if value_end == body.length
            values << next_seen
          elsif valid_exploded_hash_pair?(
            body, value_end + separator.length, separator, reserved
          )
            values.concat(
              parse_exploded_hash_pairs(
                body, value_end + separator.length,
                separator, reserved, next_seen
              )
            )
          end
        end
        values.uniq.first(2)
      end

      def valid_exploded_hash_pair?(body, position, separator, reserved)
        return false if position >= body.length
        equals = body.index("=", position)
        next_separator = body.index(separator, position)
        return false if equals.nil?
        return false if next_separator && equals > next_separator
        key = decode(body[position...equals], reserved)
        !key.nil?
      end

      def parse_non_exploded_hash(body, reserved)
        tokens = split(body, ",", reserved)
        return nil unless tokens && tokens.length.even? && !tokens.empty?
        value = {}
        tokens.each_slice(2) do |key, val|
          return nil if value.key?(key)
          value[key] = val
        end
        value
      end

      def parse_reserved_non_exploded_hash(body)
        values = parse_reserved_hash_pairs(body, 0, {}).uniq
        values.first(2)
      end

      def parse_reserved_hash_pairs(body, position, seen)
        return [seen.dup] if position >= body.length

        values = []
        key_end = body.index(",", position)
        key_end = body.length unless key_end
        key = decode(body[position...key_end], true)
        return values if key.nil? || seen.key?(key)

        if key_end == body.length
          next_seen = seen.merge(key => "")
          values.concat(parse_reserved_hash_pairs(
            body, body.length, next_seen
          ))
          return values.uniq.first(2)
        end

        value_start = key_end + 1
        value_end = value_start
        while value_end
          value = decode(body[value_start...value_end], true)
          next_seen = seen.merge(key => value)
          if value_end == body.length
            values.concat(parse_reserved_hash_pairs(
              body, body.length, next_seen
            ))
          elsif body[value_end] == ","
            values.concat(parse_reserved_hash_pairs(
              body, value_end + 1, next_seen
            ))
          end
          found = body.index(",", value_end + 1)
          break unless found
          value_end = found
        end
        values.uniq.first(2)
      end

      def decode(body, reserved)
        value, = decode_with_flag(body, reserved)
        value
      end

      def decode_with_flag(body, reserved)
        character_class = reserved ? RESERVED : UNRESERVED
        valid = true
        encoded_reserved = false
        body.scan(TOKEN) do
          token = Regexp.last_match[0]
          if token =~ /\A%[a-fA-F0-9]{2}\z/
            decoded = Addressable::URI.unencode_component(token)
            encoded_reserved = true if reserved &&
              decoded =~ /\A[#{Addressable::URI::CharacterClasses::RESERVED}]\z/
          elsif token !~ /\A[#{character_class}]\z/
            valid = false
          end
        end
        valid ?
          [Addressable::URI.unencode_component(body), encoded_reserved] :
          [nil, false]
      end

      def split(body, separator, reserved)
        return [""] if body.empty?
        positions = []
        body.scan(TOKEN) do
          token = Regexp.last_match[0]
          if token == separator
            positions << Regexp.last_match.begin(0)
          end
        end
        return [body] if positions.empty?

        parts = []
        start = 0
        positions.each do |boundary|
          parts << body[start...boundary]
          start = boundary + separator.length
        end
        parts << body[start..-1]
        decoded = parts.map { |part| decode(part, reserved) }
        decoded.any?(&:nil?) ? nil : decoded
      end

      def assign_variable(state, var, value)
        name = var[:name]
        candidate = copy_state(state)

        if state[:values].key?(name)
          return nil unless state[:values][name] == value
        elsif state[:prefixes].key?(name)
          return nil unless value.to_s.start_with?(state[:prefixes][name])
          candidate[:values][name] = value
        elsif var[:prefix] && value.to_s.length >= var[:prefix]
          candidate[:prefixes][name] = value.to_s[0, var[:prefix]]
        else
          candidate[:values][name] = value
        end
        candidate
      end

      def copy_state(state)
        {
          :values => state[:values].dup,
          :prefixes => state[:prefixes].dup,
          :encoding_ambiguous => state[:encoding_ambiguous],
          :position => state[:position]
        }
      end

      def fresh_state
        {
          :values => {},
          :prefixes => {},
          :encoding_ambiguous => false,
          :position => 0
        }
      end

      def valid_bindings(states)
        bindings = states.select { |state| state[:prefixes].empty? }.map do |state|
          binding = {}
          @template.variables.each do |name|
            binding[name] = state[:values][name]
          end
          binding
        end.uniq

        bindings.select do |binding|
          @template.expand(binding, nil, false).to_str == @target
        end
      end
    end

  end
end
