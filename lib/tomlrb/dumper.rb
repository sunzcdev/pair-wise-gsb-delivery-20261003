# frozen-string-literal: true

require 'date'

module Tomlrb
  class DumpError < StandardError; end

  # Serializes a Ruby Hash into a TOML 1.0 string.
  #
  # Supported values: String, Symbol keys, Integer, Float, true/false,
  # Time, DateTime, Date, LocalDateTime, LocalDate, LocalTime, Array and
  # Hash. Anything else (including nil) raises a DumpError reporting the
  # structural path of the offending value.
  class Dumper
    BARE_KEY = /\A[A-Za-z0-9_-]+\z/.freeze

    ESCAPES = {
      "\b" => '\b',
      "\t" => '\t',
      "\n" => '\n',
      "\f" => '\f',
      "\r" => '\r',
      '"' => '\"',
      '\\' => '\\\\'
    }.freeze

    def initialize(hash)
      unless hash.is_a?(Hash)
        raise DumpError, "Root value must be a Hash, got #{hash.class}"
      end

      @hash = hash
      @output = +''
      @ancestors = {}.compare_by_identity
    end

    def to_toml
      visit_table(@hash, [])
      @output
    end

    private

    def visit_table(hash, path)
      with_container(hash, path) do
        pairs = []
        tables = []
        table_arrays = []

        hash.each do |key, value|
          key = normalize_key(key, path)
          case value
          when Hash
            tables << [key, value]
          when Array
            if !value.empty? && value.all? { |element| element.is_a?(Hash) }
              table_arrays << [key, value]
            else
              pairs << [key, value]
            end
          else
            pairs << [key, value]
          end
        end

        pairs.each do |key, value|
          @output << quote_key(key) << ' = ' << toml_value(value, path + [key]) << "\n"
        end

        tables.each do |key, table|
          write_header('[', ']', path + [key])
          visit_table(table, path + [key])
        end

        table_arrays.each do |key, elements|
          elements.each do |element|
            write_header('[[', ']]', path + [key])
            visit_table(element, path + [key])
          end
        end
      end
    end

    def write_header(opening, closing, path)
      @output << "\n" unless @output.empty?
      @output << opening << path.map { |key| quote_key(key) }.join('.') << closing << "\n"
    end

    def toml_value(value, path)
      case value
      when String then toml_string(value)
      when Integer then value.to_s
      when Float then toml_float(value)
      when true, false then value.to_s
      when Time then toml_time(value)
      when DateTime then toml_datetime(value)
      when Date then value.strftime('%Y-%m-%d')
      when LocalDateTime, LocalDate, LocalTime then value.to_s
      when Array then toml_array(value, path)
      when Hash then toml_inline_table(value, path)
      when nil
        raise DumpError, "nil cannot be represented in TOML at #{format_path(path)}"
      else
        raise DumpError, "Unsupported value of type #{value.class} at #{format_path(path)}"
      end
    end

    def toml_array(array, path)
      with_container(array, path) do
        elements = array.each_with_index.map do |element, index|
          toml_value(element, path + [index])
        end
        "[#{elements.join(', ')}]"
      end
    end

    def toml_inline_table(hash, path)
      with_container(hash, path) do
        pairs = hash.map do |key, value|
          key = normalize_key(key, path)
          "#{quote_key(key)} = #{toml_value(value, path + [key])}"
        end
        "{#{pairs.join(', ')}}"
      end
    end

    def toml_string(string)
      escaped = string.gsub(/["\\]|[\x00-\x1f\x7f]/) do |char|
        ESCAPES[char] || format('\u%04X', char.ord)
      end
      %("#{escaped}")
    end

    def toml_float(float)
      if float.nan?
        'nan'
      elsif float.infinite?
        float.positive? ? 'inf' : '-inf'
      else
        float.to_s
      end
    end

    def toml_time(time)
      base = time.strftime('%Y-%m-%dT%H:%M:%S')
      "#{base}#{fraction_str(time.subsec)}#{offset_str(time.utc_offset)}"
    end

    def toml_datetime(datetime)
      base = datetime.strftime('%Y-%m-%dT%H:%M:%S')
      offset_seconds = (datetime.offset * 86_400).to_i
      "#{base}#{fraction_str(datetime.sec_fraction)}#{offset_str(offset_seconds)}"
    end

    def fraction_str(subsec)
      return '' if subsec.zero?

      digits = format('%09d', (subsec * 1_000_000_000).round).sub(/0+\z/, '')
      ".#{digits}"
    end

    def offset_str(offset_seconds)
      return 'Z' if offset_seconds.zero?

      sign = offset_seconds.negative? ? '-' : '+'
      absolute = offset_seconds.abs
      format('%s%02d:%02d', sign, absolute / 3600, (absolute % 3600) / 60)
    end

    def quote_key(key)
      key.match?(BARE_KEY) ? key : toml_string(key)
    end

    def normalize_key(key, path)
      case key
      when String then key
      when Symbol then key.to_s
      else
        raise DumpError, "Unsupported key of type #{key.class} at #{format_path(path)}"
      end
    end

    def with_container(container, path)
      if @ancestors.key?(container)
        raise DumpError, "Circular reference detected at #{format_path(path)}"
      end

      @ancestors[container] = true
      begin
        yield
      ensure
        @ancestors.delete(container)
      end
    end

    def format_path(path)
      return '<root>' if path.empty?

      path.map { |segment| segment.is_a?(Integer) ? "[#{segment}]" : ".#{segment}" }.join.sub(/\A\./, '')
    end
  end
end
