# frozen_string_literal: true

class CSV
  # Note: Don't use this class directly. This is an internal class.
  class FieldsConverter
    include Enumerable

    NO_QUOTED_FIELDS = [] # :nodoc:
    def NO_QUOTED_FIELDS.[](_index)
      false
    end
    NO_QUOTED_FIELDS.freeze

    #
    # A CSV::FieldsConverter is a data structure for storing the
    # fields converter properties to be passed as a parameter
    # when parsing a new file (e.g. CSV::Parser.new(@io, parser_options))
    #

    ERROR_MODES = [:raise, :collect].freeze # :nodoc:

    def initialize(options={})
      @converters = []
      @nil_value = options[:nil_value]
      @empty_value = options[:empty_value]
      @empty_value_is_empty_string = (@empty_value == "")
      @accept_nil = options[:accept_nil]
      @builtin_converters_name = options[:builtin_converters_name]
      @column_error_mode = options[:column_conversion_errors] || :raise
      unless ERROR_MODES.include?(@column_error_mode)
        raise ArgumentError,
              ":column_conversion_errors must be :raise or :collect: " \
              "#{@column_error_mode.inspect}"
      end
      @column_plans = build_column_plans(options[:column_converters])
      @column_record_count = 0
      @column_errors = []
      @headers_bound = false
      @no_headers_bound = false
      @column_chains = {}
      @column_resolved_plans = {}
      @need_static_convert = need_static_convert?
    end

    def add_converter(name=nil, &converter)
      if name.nil?  # custom converter
        @converters << converter
      else          # named converter
        combo = builtin_converters[name]
        case combo
        when Array  # combo converter
          combo.each do |sub_name|
            add_converter(sub_name)
          end
        else        # individual named converter
          @converters << combo
        end
      end
    end

    def each(&block)
      @converters.each(&block)
    end

    def empty?
      @converters.empty?
    end

    # Failures gathered from the most recent #convert call, available when
    # the :column_conversion_errors option is :collect.
    attr_reader :column_errors

    # Resolves name/occurrence selectors as soon as the header row is known.
    # Index selectors that depend on the row width are resolved lazily on
    # the first data row.
    def bind_headers(headers)
      return if @column_plans.empty?
      # Name/occurrence selectors must be checked as soon as the header row
      # is read.  Index selectors needing the row width are deferred to the
      # first data row.
      return if headers ? @headers_bound : @no_headers_bound
      width = headers&.size
      @column_plans.each do |plan|
        next if @column_resolved_plans.key?(plan)
        next if width.nil? and plan.index_selector?
        bind_plan(plan, plan.resolve(headers, width))
      end
      headers ? @headers_bound = true : @no_headers_bound = true
    end

    def convert(fields, headers, lineno, quoted_fields=NO_QUOTED_FIELDS)
      return fields unless need_convert?

      @column_record_count += 1 unless @column_plans.empty?
      prepare_column_conversion(fields, headers)
      @column_errors = []

      fields.collect.with_index do |field, index|
        original_field = field
        if field.nil?
          field = @nil_value
        elsif field.is_a?(String) and field.empty?
          field = @empty_value unless @empty_value_is_empty_string
        end
        @converters.each do |converter|
          break if field.nil? and @accept_nil
          field = call_converter(converter, field, index, lineno,
                                 headers, quoted_fields)
          break unless field.is_a?(String)  # short-circuit pipeline for speed
        end
        chain = @column_chains[index]
        field = apply_column_chain(chain, original_field, field, index,
                                   lineno, headers, quoted_fields) if chain
        field  # final state of each field, converted or original
      end
    end

    private
    def build_column_plans(column_converters)
      return [] if column_converters.nil?
      unless column_converters.is_a?(Hash)
        raise ArgumentError,
              ":column_converters must be a Hash of column selectors to " \
              "converter chains: #{column_converters.inspect}"
      end
      column_converters.collect do |selector, converters|
        ColumnConverterPlan.normalize(selector, converters,
                                      builtin_converters)
      end
    end

    def prepare_column_conversion(fields, headers)
      return if @column_plans.empty?
      bind_headers(headers) unless (headers ? @headers_bound : @no_headers_bound)
      width = headers&.size || fields.size
      @column_plans.each do |plan|
        next if @column_resolved_plans.key?(plan)
        bind_plan(plan, plan.resolve(headers, width))
      end
    end

    def bind_plan(plan, indexes)
      @column_resolved_plans[plan] = indexes
      indexes.each do |index|
        previous = @column_chains[index]
        unless previous.nil?
          previous_plan = @column_resolved_plans.find do |candidate, candidate_indexes|
            candidate_indexes.include?(index)
          end&.first
          first_selector = previous_plan&.selector
          raise DuplicateColumnError,
                "column at index #{index} is configured more than once: " \
                "#{first_selector.inspect} and #{plan.selector.inspect}"
        end
        @column_chains[index] = plan.chain
      end
    end

    def apply_column_chain(chain, original_field, field, index, lineno,
                           headers, quoted_fields)
      chain.each_with_index do |converter_spec, step|
        converter = converter_spec.is_a?(Array) ? converter_spec[1] : converter_spec
        break if field.nil? and @accept_nil
        field = call_converter(converter, field, index, lineno,
                               headers, quoted_fields)
        break unless field.is_a?(String)  # short-circuit pipeline for speed
      rescue StandardError
        failure = build_column_failure(chain, converter_spec, step,
                                       original_field, index,
                                       lineno, headers)
        if @column_error_mode == :collect
          @column_errors << failure
          return original_field
        else
          raise ColumnConversionError.new(failure)
        end
      end
      field
    end

    def call_converter(converter, field, index, lineno, headers,
                       quoted_fields)
      if converter.arity == 1  # straight field converter
        converter[field]
      else                     # FieldInfo converter
        header = headers ? headers[index] : nil
        quoted = quoted_fields[index]
        converter[field, FieldInfo.new(index, lineno, header, quoted)]
      end
    end

    def build_column_failure(chain, converter_spec, step, original_field,
                             index, lineno, headers)
      converter_name =
        if converter_spec.is_a?(Array)
          converter_spec[0]
        else
          converter_spec
        end
      plan = @column_plans.find do |candidate|
        candidate.chain.equal?(chain)
      end
      ColumnConversionFailure.new(
        selector: plan&.selector,
        index: index,
        header: headers ? headers[index] : nil,
        occurrence: selector_occurrence(plan),
        lineno: lineno,
        record: @column_record_count,
        field: original_field,
        step: step,
        converter: converter_name,
        cause: $!)
    end

    def selector_occurrence(plan)
      return nil if plan.nil?
      selector = plan.selector
      if selector.is_a?(Array)
        selector[1]
      elsif selector.is_a?(Hash)
        selector[:occurrence]
      end
    end

    def need_static_convert?
      not (@nil_value.nil? and @empty_value_is_empty_string)
    end

    def need_convert?
      @need_static_convert or
        (not @converters.empty?) or
        (not @column_plans.empty?)
    end

    def builtin_converters
      @builtin_converters ||= ::CSV.const_get(@builtin_converters_name)
    end
  end
end
