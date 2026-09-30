# frozen_string_literal: true

class CSV
  # Base error for declarative column conversion problems.
  class ColumnConverterError < StandardError
  end

  # Raised when a configured column selector doesn't match the header row.
  class MissingColumnError < ColumnConverterError
  end

  # Raised when a header-name selector matches more than one column and no
  # occurrence disambiguates it.
  class AmbiguousColumnError < ColumnConverterError
  end

  # Raised when two column selectors resolve to the same column.
  class DuplicateColumnError < ColumnConverterError
  end

  # Raised (in the default +:raise+ error mode) when a column conversion step
  # raises.  See CSV::ColumnConversionFailure for the details.
  class ColumnConversionError < ColumnConverterError
    attr_reader :failure

    def initialize(failure)
      @failure = failure
      super(failure.message)
    end
  end

  # Details of a single failed column conversion.
  #
  # <b><tt>selector</tt></b>:: The selector used in the conversion plan.
  # <b><tt>index</tt></b>:: The 0-based field index of the failed cell.
  # <b><tt>header</tt></b>:: The header of the column, when available.
  # <b><tt>occurrence</tt></b>:: The 1-based occurrence used by the selector,
  #                              when applicable.
  # <b><tt>lineno</tt></b>:: The 1-based physical line number.
  # <b><tt>record</tt></b>:: The 1-based data record number (excluding the
  #                          header row).
  # <b><tt>field</tt></b>:: The original field value before any conversion.
  # <b><tt>step</tt></b>:: The failing step's position in the conversion
  #                        chain (0-based).
  # <b><tt>converter</tt></b>:: The failing step's converter name or Proc.
  # <b><tt>cause</tt></b>:: The exception raised by the converter.
  ColumnConversionFailure = Struct.new(:selector,
                                       :index,
                                       :header,
                                       :occurrence,
                                       :lineno,
                                       :record,
                                       :field,
                                       :step,
                                       :converter,
                                       :cause,
                                       keyword_init: true) do
    def message
      location = "line #{lineno}, record #{record}, column #{index + 1}"
      location += " (#{column_identity})" unless column_identity.nil?
      "column conversion failed at step #{step} " \
        "(#{converter.inspect}) for #{location}: #{field.inspect}: " \
        "#{cause.class}: #{cause.message}"
    end

    def column_identity
      if selector.is_a?(Array)
        selector
      elsif selector.is_a?(String) or selector.is_a?(Symbol)
        selector
      elsif selector.is_a?(Hash) and selector.key?(:header)
        name, occurrence = selector_name_and_occurrence
        occurrence ? [name, occurrence] : name
      elsif occurrence.nil?
        header
      else
        [header, occurrence]
      end
    end

    private
    def selector_name_and_occurrence
      if selector.is_a?(Hash)
        [selector[:header], selector[:occurrence]]
      else
        [selector, nil]
      end
    end
  end

  # Note: Don't use this class directly. This is an internal class.
  #
  # A CSV::ColumnConverterPlan stores one declarative column conversion
  # configuration: how to select the column and which converter chain to
  # apply to it.
  class ColumnConverterPlan
    class << self
      # Normalizes a single (selector, converters) pair from the
      # +:column_converters+ option.
      def normalize(selector, converters, builtin_converters)
        normalized_selector = normalize_selector(selector)
        chain = normalize_chain(converters, builtin_converters)
        if chain.empty?
          raise ArgumentError,
                "column converter chain must not be empty: " \
                "#{selector.inspect}"
        end
        new(normalized_selector, chain)
      end

      private
      def normalize_selector(selector)
        case selector
        when Integer, String, Symbol
          selector
        when Range
          validate_range(selector)
          selector
        when Array
          normalize_array_selector(selector)
        when Hash
          normalize_hash_selector(selector)
        else
          raise ArgumentError,
                "invalid column selector: #{selector.inspect}"
        end
      end

      def validate_range(selector)
        unless selector.begin.is_a?(Integer) and selector.end.is_a?(Integer)
          raise ArgumentError,
                "column selector range must use integer endpoints: " \
                "#{selector.inspect}"
        end
      end

      def normalize_array_selector(selector)
        unless selector.size == 2
          raise ArgumentError,
                "column selector array must be [header, occurrence]: " \
                "#{selector.inspect}"
        end
        name, occurrence = selector
        unless name.is_a?(String) or name.is_a?(Symbol)
          raise ArgumentError,
                "column selector name must be a String or a Symbol: " \
                "#{selector.inspect}"
        end
        occurrence = validate_occurrence(occurrence, selector)
        [name, occurrence]
      end

      def normalize_hash_selector(selector)
        if selector.key?(:index)
          index = selector[:index]
          unless index.is_a?(Integer) or index.is_a?(Range)
            raise ArgumentError,
                  "column selector :index must be an Integer or a Range: " \
                  "#{selector.inspect}"
          end
          {index: index}
        elsif selector.key?(:header)
          name = selector[:header]
          unless name.is_a?(String) or name.is_a?(Symbol)
            raise ArgumentError,
                  "column selector :header must be a String or a Symbol: " \
                  "#{selector.inspect}"
          end
          normalized = {header: name}
          normalized[:occurrence] =
            validate_occurrence(selector[:occurrence], selector) if
            selector.key?(:occurrence)
          normalized
        else
          raise ArgumentError,
                "column selector must include :index or :header: " \
                "#{selector.inspect}"
        end
      end

      def validate_occurrence(occurrence, selector)
        unless occurrence.is_a?(Integer) and occurrence >= 1
          raise ArgumentError,
                "column selector occurrence must be a 1-based Integer: " \
                "#{selector.inspect}"
        end
        occurrence
      end

      def normalize_chain(converters, builtin_converters)
        chain = []
        append_chain(chain, converters, builtin_converters)
        chain
      end

      def append_chain(chain, converters, builtin_converters)
        if converters.is_a?(Array)
          converters.each do |converter|
            append_chain(chain, converter, builtin_converters)
          end
          return
        end

        case converters
        when Proc
          chain << converters
        when Symbol, String
          name = converters.to_sym
          combo = builtin_converters[name]
          if combo.nil?
            raise ArgumentError,
                  "unknown converter: #{converters.inspect}"
          elsif combo.is_a?(Array)
            combo.each do |sub_name|
              sub_converter = builtin_converters[sub_name]
              if sub_converter.is_a?(Array)
                raise "unexpected nested combo converter: #{sub_name}"
              end
              chain << [sub_name, sub_converter]
            end
          else
            chain << [name, combo]
          end
        when nil
          raise ArgumentError, "column converter must not be nil"
        else
          raise ArgumentError,
                "column converter must be a Proc, a converter name, or an " \
                "Array of them: #{converters.inspect}"
        end
      end
    end

    attr_reader :selector, :chain

    def initialize(selector, chain)
      @selector = selector
      @chain = chain
    end

    # Resolves this selector to concrete column indexes.
    #
    # +headers+ is the converted header row (nil when headers aren't used);
    # +width+ is the known column count, when known up front.  Returns nil
    # when the selector needs the first data row's width and resolution must
    # be deferred.
    def resolve(headers, width=nil)
      case @selector
      when Integer
        resolve_index(@selector, headers, width)
      when Range
        return nil if width.nil?
        resolve_range(@selector, width)
      when String, Symbol
        resolve_name(@selector, nil, headers)
      when Array
        name, occurrence = @selector
        resolve_name(name, occurrence, headers)
      when Hash
        if @selector.key?(:index)
          index = @selector[:index]
          if index.is_a?(Range)
            return nil if width.nil?
            resolve_range(index, width)
          else
            resolve_index(index, headers, width)
          end
        else
          resolve_name(@selector[:header],
                        @selector[:occurrence],
                        headers)
        end
      end
    end

    # True when the selector addresses columns by position and therefore
    # needs a known column count when no header row is available.
    def index_selector?
      if @selector.is_a?(Integer) or @selector.is_a?(Range)
        true
      elsif @selector.is_a?(Hash)
        @selector.key?(:index)
      else
        false
      end
    end

    private
    def resolve_index(index, headers, width)
      if index < 0
        return nil if width.nil?
        resolved = width + index
      else
        resolved = index
      end
      limit = width
      if limit and (resolved < 0 or resolved >= limit)
        raise MissingColumnError,
              "column not found: #{describe_selector} (column index " \
              "#{index} is out of range for #{limit} columns)"
      else
        [resolved]
      end
    end

    def resolve_range(range, limit)
      indexes = range.to_a
      indexes = indexes.collect do |index|
        index < 0 ? limit + index : index
      end
      if indexes.any? {|index| index < 0 or index >= limit}
        raise MissingColumnError,
              "column not found: #{describe_selector} (out of range for " \
              "#{limit} columns)"
      end
      indexes
    end

    def resolve_name(name, occurrence, headers)
      if headers.nil?
        raise MissingColumnError,
              "column not found: #{describe_selector} (no headers are " \
              "available; select the column by index instead)"
      end

      matches = []
      headers.each_with_index do |header, index|
        matches << index if header == name
      end

      if occurrence
        index = matches[occurrence - 1]
        if index.nil?
          raise MissingColumnError,
                "column not found: #{describe_selector} (header " \
                "#{name.inspect} occurs #{matches.size} time(s))"
        end
        [index]
      elsif matches.size == 1
        matches
      elsif matches.empty?
        raise MissingColumnError,
              "column not found: #{describe_selector}"
      else
        raise AmbiguousColumnError,
              "column #{name.inspect} is ambiguous; it occurs at " \
              "indexes #{matches.inspect}. Disambiguate with an " \
              "occurrence (e.g. #{[name, 2].inspect})."
      end
    end

    def describe_selector
      @selector.inspect
    end
  end
end
