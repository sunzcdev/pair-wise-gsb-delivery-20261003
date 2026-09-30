# frozen_string_literal: true

class CSV
  # The error thrown when a CSV::ColumnConversionPlan does not match the
  # actual headers of the data: a selected column is missing, a header name
  # selector is ambiguous, or two plan entries resolve to the same column.
  # This is raised as soon as the header row has been read, before any data
  # row is converted.
  class InvalidColumnConversionPlanError < ArgumentError
  end

  # Describes a single conversion failure in a CSV::ColumnConversionPlan.
  #
  # It carries the record position (+lineno+), the column identity
  # (+column_index+, +header+ and +occurrence+), the +original_value+ before
  # the failing chain ran, and the failing chain link (+step+ and
  # +converter+). The original exception raised by the converter is
  # available as #cause.
  class ColumnConversionError < StandardError
    attr_reader :lineno, :column_index, :header, :occurrence,
                :original_value, :step, :converter, :cause

    def initialize(lineno:, column_index:, header:, occurrence:,
                   original_value:, step:, converter:, cause: nil)
      @lineno = lineno
      @column_index = column_index
      @header = header
      @occurrence = occurrence
      @original_value = original_value
      @step = step
      @converter = converter
      @cause = cause
      super(build_message)
    end

    private
    def build_message
      column = "column #{column_index}"
      if header
        column << " (#{header.inspect}"
        column << ", occurrence #{occurrence}" if occurrence
        column << ")"
      end
      message = "conversion failed for #{column} at step #{step}"
      message << " (#{converter.inspect})"
      message << " with value #{original_value.inspect}"
      message << " in line #{lineno}"
      message << ": #{cause.message}" if cause
      message
    end
  end

  #
  # A CSV::ColumnConversionPlan is a declarative description of per-column
  # field conversions.
  #
  # Columns are selected either by zero-based index or by header name plus
  # an occurrence number (one-based), which allows targeting a specific
  # column when several columns share the same header text:
  #
  #   plan = CSV::ColumnConversionPlan.new
  #   plan.convert(0, with: :integer)
  #   plan.convert("amount", occurrence: 2, with: [:strip, :float])
  #   plan.convert(["amount", 1], with: :integer)
  #
  # A plan can also be built from a single specification \Hash or \Array:
  #
  #   CSV::ColumnConversionPlan.new(
  #     0              => :integer,
  #     ["amount", 2]  => [:strip, :float],
  #   )
  #
  # Each column gets a chain of converters. Chain elements follow the same
  # conventions as regular field converters: a \Symbol names a converter in
  # CSV::Converters (combos are expanded), a callable of arity 1 receives
  # the field value, and a callable of any other arity receives the value
  # and a CSV::FieldInfo. A chain stops as soon as a link returns a
  # non-String value.
  #
  # The plan is validated against the actual headers as soon as the header
  # row has been read: missing columns, ambiguous name selectors and
  # overlapping entries raise CSV::InvalidColumnConversionPlanError before
  # any data row is processed.
  #
  # When a converter raises, the plan builds a CSV::ColumnConversionError
  # and handles it according to the +on_error+ mode:
  #
  # [:raise]   (default) the error is raised, aborting the parse.
  # [:collect] the error is appended to #failures and the field keeps its
  #            original value, so the caller can inspect the failures later.
  # [callable] the callable is invoked with the error; the field keeps its
  #            original value.
  #
  class ColumnConversionPlan
    include Enumerable

    # Failures collected while running with <tt>on_error: :collect</tt>.
    attr_reader :failures

    def initialize(spec=nil, on_error: :raise)
      unless [:raise, :collect].include?(on_error) or on_error.respond_to?(:call)
        raise ArgumentError,
              ":on_error must be :raise, :collect, or a callable"
      end
      @on_error = on_error
      @entries = []
      @failures = []
      @resolved = nil
      add(spec) if spec
    end

    # Adds entries from a specification: a \Hash mapping selectors to
    # converter chains, or an \Array of <tt>[selector, converters]</tt>
    # pairs.
    def add(spec)
      pairs = spec.is_a?(Hash) ? spec.to_a : spec
      pairs.each do |selector, converters|
        convert(selector, with: converters)
      end
      self
    end

    #
    # Declares a conversion chain for a single column.
    #
    # +selector+ is one of:
    #
    # - an \Integer zero-based column index;
    # - a \String header name (must be unique unless +occurrence+ is given);
    # - an <tt>[name, occurrence]</tt> \Array pair;
    # - a <tt>{name: ..., occurrence: ...}</tt> \Hash.
    #
    # +with+ is a single converter or an \Array of converters applied in
    # order.
    #
    def convert(selector, occurrence: nil, with:)
      @entries << [normalize_selector(selector, occurrence),
                   normalize_converters(with)]
      @resolved = nil
      self
    end

    def each(&block)
      @entries.each(&block)
    end

    def empty?
      @entries.empty?
    end

    #
    # Resolves every selector against +headers+ (which may be +nil+ when
    # the data has no headers) and validates the whole plan. Raises
    # CSV::InvalidColumnConversionPlanError for missing columns, ambiguous
    # name selectors and overlapping entries.
    #
    def bind!(headers)
      @headers = headers
      resolved = {}
      @entries.each do |selector, converters|
        index, occurrence = resolve_selector(selector, headers)
        if resolved.key?(index)
          raise InvalidColumnConversionPlanError,
                "overlapping conversion plans for " \
                "#{describe_column(index, headers)}"
        end
        resolved[index] = [converters, occurrence]
      end
      @resolved = resolved
      self
    end

    #
    # Applies the resolved conversion chains to +fields+, returning a new
    # \Array. Must be called after bind!.
    #
    def convert_fields(fields, lineno,
                       quoted_fields=FieldsConverter::NO_QUOTED_FIELDS)
      return fields if @resolved.nil? or @resolved.empty?

      converted = fields.dup
      @resolved.each do |index, (converters, occurrence)|
        next if index >= fields.size
        original = fields[index]
        value = original
        converters.each_with_index do |converter, step|
          begin
            if converter.arity == 1  # straight field converter
              value = converter[value]
            else                     # FieldInfo converter
              header = @headers ? @headers[index] : nil
              quoted = quoted_fields[index]
              value = converter[value, FieldInfo.new(index, lineno,
                                                     header, quoted)]
            end
          rescue StandardError => error
            failure = ColumnConversionError.new(
              lineno:         lineno,
              column_index:   index,
              header:         @headers ? @headers[index] : nil,
              occurrence:     occurrence,
              original_value: original,
              step:           step,
              converter:      converter,
              cause:          error,
            )
            handle_failure(failure)
            value = original
            break
          end
          break unless value.is_a?(String)  # short-circuit pipeline
        end
        converted[index] = value
      end
      converted
    end

    private
    def normalize_selector(selector, occurrence)
      case selector
      when Integer
        raise ArgumentError, ":occurrence requires a header name" if occurrence
        {type: :index, index: selector}
      when String
        {type: :name, name: selector, occurrence: occurrence}
      when Array
        name, array_occurrence = selector
        occurrence ||= array_occurrence
        unless name.is_a?(String) and occurrence.is_a?(Integer)
          raise ArgumentError,
                "array selectors must be [name, occurrence] pairs"
        end
        {type: :name, name: name, occurrence: occurrence}
      when Hash
        name = selector[:name]
        occurrence ||= selector[:occurrence]
        unless name.is_a?(String)
          raise ArgumentError, "hash selectors require a :name"
        end
        {type: :name, name: name, occurrence: occurrence}
      else
        raise ArgumentError, "unsupported column selector: #{selector.inspect}"
      end
    end

    def normalize_converters(converters)
      converters = [converters] unless converters.is_a?(Array)
      converters.flat_map {|converter| expand_converter(converter)}
    end

    def expand_converter(converter)
      case converter
      when Symbol
        combo = Converters[converter]
        if combo.nil?
          raise ArgumentError, "unknown converter: #{converter.inspect}"
        end
        if combo.is_a?(Array)  # combo converter
          combo.flat_map {|sub_name| expand_converter(sub_name)}
        else
          [combo]
        end
      else
        unless converter.respond_to?(:call)
          raise ArgumentError,
                "converters must be names or callables: #{converter.inspect}"
        end
        [converter]
      end
    end

    def resolve_selector(selector, headers)
      case selector[:type]
      when :index
        resolve_index(selector[:index], headers)
      else
        resolve_name(selector[:name], selector[:occurrence], headers)
      end
    end

    def resolve_index(index, headers)
      if headers.nil?
        if index < 0
          raise InvalidColumnConversionPlanError,
                "negative column index #{index} requires headers"
        end
        return [index, nil]
      end
      index += headers.size if index < 0
      unless index >= 0 and index < headers.size
        raise InvalidColumnConversionPlanError,
              "no column at index #{index} " \
              "(headers: #{headers.map(&:inspect).join(', ')})"
      end
      [index, header_occurrence(headers, index)]
    end

    def resolve_name(name, occurrence, headers)
      if headers.nil?
        raise InvalidColumnConversionPlanError,
              "column name #{name.inspect} cannot be resolved without headers"
      end
      matches = headers.each_index.select {|i| headers[i] == name}
      if matches.empty?
        raise InvalidColumnConversionPlanError,
              "no column named #{name.inspect} " \
              "(headers: #{headers.map(&:inspect).join(', ')})"
      end
      if occurrence.nil?
        if matches.size > 1
          raise InvalidColumnConversionPlanError,
                "ambiguous column name #{name.inspect} " \
                "(appears #{matches.size} times); " \
                "specify an occurrence number"
        end
        occurrence = 1
      end
      index = matches[occurrence - 1]
      if index.nil?
        raise InvalidColumnConversionPlanError,
              "no column #{occurrence} named #{name.inspect} " \
              "(only #{matches.size} found)"
      end
      [index, occurrence]
    end

    def header_occurrence(headers, index)
      header = headers[index]
      headers[0..index].count(header)
    end

    def describe_column(index, headers)
      description = "column #{index}"
      description << " (#{headers[index].inspect})" if headers
      description
    end

    def handle_failure(failure)
      case @on_error
      when :raise
        raise failure
      when :collect
        @failures << failure
      else
        @on_error.call(failure)
      end
    end
  end
end
