# frozen_string_literal: true

require 'i18n/message_pattern/syntax_error'
require 'i18n/message_pattern/parser'

module I18n
  # An explicitly constructed, ICU-MessageFormat-inspired message pattern.
  #
  # Message patterns are an *opt-in* translation type: plain translation
  # strings are never parsed as patterns. To use one, wrap the pattern
  # string and store it as translation data, and include
  # I18n::Backend::MessagePattern into your backend:
  #
  #   I18n::Backend::Simple.include(I18n::Backend::MessagePattern)
  #   I18n.backend.store_translations(:en, :inbox => I18n::MessagePattern.new(
  #     '{count, plural, =0{No messages} one{One message} other{# messages}}'
  #   ))
  #   I18n.t(:inbox, :count => 3) # => "3 messages"
  #
  # == Syntax
  #
  #   literal text                  rendered as-is
  #   {name}                        inserts the value of +name+
  #   {name, select, a{..} b{..} other{..}}
  #                                 picks the branch whose selector matches the
  #                                 value of +name+; falls back to +other+
  #   {name, plural, one{..} other{..}}
  #                                 picks a branch by the CLDR plural category
  #                                 of the numeric value of +name+
  #   {name, plural, =0{..} other{..}}
  #                                 '=N' branches match the exact number first
  #   #                             inside a plural branch: the number itself
  #
  # Branch bodies are full messages: selects and plurals nest arbitrarily.
  # Whitespace around structural tokens (braces, commas, selectors) is
  # ignored; whitespace inside literal text is significant.
  #
  # == Escaping
  #
  # A single quote starts a quoted literal only when immediately followed by
  # '{', '}' or '#'; the quote runs until the next single quote (or the end
  # of the current text). Two single quotes ('') always produce one literal
  # apostrophe. A single quote followed by any other character is literal:
  #
  #   '{'count'}'   => "{count}"     (literal braces)
  #   it''s         => "it's"        (literal apostrophe)
  #   don't         => "don't"       (apostrophe before 't' is literal)
  #
  # == Evaluation
  #
  # Only the selected branch is evaluated: interpolation values that respond
  # to #call are invoked lazily, so callables referenced solely by
  # unselected branches are never called.
  #
  # Parsed patterns are immutable and safe to share. Use .compile to get a
  # cached instance for a repeated pattern string.
  class MessagePattern
    DEFAULT_PLURALIZER = lambda { |count| count == 1 ? :one : :other }

    @cache = Concurrent::Map.new

    class << self
      # Returns a cached MessagePattern for the given pattern string.
      def compile(pattern)
        @cache.compute_if_absent(pattern) { new(pattern) }
      end

      # Empties the pattern cache used by .compile.
      def clear_cache!
        @cache.clear
      end
    end

    attr_reader :source

    def initialize(pattern)
      pattern = pattern.to_str
      @source = pattern
      @ast = deep_freeze(Parser.parse(pattern))
      freeze
    end

    # Formats the pattern with the given values.
    #
    # +values+ is a Hash with Symbol or String keys. Values that respond to
    # #call are called (with +values+) only when the branch referencing them
    # is actually rendered.
    #
    # +pluralizer+ is an optional callable mapping a number to a CLDR plural
    # category (:zero, :one, :two, :few, :many, :other). Defaults to English
    # rules. I18n::Backend::MessagePattern passes the backend's per-locale
    # pluralizer when the Pluralization module is included.
    def format(values = EMPTY_HASH, pluralizer: nil, **extra_values)
      raise ArgumentError.new('Message pattern values must be a Hash.') unless values.is_a?(Hash)
      values = values.merge(extra_values) unless extra_values.empty?
      evaluate(@ast, values, pluralizer || DEFAULT_PLURALIZER, nil, +'')
    end

    private

    def evaluate(nodes, values, pluralizer, number, output)
      nodes.each do |node|
        case node[0]
        when :text
          output << node[1]
        when :number
          output << number.to_s
        when :arg
          output << resolve_value(values, node[1]).to_s
        when :select
          key = resolve_value(values, node[1])
          key = key.to_sym if key.respond_to?(:to_sym)
          evaluate(node[2][key] || node[2][:other], values, pluralizer, number, output)
        when :plural
          count = resolve_value(values, node[1])
          branches = node[2]
          branch = branches[count]
          branch ||= branches[count.to_i] if count.is_a?(Numeric)
          category = pluralizer.call(count)
          category = category.to_sym if category.respond_to?(:to_sym)
          branch ||= branches[category] || branches[:other]
          evaluate(branch, values, pluralizer, count, output)
        end
      end
      output
    end

    def resolve_value(values, name)
      key = name.to_sym
      if values.key?(key)
        value = values[key]
      elsif values.key?(name)
        value = values[name]
      else
        raise MissingInterpolationArgument.new(key, values, @source)
      end
      value.respond_to?(:call) ? value.call(values) : value
    end

    def deep_freeze(node)
      case node
      when Array
        node.each { |element| deep_freeze(element) }.freeze
      when Hash
        node.each_value { |value| deep_freeze(value) }.freeze
      else
        node.freeze
      end
    end
  end
end
