# frozen_string_literal: true

require 'set'
require 'tomlrb/document_parser'
require 'tomlrb/toml_encoder'
require 'tomlrb/piece_table'

module Tomlrb
  # Raised when an edit is structurally impossible (missing path, duplicate
  # declaration, kind conflict...).  The document is left untouched.
  class EditError < StandardError; end

  # A comment- and layout-preserving TOML editor.
  #
  # Edits operate on the original bytes through a piece table, so regions
  # that are not part of an edit keep their exact spacing, quoting, numeric
  # spelling, comments and blank lines.  Every successful edit is re-parsed
  # and cross-checked; on failure the document rolls back and raises
  # {EditError}, so a batch never leaves a half-written file behind.
  class Document
    attr_reader :text

    def initialize(text)
      @table = PieceTable.new(text)
      reload
    end

    def self.load(path)
      new(File.read(path, encoding: 'utf-8'))
    end

    # Yields the document and writes it to +path+ only when the block finishes
    # without raising.
    def self.open(path)
      document = load(path)
      yield(document)
      File.write(path, document.text)
      document
    end

    def to_s
      @table.to_s
    end

    def to_ruby
      Tomlrb.parse(@text)
    end

    # Replaces the value at +path+ with +value+.  A missing leaf key is
    # created inside its existing parent table/inline table.
    def set(path, value)
      commit do |model|
        site, container, key = find_site(model, path)
        if site
          set_existing(site, path, value)
        else
          insert_new_leaf(model, container, key, path, TomlEncoder.encode(value))
        end
      end
    end

    # Renames the key component at +path+ (values are kept byte for byte).
    # Renaming a table header renames that table (all instance headers for an
    # array of tables); dotted keys that define sub-tables follow.
    def rename(path, new_key)
      commit { |model| rename_edits(model, path, new_key) }
    end

    # Moves a key/value pair to +destination_path+.  The value, including its
    # original spelling, is moved as a whole; scopes and dotted keys are
    # rewritten so neighbouring keys are unaffected.
    def move(source_path, destination_path)
      commit { |model| move_edits(model, source_path, destination_path) }
    end

    # ----- byte-preservation introspection -------------------------------

    # [current_from, current_to, original_from] ranges covering every byte of
    # the current document that still matches the original verbatim.
    def surviving_original_ranges
      @table.surviving_original_ranges
    end

    # True when every original byte outside the given ranges of the ORIGINAL
    # document survives unchanged in the current text.
    def untouched_bytes_preserved?(expected_changed_ranges)
      changed = Set.new
      normalize_ranges(expected_changed_ranges).each do |from, to|
        (from...to).each { |index| changed << index }
      end

      originals = @table.original
      surviving = Set.new
      @table.surviving_original_ranges.each do |current_from, current_to, original_from|
        length = current_to - current_from
        length.times do |offset|
          original_index = original_from + offset
          surviving << original_index
          next if changed.include?(original_index)
          unless originals.getbyte(original_index) == @text.getbyte(current_from + offset)
            return false
          end
        end
      end

      expected = Set.new
      originals.bytesize.times { |index| expected << index }
      surviving == expected - changed
    end

    def changed_original_ranges
      originals = @table.original
      surviving = Set.new
      @table.surviving_original_ranges.each do |current_from, current_to, original_from|
        (original_from...(original_from + (current_to - current_from))).each do |index|
          surviving << index
        end
      end
      gaps = ((0...originals.bytesize).to_a - surviving.to_a).sort
      ranges = []
      gaps.each do |index|
        if ranges.any? && ranges[-1][1] == index
          ranges[-1][1] = index + 1
        else
          ranges << [index, index + 1]
        end
      end
      ranges
    end

    private

    def normalize_ranges(ranges)
      Array(ranges).map do |range|
        range.is_a?(Range) ? [range.begin, range.end] : [range[0], range[1]]
      end
    end

    # ----- transaction machinery -----------------------------------------

    def commit
      snapshot = [@table, @text, @model]
      edits = yield(@model)
      edits = edits.reject { |edit| edit.nil? }
      return self if edits.empty?

      @table.apply(edits)
      @text = @table.to_s
      verify_result
      self
    rescue EditError, DocumentParseError, ParseError, EncodeError
      @table, @text, @model = snapshot
      raise
    end

    def verify_result
      Tomlrb.parse(@text)
      parser = DocumentParser.new(@text)
      parser.parse
      @model = parser
    end

    def reload
      @text = @table.to_s
      parser = DocumentParser.new(@text)
      parser.parse
      @model = parser
    end

    # ----- paths ----------------------------------------------------------

    def parse_path(path)
      segments =
        if path.is_a?(Array)
          path.map { |segment| segment.is_a?(Symbol) ? segment.to_s : segment }
        else
          path.to_s.split('.')
        end
      raise EditError, 'Empty path' if segments.empty?
      segments
    end

    def human_path(path)
      Array(path).map(&:to_s).join('.')
    end

    # Returns [site, parent_descriptor_or_hash, key].  +site+ is nil for
    # missing leaves; +container+ is the raw key->descriptor hash of the
    # parent section/inline table.
    def find_site(model, path, strict_parent: true)
      segments = parse_path(path)
      key = segments.last
      parent_path = segments[0..-2]
      container = resolve_container_hash(@model.root, parent_path)
      if container.nil?
        raise EditError, "Parent of #{human_path(path)} does not exist" if strict_parent
        return [nil, nil, key]
      end
      site = container[key]
      [site, container, key]
    end

    # Resolves a section/inline-table path to the raw Hash of its children.
    # Integer segments address array-of-tables instances.
    def resolve_container_hash(node, segments)
      segments.each do |segment|
        if segment.is_a?(Integer)
          return nil unless node.is_a?(Array) && node[0] == :aot
          node = node[1][segment]
          return nil if node.nil?
        else
          return nil unless node.is_a?(Hash)
          descriptor = node[segment]
          return nil if descriptor.nil?
          case descriptor[0]
          when :section, :dotted then node = descriptor[1]
          when :inline then node = descriptor[2]
          when :aot then node = descriptor
          else return nil
          end
        end
      end
      node
    end

    def resolve_descriptor(node, segments)
      segments.each do |segment|
        if segment.is_a?(Integer)
          return nil unless node.is_a?(Array) && node[0] == :aot
          node = node[1][segment]
          return nil if node.nil?
        else
          hash =
            case node
            when Hash then node
            when Array then descriptor_container(node)
            else return nil
            end
          descriptor = hash[segment]
          return nil unless descriptor.is_a?(Array)
          node = descriptor
        end
      end
      node
    end

    def descriptor_container(descriptor)
      case descriptor[0]
      when :section, :dotted then descriptor[1]
      when :inline then descriptor[2]
      else {}
      end
    end

    # ----- set ------------------------------------------------------------

    def set_existing(site, path, value)
      encoded = TomlEncoder.encode(value)
      case site[0]
      when :leaf, :array, :inline
        [[site[1].begin, site[1].end, encoded]]
      when :section, :dotted, :aot
        raise EditError, "#{human_path(path)} is a table and cannot hold a scalar value"
      else
        raise EditError, "Cannot set #{human_path(path)}"
      end
    end

    def insert_new_leaf(model, container_hash, key, path, value_text)
      descriptor_path = descriptor_container_path(path)
      container_descriptor = resolve_descriptor(@model.root, descriptor_path)
      raise EditError, "Parent of #{human_path(path)} does not exist" if container_descriptor.nil?

      if container_descriptor.is_a?(Hash)
        insert_into_root_or_section(model, descriptor_path, container_descriptor, key, value_text)
      else
        insert_into_inline(container_descriptor, key, value_text)
      end
    end

    def descriptor_container_path(path)
      segments = parse_path(path)
      segments[0..-2]
    end

    def insert_into_root_or_section(model, section_path, descriptor, key, value_text)
      ensure_insert_key_valid(key)
      ensure_key_absent(descriptor, key)
      section = section_by_path(model, section_path)
      entry_text = "#{TomlEncoder.encode_key(key)} = #{value_text}"
      insertion_for_section(model, section, entry_text)
    end

    def insert_into_inline(inline_descriptor, key, value_text)
      ensure_insert_key_valid(key)
      children = inline_descriptor[2]
      raise EditError, "Inline key #{TomlEncoder.encode_key(key)} already exists" if children.key?(key)
      range = inline_descriptor[1]
      text = @text.byteslice(range)
      pair = "#{TomlEncoder.encode_key(key)} = #{value_text}"
      insertion =
        if strip_inline_body(text).empty?
          [[range.begin, range.end, "{ #{pair} }"]]
        else
          close = range.end - 1
          insert_pos = close
          insert_pos -= 1 while whitespace_byte?(@text.getbyte(insert_pos - 1))
          [[insert_pos, insert_pos, ", #{pair}"]]
        end
      Array(insertion)
    end

    def strip_inline_body(inline_text)
      inline_text[1..-2].to_s.strip
    end

    def ensure_insert_key_valid(key)
      TomlEncoder.encode_key(key)
      raise EditError, 'Table paths cannot be created with dotted new keys' if key.to_s.include?('.')
    end

    def ensure_key_absent(descriptor_hash, key)
      return unless descriptor_hash.is_a?(Hash) && descriptor_hash.key?(key)
      raise EditError, "Key #{TomlEncoder.encode_key(key)} is already declared"
    end

    def section_by_path(model, path)
      model.sections_by_path[path]
    end

    # Builds an edit that appends +entry_text+ to the end of the section
    # content.  The root section appends after its last top-level pair.
    def insertion_for_section(model, section, entry_text)
      indent = section_indent(section)
      eol = detect_eol
      anchor_entry = section.entries.last

      line = "#{indent}#{entry_text}"
      if anchor_entry
        insert_after_line(anchor_entry.line_range.end, line, eol)
      elsif section.header
        insert_after_line(section.header.line_range.end, line, eol)
      else
        insert_root_entry(model, indent, entry_text, eol)
      end
    end

    # Inserts +line+ after the line ending at +line_ending+, reusing the
    # blank lines that already follow it to separate the new entry from the
    # next header.
    def insert_after_line(line_ending, line, eol)
      remainder = @text.byteslice(line_ending, @text.bytesize - line_ending)
      match = remainder&.match(/\A(?:#{Regexp.escape(eol)})*/)
      blank_run = match ? match[0].bytesize / eol.bytesize : 0
      insertion = "#{eol}#{line}" + (blank_run.zero? ? '' : eol)
      [[line_ending, line_ending, insertion]]
    end

    def insert_root_entry(model, indent, entry_text, eol)
      first_header = model.sections.find { |section| section.header }
      if first_header
        header_start = first_header.line_range.begin
        [[header_start, header_start, "#{indent}#{entry_text}#{eol}#{eol}"]]
      else
        position = @text.bytesize
        prefix = @text.empty? || @text.end_with?("\n") ? '' : eol
        suffix = @text.empty? ? eol : ''
        [[position, position, "#{prefix}#{indent}#{entry_text}#{suffix}"]]
      end
    end

    def section_indent(section)
      indented = section.entries.map(&:indent).reject(&:empty?)
      indented.min_by(&:length) || ''
    end

    def detect_eol
      @text.include?("\r\n") ? "\r\n" : "\n"
    end

    # ----- rename ---------------------------------------------------------

    def rename_edits(model, path, new_key)
      new_key = new_key.to_s
      segments = parse_path(path)
      new_segments = segments[0..-2] + [new_key]
      validate_new_key(new_key)

      if segments.any? { |segment| segment.is_a?(Integer) }
        rename_instance_path(model, segments, new_segments)
      elsif table_header?(model, segments)
        rename_table(model, segments, new_segments, new_key)
      else
        rename_pair(model, segments, new_segments, new_key)
      end
    end

    def validate_new_key(new_key)
      raise EditError, 'New key cannot be empty' if new_key.empty?
      raise EditError, 'New key cannot contain dots' if new_key.include?('.')
      TomlEncoder.encode_key(new_key)
    end

    def table_header?(model, segments)
      descriptor = resolve_descriptor(model.root, segments)
      descriptor.is_a?(Array) && [:section, :aot, :dotted].include?(descriptor[0]) &&
        section_for_table(model, segments)
    end

    def section_for_table(model, segments)
      model.sections.find { |section| section.path == segments ||
        (section.aot && section.path[0..-2] == segments) }
    end

    def rename_instance_path(model, segments, new_segments)
      index_position = segments.index { |segment| segment.is_a?(Integer) }
      if index_position != segments.length - 1
        rename_instance_key(model, segments, new_segments)
      else
        raise EditError, 'Individual array-of-tables instances cannot be renamed; rename the array instead'
      end
    end

    def rename_instance_key(model, segments, new_segments)
      site, = find_site(model, segments)
      raise EditError, "#{human_path(segments)} does not exist" if site.nil?
      collision, = find_site(model, new_segments)
      raise EditError, "#{human_path(new_segments)} is already declared" if collision

      occurrence = find_occurrence(model, segments)
      [[occurrence[:range].begin, occurrence[:range].end,
        TomlEncoder.encode_key(new_segments.last)]]
    end

    def find_occurrence(model, segments)
      model.occurrences.find { |occurrence| occurrence[:path] == segments } ||
        raise(EditError, "#{human_path(segments)} cannot be renamed")
    end

    def rename_pair(model, segments, new_segments, new_key)
      site, = find_site(model, segments)
      raise EditError, "#{human_path(segments)} does not exist" if site.nil?
      collision, = find_site(model, new_segments)
      raise EditError, "#{human_path(new_segments)} is already declared" if collision

      occurrence = find_occurrence(model, segments)
      [[occurrence[:range].begin, occurrence[:range].end,
        TomlEncoder.encode_key(new_key)]]
    end

    def rename_table(model, segments, new_segments, new_key)
      sections = model.sections.select do |section|
        section.path[0...segments.length] == segments ||
          (section.aot && section.path[0...segments.length] == segments)
      end
      raise EditError, "#{human_path(segments)} does not exist" if sections.empty?

      ensure_table_rename_target_absent(model, segments, new_segments)

      edits = []
      model.sections.each do |section|
        next unless section.header
        header_components = section.header.components
        section.header.component_paths.each_with_index do |component_path, index|
          next unless header_component_matches?(component_path, segments)
          component = header_components[index]
          edits << [component[:range].begin, component[:range].end,
                    TomlEncoder.encode_key(new_key)]
        end
      end

      model.occurrences.each do |occurrence|
        next unless occurrence[:path] == segments
        next if occurrence[:inline]
        next if occurrence[:last] && !defines_table?(model, occurrence[:path])
        edits << [occurrence[:range].begin, occurrence[:range].end,
                  TomlEncoder.encode_key(new_key)]
      end

      edits
    end

    def ensure_table_rename_target_absent(model, old_segments, new_segments)
      parent_path = new_segments[0..-2]
      parent = resolve_container_hash(model.root, parent_path)
      return if parent.nil? || !parent.key?(new_segments.last)
      existing = parent[new_segments.last]
      return if existing.is_a?(Array) && existing[0] == :section &&
        existing[2] && existing[2].path[0...old_segments.length] == old_segments
      raise EditError, "#{human_path(new_segments)} is already declared"
    end

    def defines_table?(model, path)
      descriptor = resolve_descriptor(model.root, path)
      descriptor.is_a?(Array) && [:section, :dotted, :aot].include?(descriptor[0])
    end

    # Whether a header component's resolved path corresponds exactly to the
    # table being renamed (ignoring array-of-tables instance indices).
    def header_component_matches?(component_path, table_path)
      stripped = component_path.reject { |segment| segment.is_a?(Integer) }
      stripped == table_path
    end

    # ----- move -----------------------------------------------------------

    def move_edits(model, source_path, destination_path)
      source = parse_path(source_path)
      destination = parse_path(destination_path)
      source_site, = find_site(model, source)
      raise EditError, "#{human_path(source)} does not exist" if source_site.nil?
      unless [:leaf, :array, :inline].include?(source_site[0])
        raise EditError, "#{human_path(source)} is a table; tables are renamed, not moved as values"
      end

      destination_site, = find_site(model, destination, strict_parent: false)
      raise EditError, "#{human_path(destination)} is already declared" if destination_site

      source_entry = entry_for_path(model, source)
      value_range = value_range_of(source_site)
      value_text = @text.byteslice(value_range)

      insertion_edits = build_move_insertion(model, source, destination, value_text)
      removal_edits = build_move_removal(model, source, source_entry, source_site)
      normalize_edits(insertion_edits) + normalize_edits(removal_edits)
    end

    def normalize_edits(edits)
      result = []
      walk = lambda do |item|
        if item.is_a?(Array) && item.length == 3 && item[0].is_a?(Integer)
          result << item
        elsif item.is_a?(Array)
          item.each { |child| walk.call(child) }
        end
      end
      walk.call(edits)
      result
    end

    def entry_for_path(model, path)
      model.entries.find { |entry| entry.path == path ||
        path[0...entry.path.length] == entry.path && path.length > entry.path.length && false }
    end

    def value_range_of(site)
      site[1]
    end

    def build_move_insertion(model, source, destination, value_text)
      parent_segments = destination[0..-2]
      key = destination.last
      validate_new_key(key)

      if parent_segments.empty?
        section = section_by_path(model, [])
        ensure_key_absent(model.root, key)
        return insertion_for_section(model, section, "#{TomlEncoder.encode_key(key)} = #{value_text}")
      end

      parent_descriptor = resolve_descriptor(model.root, parent_segments)
      if parent_descriptor.nil?
        return move_to_new_section(model, destination, parent_segments, key, value_text)
      end

      if parent_descriptor.is_a?(Hash)
        # An array-of-tables instance addressed by a trailing integer.
        ensure_key_absent(parent_descriptor, key)
        section = insertion_section(model, parent_segments)
        insertion_for_section(model, section,
          "#{TomlEncoder.encode_key(key)} = #{value_text}")
      elsif parent_descriptor[0] == :inline
        insert_into_inline(parent_descriptor, key, value_text)
      elsif parent_descriptor[0] == :aot
        raise EditError, "#{human_path(parent_segments)} is an array of tables; target a specific instance"
      elsif [:section, :dotted].include?(parent_descriptor[0])
        ensure_key_absent(parent_descriptor[1], key)
        section = insertion_section(model, parent_segments)
        relative_key = relative_key_for_section(section, parent_segments, key)
        insertion_for_section(model, section,
          "#{relative_key} = #{value_text}")
      else
        raise EditError, "#{human_path(parent_segments)} is not a table"
      end
    end

    # Renders the key of a new entry relative to the section it is inserted
    # into (e.g. a dotted-table target deeper than the section header).
    def relative_key_for_section(section, full_parent_path, key)
      prefix = section ? section.path.length : 0
      remaining = full_parent_path[prefix..]
      (remaining + [key]).map { |part| TomlEncoder.encode_key(part) }.join('.')
    end

    # The section a new pair should be appended to.  Explicit section headers
    # resolve directly; tables created implicitly by dotted keys resolve to
    # the section whose pair defines them.
    def insertion_section(model, path)
      direct = section_by_path(model, path)
      return direct if direct
      model.sections.reverse.find do |section|
        next false if section.path.length > path.length
        section.path == path[0...section.path.length] &&
          section.entries.any? { |entry| entry.path[0...path.length] == path }
      end
    end

    def move_to_new_section(model, destination, parent_segments, key, value_text)
      existing_prefix = longest_existing_section_prefix(model, parent_segments)
      first_header = model.sections.find { |candidate| candidate.header }
      if existing_prefix.empty? && first_header
        raise EditError,
          "Cannot create table #{human_path(parent_segments)} above already-defined tables"
      end

      new_parts = parent_segments[existing_prefix.length..]
      if new_parts.any? { |part| part.is_a?(Integer) }
        raise EditError, 'Cannot create array-of-tables instances while moving a value'
      end

      section = section_by_path(model, existing_prefix) || section_by_path(model, [])
      eol = detect_eol
      header_path = new_parts.map { |part| TomlEncoder.encode_key(part) }.join('.')
      block = ["[#{header_path}]", "#{TomlEncoder.encode_key(key)} = #{value_text}"].join(eol)

      if existing_prefix.empty?
        insert_before_first_header(model, block + eol)
      else
        anchor = section.entries.last
        position = anchor ? anchor.line_range.end : section.header.line_range.end
        [[position, position, "#{eol}#{eol}#{block}"]]
      end
    end

    def longest_existing_section_prefix(model, segments)
      prefix = []
      resolved = model.root
      segments.each do |segment|
        break if segment.is_a?(Integer)
        descriptor = resolved[segment]
        break unless descriptor.is_a?(Array) && [:section, :dotted, :aot].include?(descriptor[0])
        prefix << segment
        resolved = descriptor[0] == :aot ? descriptor : descriptor[1]
      end
      prefix
    end

    def insert_before_first_header(model, block)
      first_header = model.sections.find { |candidate| candidate.header }
      position = first_header ? first_header.header.line_range.begin : @text.bytesize
      separator = position == @text.bytesize && !@text.empty? && !@text.end_with?("\n") ? detect_eol : ''
      [[position, position, "#{separator}#{block}#{detect_eol}"]]
    end

    def build_move_removal(model, source, source_entry, source_site)
      occurrence = find_occurrence(model, source)
      if occurrence[:inline]
        remove_inline_pair(model, source, occurrence, source_site)
      else
        entry = model.entries.find { |candidate| candidate.path == source }
        raise EditError, "#{human_path(source)} cannot be moved" unless entry
        [entry.line_range.begin, entry.line_range.end, '']
      end
    end

    def remove_inline_pair(model, source, occurrence, site)
      parent_descriptor = resolve_descriptor(model.root, source[0..-2])
      inline_descriptor =
        if parent_descriptor.is_a?(Array) && parent_descriptor[0] == :inline
          parent_descriptor
        else
          find_enclosing_inline(model, source)
        end
      raise EditError, "#{human_path(source)} cannot be removed" unless inline_descriptor

      range = inline_descriptor[1]
      value_range = site[1]
      key_range = occurrence[:range]

      pair_start = key_range.begin
      pair_end = value_range.end
      only_pair = body_without_pair_is_empty?(range, pair_start, pair_end)

      if only_pair
        [range.begin, range.end, '{}']
      else
        remove_pair_with_one_comma(range, pair_start, pair_end)
      end
    end

    def find_enclosing_inline(model, source)
      (source.length - 1).downto(0) do |length|
        candidate = resolve_descriptor(model.root, source[0...length])
        return candidate if candidate.is_a?(Array) && candidate[0] == :inline
      end
      nil
    end

    def body_without_pair_is_empty?(range, pair_start, pair_end)
      before = @text.byteslice(range.begin + 1, pair_start - (range.begin + 1))
      after = @text.byteslice(pair_end, range.end - 1 - pair_end)
      before.strip.empty? && after.strip.empty?
    end

    # Removes the pair span plus exactly one neighbouring comma and any
    # whitespace up to it, producing a tidy "{ a = 1, c = 3 }" style result.
    def remove_pair_with_one_comma(range, pair_start, pair_end)
      body_start = range.begin + 1
      before_text = @text.byteslice(body_start, pair_start - body_start)
      after_start = pair_end
      after_text = @text.byteslice(after_start, range.end - 1 - after_start)

      comma_after = after_text.index(',')
      comma_before = before_text.rindex(',')

      if comma_after
        comma_position = after_start + comma_after
        remove_end = comma_position + 1
        remove_end += 1 while whitespace_byte?(@text.getbyte(remove_end))
        [[pair_start, remove_end, '']]
      elsif comma_before
        comma_position = body_start + comma_before
        remove_start = comma_position
        remove_start -= 1 while whitespace_byte?(@text.getbyte(remove_start - 1))
        [[remove_start, pair_end, ' ']]
      else
        [[range.begin, range.end, '{}']]
      end
    end

    def whitespace_byte?(byte)
      byte == 32 || byte == 9
    end
  end
end
