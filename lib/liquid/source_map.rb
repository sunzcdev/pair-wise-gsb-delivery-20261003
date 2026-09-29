# frozen_string_literal: true

module Liquid
  # SourceMap records which template fragment produced each byte range of the
  # rendered output. It is only active when a template is parsed (or rendered)
  # with `source_map: true`:
  #
  #   template = Liquid::Template.parse(source, source_map: true)
  #   output   = template.render(assigns)
  #   entry    = template.source_map.lookup(byte_offset)
  #
  # Only template names and source positions are stored. Rendered values
  # (business data) are never copied into the map.
  class SourceMap
    # A Frame marks entering a partial through {% render %} or {% include %}.
    # +template_name+ is the partial being rendered, +line_number+ is the
    # line of the render/include tag in the calling template.
    Frame = Struct.new(:template_name, :line_number)

    # An Entry attributes the byte range [start_byte, end_byte) of the output
    # to a template node. +line_number+ and +column+ are 1-based positions in
    # the template source (+column+ counts bytes). +stack+ lists the nested
    # render/include calls that led to this entry, innermost last.
    Entry = Struct.new(:start_byte, :end_byte, :template_name, :line_number, :column, :node_type, :stack) do
      def empty?
        start_byte == end_byte
      end

      def cover?(byte_offset)
        start_byte <= byte_offset && byte_offset < end_byte
      end
    end

    attr_reader :entries, :root_template_name

    def initialize(root_template_name = nil)
      @root_template_name = root_template_name
      @entries = []
      @stack   = []
      # The output buffer of the root template. Nodes rendered into other
      # buffers (e.g. {% capture %}) are not recorded against the main output.
      @output  = nil
    end

    # @api private
    attr_accessor :output

    # @api private
    def push_frame(template_name, line_number)
      @stack.push(Frame.new(template_name, line_number))
    end

    # @api private
    def pop_frame
      @stack.pop
    end

    # @api private
    # Called by BlockBody after rendering each node. +position+ is a
    # [line, column] pair captured at parse time, or nil when the template
    # was parsed without source positions.
    def record(start_byte, end_byte, node, position, context)
      line_number = position ? position[0] : (node.line_number if node.respond_to?(:line_number))
      column      = position && position[1]
      node_type   = if node.instance_of?(String)
        :text
      elsif node.is_a?(Variable)
        :variable
      else
        :tag
      end

      @entries << Entry.new(
        start_byte,
        end_byte,
        context.template_name,
        line_number,
        column,
        node_type,
        @stack.empty? ? Const::EMPTY_ARRAY : @stack.dup
      )
    end

    # All entries covering +byte_offset+, including empty (zero-length)
    # entries anchored exactly at the offset.
    def entries_at(byte_offset)
      @entries.select do |entry|
        entry.cover?(byte_offset) || (entry.empty? && entry.start_byte == byte_offset)
      end
    end

    # The single most useful entry for +byte_offset+. Empty entries are only
    # returned when no non-empty entry covers the offset.
    def lookup(byte_offset)
      matches = entries_at(byte_offset)
      matches.find { |entry| !entry.empty? } || matches.first
    end

    # All entries overlapping the byte range [start_byte, end_byte).
    def query(start_byte, end_byte)
      @entries.select do |entry|
        if entry.empty?
          start_byte <= entry.start_byte && entry.start_byte <= end_byte
        else
          entry.start_byte < end_byte && start_byte < entry.end_byte
        end
      end
    end

    # Total number of output bytes covered by the map.
    def byte_length
      @entries.map(&:end_byte).max || 0
    end
  end
end
