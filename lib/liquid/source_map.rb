# frozen_string_literal: true

module Liquid
  # An optional, opt-in mapping from the bytes of a rendered template back to
  # the template source that produced them.
  #
  # The source map does not change the rendered output in any way; it only
  # observes rendering. Enable it by passing `source_map: true` (or a
  # `Liquid::SourceMap` instance) to `Template#render`, and read it back via
  # `Template#source_map`:
  #
  #   template = Liquid::Template.parse(source, line_numbers: true)
  #   template.name = 'mail/order_confirmation'
  #   output = template.render(assigns, source_map: true)
  #
  #   entry = template.source_map.entry_at(42)
  #   entry.template_name # => 'mail/order_confirmation'
  #   entry.line_number   # => 12
  #   entry.column        # => 7
  #   entry.stack         # => call-site frames of nested {% render %}/{% include %}
  #
  # Parse templates with `line_numbers: true` to get line and column
  # information; without it entries still carry template names and call stacks.
  #
  # All byte offsets are UTF-8 byte offsets into the rendered output, i.e. they
  # are compatible with `String#byteslice` on the rendered body. Columns are
  # 1-based character columns in the template source.
  #
  # The map only records source positions (template names, line/column numbers
  # and node kinds); it never copies rendered content or variable values.
  class SourceMap
    # A single call site of a nested {% render %} or {% include %} tag.
    class Frame
      attr_reader :template_name, :line_number, :column

      def initialize(template_name, line_number, column)
        @template_name = template_name
        @line_number   = line_number
        @column        = column
      end

      def ==(other)
        other.is_a?(Frame) &&
          template_name == other.template_name &&
          line_number == other.line_number &&
          column == other.column
      end
      alias_method :eql?, :==

      def to_h
        { template_name: template_name, line_number: line_number, column: column }
      end

      def inspect
        "#<Liquid::SourceMap::Frame template=#{template_name.inspect} " \
          "line=#{line_number.inspect} column=#{column.inspect}>"
      end
    end

    # Maps a UTF-8 byte range of the rendered output to the template node that
    # produced it.
    class Entry
      # Byte range in the rendered output (`end_byte` is exclusive). Empty
      # ranges (`start_byte == end_byte`) belong to nodes that rendered
      # nothing, e.g. blank tags or variables evaluating to nil.
      attr_reader :start_byte, :end_byte

      # Name of the template (or partial) the node belongs to.
      attr_reader :template_name

      # 1-based line number and 1-based character column of the node in its
      # template source. Both are nil for literal text and when the template
      # was not parsed with `line_numbers: true`.
      attr_reader :line_number, :column

      # Kind of the producing node: 'text', 'variable', or a tag name such as
      # 'for' or 'render'.
      attr_reader :node_type

      # Call sites of enclosing {% render %}/{% include %} tags, outermost
      # first. Empty for output produced by the top-level template.
      attr_reader :stack

      def initialize(start_byte, end_byte, template_name, line_number, column, node_type, stack)
        @start_byte    = start_byte
        @end_byte      = end_byte
        @template_name = template_name
        @line_number   = line_number
        @column        = column
        @node_type     = node_type
        @stack         = stack
      end

      def byte_range
        start_byte...end_byte
      end

      def length
        end_byte - start_byte
      end

      def empty?
        start_byte == end_byte
      end

      def ==(other)
        other.is_a?(Entry) &&
          start_byte == other.start_byte &&
          end_byte == other.end_byte &&
          template_name == other.template_name &&
          line_number == other.line_number &&
          column == other.column &&
          node_type == other.node_type &&
          stack == other.stack
      end
      alias_method :eql?, :==

      def to_h
        {
          byte_range: byte_range,
          template_name: template_name,
          line_number: line_number,
          column: column,
          node_type: node_type,
          stack: stack.map(&:to_h),
        }
      end

      def inspect
        "#<Liquid::SourceMap::Entry #{start_byte}...#{end_byte} " \
          "template=#{template_name.inspect} line=#{line_number.inspect} " \
          "column=#{column.inspect} node=#{node_type.inspect}>"
      end
    end

    include Enumerable

    attr_reader :entries

    # @api private
    # The output buffer the map records offsets for. Rendering into any other
    # buffer (e.g. {% capture %} bodies) is not recorded.
    attr_accessor :root_buffer

    def initialize
      @entries     = []
      @stack       = []
      @root_buffer = nil
    end

    def each(&block)
      @entries.each(&block)
    end

    def size
      @entries.size
    end

    # Returns the innermost entry covering the given UTF-8 byte offset of the
    # rendered output, or nil when no entry covers it. When only empty-output
    # nodes sit at that offset, the innermost empty entry is returned.
    def entry_at(byte_offset)
      entries_at(byte_offset).first
    end

    # Returns all entries covering the given UTF-8 byte offset, innermost
    # (most specific) first. Entries from enclosing tags such as {% for %} or
    # {% render %} cover the output of their children and therefore appear
    # later in the list.
    def entries_at(byte_offset)
      covering = @entries.select do |entry|
        entry.start_byte <= byte_offset && byte_offset < entry.end_byte
      end
      if covering.empty?
        covering = @entries.select do |entry|
          entry.empty? && entry.start_byte == byte_offset
        end
      end
      # Innermost first: shorter ranges are more specific; record order
      # (inner nodes complete before enclosing tags) breaks ties.
      covering.each_with_index.sort_by { |entry, index| [entry.length, index] }.map(&:first)
    end

    # Returns all entries intersecting the given UTF-8 byte range of the
    # rendered output (a Range, e.g. `10...25`). Empty-output entries
    # positioned inside the range are included.
    def entries_in(byte_range)
      range_start = byte_range.begin
      range_end   = byte_range.end
      range_end  -= 1 if byte_range.exclude_end? && range_end > range_start

      @entries.select do |entry|
        if entry.empty?
          range_start <= entry.start_byte && entry.start_byte <= range_end
        else
          entry.start_byte <= range_end && range_start < entry.end_byte
        end
      end
    end

    # @api private
    def record(start_byte, end_byte, template_name, line_number, column, node_type)
      stack = @stack.empty? ? Const::EMPTY_ARRAY : @stack.dup.freeze
      @entries << Entry.new(
        start_byte, end_byte, template_name, line_number, column, node_type, stack
      )
    end

    # @api private
    def push_frame(template_name, line_number, column)
      @stack << Frame.new(template_name, line_number, column)
    end

    # @api private
    def pop_frame
      @stack.pop
    end
  end
end
