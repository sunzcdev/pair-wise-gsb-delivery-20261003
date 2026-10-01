# frozen_string_literal: true

require 'tomlrb/string_utils'
require 'tomlrb/document_scanner'

module Tomlrb
  # Raised when source text cannot be represented by the document parser.
  class DocumentParseError < StandardError; end

  # Parses a TOML document into a structural model where every key, value,
  # header and line carries its exact byte range.  The semantic tree returned
  # by {Tomlrb.parse} is used to resolve array-of-tables instance indices.
  #
  # Descriptor conventions used inside the tree:
  #   [:section, Hash, Section]   plain or implicit table
  #   [:aot, Array<Hash>]        array of tables (elements are raw hashes)
  #   [:dotted, Hash, Entry]      table implicitly created by a dotted key
  #   [:inline, Range, Hash]      inline table value
  #   [:array, Range, Array]      array value
  #   [:leaf, Range]             scalar value
  class DocumentParser
    Section = Struct.new(:path, :aot, :header, :entries, :children_sections,
                         keyword_init: true)
    Header = Struct.new(:line_range, :aot, :components, :component_paths,
                        keyword_init: true)
    Entry = Struct.new(:path, :line_range, :indent, :key_parts, :value_site,
                       keyword_init: true)

    SCALAR_TYPES = %i[
      STRING_MULTI STRING_LITERAL_MULTI STRING_BASIC STRING_LITERAL
      DATETIME LOCAL_TIME FLOAT FLOAT_KEYWORD INTEGER NON_DEC_INTEGER BOOLEAN
    ].freeze

    def self.parse(text)
      new(text).parse
    end

    def initialize(text)
      @text = text
      @tokens = DocumentScanner.new(text).each_token.to_a
      @pos = 0
      @data = Tomlrb.parse(text)

      @root = {}
      @sections = []
      @sections_by_path = {}
      @entries = []
      @occurrences = []
      @aot_next = Hash.new(0)
    end

    attr_reader :root, :sections, :sections_by_path, :entries, :occurrences

    def parse
      open_section(build_root_section)

      loop do
        skip_inline_ws
        tok = current
        break if tok.type == :EOS

        if tok.type == :NEWLINE
          advance
        elsif tok.type == :COMMENT
          advance
          skip_inline_ws
          expect_newline_or_eos
        elsif tok.type == '['
          parse_section
        else
          parse_pair
        end
      end

      self
    end

    private

    def build_root_section
      section = Section.new(
        path: [], aot: false, header: nil, entries: [], children_sections: []
      )
      @sections << section
      @sections_by_path[[]] = section
      section
    end

    def open_section(section)
      @current_section = section
    end

    # ----- token helpers -------------------------------------------------

    def current
      @tokens[@pos]
    end

    def peek(offset = 1)
      @tokens[@pos + offset]
    end

    def advance
      tok = @tokens[@pos]
      @pos += 1
      tok
    end

    def skip_inline_ws
      advance while current.type == :WS
    end

    def skip_layout
      advance while [:WS, :NEWLINE, :COMMENT].include?(current.type)
    end

    def expect(char)
      tok = current
      raise DocumentParseError, "Expected #{char.inspect} at byte #{tok.from}" unless tok.type == char
      advance
    end

    def expect_newline_or_eos
      tok = current
      return if tok.type == :EOS
      raise DocumentParseError, "Expected newline at byte #{tok.from}" unless tok.type == :NEWLINE
      advance
    end

    def line_start_before(first_index)
      index = first_index
      while index > 0 && @tokens[index - 1].type == :WS
        index -= 1
      end
      if index > 0 && @tokens[index - 1].type == :NEWLINE
        @tokens[index - 1].to
      elsif index == 0
        0
      else
        @tokens[index].from
      end
    end

    def consume_line_ending(start)
      skip_inline_ws
      advance if current.type == :COMMENT
      skip_inline_ws
      line_to = current.from
      if current.type == :NEWLINE
        line_to = advance.to
      end
      (start...line_to)
    end

    # ----- sections -------------------------------------------------------

    def parse_section
      first_index = @pos
      start = line_start_before(first_index)
      aot = peek(1).type == '['
      expect('[')
      expect('[') if aot
      skip_inline_ws
      components = parse_key_components
      skip_inline_ws
      expect(']')
      expect(']') if aot
      line_range = consume_line_ending(start)

      path, component_paths = resolve_header(components, aot)
      section = ensure_section(path, aot)
      section.header = Header.new(
        line_range: line_range,
        aot: aot,
        components: components,
        component_paths: component_paths
      )
      register_section_in_tree(path, section)
      open_section(section)
    end

    # Resolves a header into its fully-qualified instance path.  Instance
    # indices come from the header currently open plus next-instance
    # counters, so a header appearing while the first [[fruit]] is open
    # resolves to ["fruit", 0, ...].
    def resolve_header(components, aot)
      path = []
      component_paths = []
      current = @current_section.path

      components.each_with_index do |component, index|
        key = component[:key]
        opening_new_instance = aot && index == components.length - 1
        if semantic_kind(path, key) == :aot
          instance =
            if opening_new_instance
              @aot_next[path + [key]]
            else
              inherited_instance(current, path, key)
            end
          path += [key, instance]
        else
          path << key
        end
        component_paths << path.dup
      end

      if aot
        @aot_next[path[0..-2]] = path[-1] + 1
      end
      [path, component_paths]
    end

    # Instance index inherited from the section currently in scope for an
    # array name encountered as a (non-opening) header prefix.
    def inherited_instance(current_path, resolved_prefix, key)
      index = resolved_prefix.length
      matching = current_path[0...index] == resolved_prefix
      raise DocumentParseError, "No open array-of-tables instance for #{key.inspect}" unless matching
      current_path[index + 1]
    end

    def semantic_kind(context_path, key)
      node = @data
      context_path.each do |segment|
        node = segment.is_a?(Integer) ? node[segment] : node[segment]
        return nil if node.nil?
      end
      return nil unless node.is_a?(Hash)
      node[key].is_a?(Array) ? :aot : :table
    end

    def ensure_section(path, aot)
      existing = @sections_by_path[path]
      return existing if existing

      section = Section.new(
        path: path, aot: aot, header: nil, entries: [], children_sections: []
      )
      @sections << section
      @sections_by_path[path] = section
      parent_path = aot ? path[0..-3] : path[0..-2]
      parent = @sections_by_path[parent_path]
      parent.children_sections << section if parent
      section
    end

    def register_section_in_tree(path, section)
      node = @root
      path.each_with_index do |segment, index|
        if segment.is_a?(Integer)
          raise DocumentParseError, 'Invalid array-of-tables path' unless node[0] == :aot
          node[1][segment] ||= {}
          node = node[1][segment]
          next
        end

        next_segment = path[index + 1]
        descriptor = node[segment]
        if descriptor.nil?
          descriptor =
            if next_segment.is_a?(Integer)
              [:aot, []]
            else
              [:section, {}, nil]
            end
          node[segment] = descriptor
        end

        case descriptor[0]
        when :aot
          node = descriptor # next segment is the integer instance index
        when :section, :dotted
          descriptor[2] = section if index == path.length - 1 && !next_segment.is_a?(Integer)
          node = descriptor[1]
        else
          raise DocumentParseError, "Table path segment #{segment.inspect} conflicts with a value"
        end
      end
    end

    # ----- key/value pairs ------------------------------------------------

    def parse_pair
      first_index = @pos
      start = line_start_before(first_index)
      key_components = parse_key_components
      section_path = @current_section.path
      full_paths = key_components.map.with_index do |_component, index|
        section_path + key_components[0..index].map { |part| part[:key] }
      end
      skip_inline_ws
      expect('=')
      skip_inline_ws
      value_site = parse_value(full_paths.last)
      line_range = consume_line_ending(start)

      entry = Entry.new(
        path: full_paths.last,
        line_range: line_range,
        indent: @text.byteslice(start, key_components.first[:range].begin - start),
        key_parts: key_components.map.with_index { |_component, index|
          { range: key_components[index][:range], path: full_paths[index] }
        },
        value_site: value_site
      )
      @entries << entry
      @current_section.entries << entry
      @occurrences.concat(entry.key_parts)
      register_pair_in_tree(section_path, key_components, full_paths, value_site)
    end

    def register_pair_in_tree(section_path, key_components, full_paths, value_site)
      container = resolve_container(section_path)
      key_components.each_with_index do |component, index|
        key = component[:key]
        if index == key_components.length - 1
          container[key] = value_site
        else
          descriptor = container[key]
          if descriptor.nil?
            descriptor = [:dotted, {}, nil]
            container[key] = descriptor
          end
          raise DocumentParseError, "Dotted key #{key.inspect} conflicts" unless descriptor[0] == :dotted
          container = descriptor[1]
        end
      end
    end

    # Resolves the raw key->descriptor hash representing +path+ (section or
    # array-of-tables instance).
    def resolve_container(path)
      node = @root
      path.each do |segment|
        if segment.is_a?(Integer)
          raise DocumentParseError, 'Invalid instance index in path' unless node[0] == :aot
          node = node[1][segment]
        else
          descriptor = node.fetch(segment)
          node =
            case descriptor[0]
            when :section, :dotted then descriptor[1]
            when :aot then descriptor
            else raise DocumentParseError, "#{segment.inspect} is a value, not a table"
            end
        end
      end
      node
    end

    # ----- keys -----------------------------------------------------------

    def parse_key_components
      components = []
      loop do
        tok = current
        if tok.type == :FLOAT
          offset = tok.from
          tok.text.split('.', -1).each do |piece|
            components << { key: piece, range: (offset...offset + piece.bytesize) }
            offset += piece.bytesize + 1
          end
          advance
        else
          components << decode_key_component(tok)
          advance
        end
        skip_inline_ws
        break unless current.type == '.'
        expect('.')
        skip_inline_ws
      end
      components
    end

    def decode_key_component(tok)
      key =
        case tok.type
        when :STRING_BASIC
          StringUtils.replace_escaped_chars(tok.text[1..-2])
        when :STRING_LITERAL
          tok.text[1..-2]
        else
          tok.text
        end
      { key: key, range: tok.range }
    end

    # ----- values ---------------------------------------------------------

    def parse_value(path)
      case current.type
      when '[' then parse_array(path)
      when '{' then parse_inline(path)
      else parse_scalar
      end
    end

    def parse_scalar
      tok = current
      unless SCALAR_TYPES.include?(tok.type)
        raise DocumentParseError, "Unexpected token #{tok.type} at byte #{tok.from}"
      end
      advance
      [:leaf, tok.range]
    end

    def parse_array(path)
      open_token = expect('[')
      items = []
      skip_layout
      unless current.type == ']'
        loop do
          items << parse_value(path + [items.length])
          skip_layout
          if current.type == ','
            advance
            skip_layout
          end
          break if current.type == ']'
        end
      end
      close_token = expect(']')
      [:array, (open_token.from...close_token.to), items]
    end

    def parse_inline(base_path)
      open_token = expect('{')
      children = {}
      skip_inline_ws
      unless current.type == '}'
        loop do
          key_components = parse_key_components
          skip_inline_ws
          expect('=')
          skip_inline_ws
          key_paths = key_components.map.with_index do |_component, index|
            base_path + key_components[0..index].map { |part| part[:key] }
          end
          value_site = parse_value(key_paths.last)
          register_inline_pair(children, key_components, key_paths, value_site)
          skip_inline_ws
          if current.type == ','
            advance
            skip_inline_ws
          end
          break if current.type == '}'
        end
      end
      close_token = expect('}')
      [:inline, (open_token.from...close_token.to), children]
    end

    def register_inline_pair(children, key_components, key_paths, value_site)
      container = children
      key_components.each_with_index do |component, index|
        key = component[:key]
        if index == key_components.length - 1
          container[key] = value_site
        else
          descriptor = container[key]
          unless descriptor
            descriptor = [:inline, nil, {}]
            container[key] = descriptor
          end
          raise DocumentParseError, "Inline key #{key.inspect} conflict" unless descriptor[0] == :inline
          container = descriptor[2]
        end
      end
      key_components.each_with_index do |component, index|
        @occurrences << {
          range: component[:range],
          path: key_paths[index],
          inline: true,
          last: index == key_components.length - 1
        }
      end
    end
  end
end
