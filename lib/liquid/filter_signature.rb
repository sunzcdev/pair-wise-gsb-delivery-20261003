# frozen_string_literal: true

module Liquid
  # A FilterSignature describes the arguments a filter accepts, so that
  # Liquid can report mismatches as early as possible instead of waiting
  # for the filter to be invoked during rendering.
  #
  # Signatures are registered on an Environment and only apply to templates
  # parsed or rendered within that environment:
  #
  #   environment.register_filter_signature('money',
  #     positional: 0..1,
  #     keywords: { currency: 'USD' },
  #     aliases: ['fmt_money'],
  #   )
  #
  # Filters without a registered signature keep their historical behavior.
  class FilterSignature
    attr_reader :name, :positional, :keywords, :aliases

    def initialize(name, positional: 0..0, keywords: {}, aliases: [])
      @name = name.to_s
      @positional = normalize_positional(positional)
      @keywords = keywords.each_with_object({}) do |(key, default), result|
        result[key.to_s] = default
      end.freeze
      @aliases = aliases.map(&:to_s).freeze
    end

    def min_positional
      @positional.begin
    end

    def max_positional
      @positional.end
    end

    # Validates the given arguments against the signature.
    #
    # @param positional_count [Integer] the number of positional arguments
    #   (the filtered input itself is not counted).
    # @param keyword_keys [Array<#to_s>] the names of the keyword arguments given.
    # @param as [String] the filter name as written, which can be an alias of
    #   the registered name. Used for error attribution.
    # @param error_class [Class] the error class raised on mismatch.
    def validate!(positional_count, keyword_keys, as: @name, error_class: FilterSignatureError)
      unless valid_positional?(positional_count)
        raise error_class, positional_error_message(as, positional_count)
      end

      validate_keywords!(keyword_keys, as: as, error_class: error_class)
    end

    # Validates only the keyword argument names against the signature.
    def validate_keywords!(keyword_keys, as: @name, error_class: FilterSignatureError)
      unknown_keywords = keyword_keys.map(&:to_s) - @keywords.keys
      unless unknown_keywords.empty?
        raise error_class, unknown_keyword_message(as, unknown_keywords)
      end
      true
    end

    def valid_positional?(positional_count)
      @positional.cover?(positional_count)
    end

    def positional_error_message(as, positional_count)
      "wrong number of arguments for filter '#{as}' " \
        "(given #{positional_count}, expected #{describe_positional})"
    end

    # Returns keyword arguments with the registered defaults applied.
    # Explicitly provided values (including nil) always win over defaults.
    def apply_defaults(provided)
      return provided if @keywords.empty?

      @keywords.merge(provided)
    end

    # Returns a hash of all registered keyword defaults, or nil when the
    # signature declares no keywords.
    def default_keywords
      @keywords.empty? ? nil : @keywords.dup
    end

    private

    def normalize_positional(positional)
      range = case positional
      when ::Range
        positional
      when ::Integer
        positional..positional
      else
        raise ::ArgumentError, "positional must be an Integer or a Range, got #{positional.inspect}"
      end

      if range.begin.nil? || range.begin.negative?
        raise ::ArgumentError, "positional range must start at 0 or more, got #{range.inspect}"
      end

      if range.end && (range.end < range.begin || (range.exclude_end? && range.end == range.begin))
        raise ::ArgumentError, "positional range end must not be smaller than its start, got #{range.inspect}"
      end

      range
    end

    def describe_positional
      if @positional.end.nil?
        "#{@positional.begin} or more"
      elsif @positional.exclude_end?
        "#{@positional.begin}...#{@positional.end}"
      elsif @positional.begin == @positional.end
        @positional.begin.to_s
      else
        "#{@positional.begin}..#{@positional.end}"
      end
    end

    def unknown_keyword_message(as, unknown_keywords)
      given = unknown_keywords.map { |key| ":#{key}" }.join(', ')
      if @keywords.empty?
        "filter '#{as}' does not accept keyword arguments (given #{given})"
      else
        valid = @keywords.keys.map { |key| ":#{key}" }.join(', ')
        "unknown keyword#{'s' if unknown_keywords.size > 1} #{given} for filter '#{as}' " \
          "(valid keywords: #{valid})"
      end
    end
  end
end
