# frozen-string-literal: true

module Tomlrb
  class Handler
    attr_reader :output, :symbolize_keys

    def initialize(**options)
      @output = {}
      @current = @output
      @stack = []
      @array_names = []
      @current_table = []
      @keys = Keys.new
      @symbolize_keys = options[:symbolize_keys]
    end

    def set_context(identifiers, is_array_of_tables: false)
      if identifiers.empty?
        raise ParseError, 'Array needs a name'
      end

      # TOML table headers always carry the full absolute path, so a new
      # header replaces (rather than extends) the current table context.
      @current_table = identifiers.map(&:dup)
      @keys.add_table_key(@current_table.map(&:dup), is_array_of_tables)
      @current = @output

      name_identifiers = identifiers.map { |identifier| symbolize(identifier.name) }
      deal_with_array_of_tables(name_identifiers, is_array_of_tables) do |table_names|
        table_names.each do |k|
          if @current[k].is_a?(Array)
            @current = @current[k].last
          else
            @current[k] ||= {}
            @current = @current[k]
          end
        end
      end
    end

    def deal_with_array_of_tables(identifiers, is_array_of_tables)
      stringified_identifier = identifiers.join('.')

      if is_array_of_tables
        @array_names << stringified_identifier
        last_identifier = identifiers.pop
      elsif @array_names.include?(stringified_identifier)
        raise ParseError, 'Cannot define a normal table with the same name as an already established array'
      end

      yield(identifiers)

      if is_array_of_tables
        @current[last_identifier] ||= []
        raise ParseError, "Cannot use key #{last_identifier} for both table and array at once" unless @current[last_identifier].respond_to?(:<<)
        @current[last_identifier] << {}
        @current = @current[last_identifier].last
      end
    end

    def assign(k)
      terminal_kind = @stack.last.is_a?(InlineTable) ? :inline_table : :value
      @keys.add_pair_key k, @current_table, terminal_kind

      if @stack.last.is_a?(InlineTable)
        inline = @stack.pop
        inline.sources.each do |relative, source|
          # Implicit parents are materialized while walking the terminal
          # value entries, so only those terminals are declared.
          next if source.kind == :implicit_table

          @keys.declare_inline_entry(@current_table + k + parts_from_names(relative), source)
        end
        @stack.push(inline.to_h)
      end

      current = @current
      while (key = k.shift)
        key = symbolize(key.name)
        current = assign_key_path(current, key, k.empty?)
      end
    end

    def push(o)
      @stack << o
    end

    def push_inline(inline_arrays)
      merged_inline = InlineTable.new

      inline_arrays.each do |inline_array|
        key_parts = inline_array.dup
        value = key_parts.pop
        current = merged_inline.hash

        nested = value.is_a?(InlineTable)
        stored_value = nested ? value.to_h : value

        key_parts.each_with_index do |part, index|
          key = symbolize(part.name)
          last_key = index == key_parts.size - 1

          if last_key
            unless current[key].nil?
              raise Key::KeyConflict.conflict(
                part.name,
                Source.new(nested ? :inline_table : :value, part.location),
                merged_inline.source_for(key_parts.map(&:name))
              )
            end
            current[key] = stored_value
            merged_inline.add_source(
              key_parts.map(&:name),
              Source.new(nested ? :inline_table : :value, part.location)
            )
            if nested
              value.sources.each do |relative, source|
                merged_inline.add_source(key_parts[0...-1].map(&:name) + relative, source)
              end
            end
          else
            if current.key?(key) && !current[key].is_a?(Hash)
              raise Key::KeyConflict.conflict(
                part.name,
                Source.new(:implicit_table, part.location),
                merged_inline.source_for(key_parts[0..index].map(&:name))
              )
            end
            current[key] ||= {}
            current = current[key]
            merged_inline.add_source(
              key_parts[0..index].map(&:name),
              Source.new(:implicit_table, part.location)
            )
          end
        end
      end

      push(merged_inline)
    end

    def start_(type)
      push([type])
    end

    def end_(type)
      array = []
      while (value = @stack.pop) != [type]
        raise ParseError, 'Unclosed table' if @stack.empty?

        array.unshift(type == :array && value.is_a?(InlineTable) ? value.to_h : value)
      end
      array
    end

    def validate_value(value)
      if value.nil?
        raise ParseError, 'Value must be present'
      end
    end

    private

    def symbolize(key)
      @symbolize_keys ? key.to_sym : key
    end

    def parts_from_names(names)
      names.map { |name| KeyPart.new(name, nil) }
    end

    def assign_key_path(current, key, key_emptied)
      existed = current.key?(key)
      raise ParseError, "Cannot overwrite value with key #{key}" if existed && !current[key].is_a?(Hash)
      if key_emptied
        raise ParseError, "Cannot overwrite value with key #{key}" unless current.is_a?(Hash)

        value = @stack.pop
        raise ParseError, "Cannot overwrite value with key #{key}" if current[key].is_a?(Hash) && !value.is_a?(Hash)
        current[key] = value
        return current
      end
      current[key] ||= {}
      current[key]
    end
  end

  # Carries the ordered source information of an inline table while it is
  # being assembled, so entries can be registered against the global key tree
  # once the assignment consuming the inline table is complete.
  class InlineTable
    attr_reader :hash, :sources

    def initialize
      @hash = {}
      # Array of [path_names, Source], kept in declaration order.
      @sources = []
    end

    def add_source(names, source)
      @sources.reject! { |path, _| path == names }
      @sources << [names, source]
    end

    def source_for(names)
      found = @sources.assoc(names)
      found && found[1]
    end

    def to_h
      @hash
    end
  end

  class Keys
    def initialize
      @keys = {}
    end

    def add_table_key(keys, is_array_of_tables = false)
      declare(keys, [], :explicit_table, is_array_of_tables, explicit_header: true)
    end

    def add_pair_key(keys, context, terminal_kind = :value)
      declare(context, keys, terminal_kind, false, explicit_header: false)
    end

    # Registers a flattened entry coming from a closed inline table.
    def declare_inline_entry(parts, source)
      declare([], parts, source.kind, false, source, from_inline: true)
    end

    private

    # +context+      : KeyParts of the enclosing table header (may be empty)
    # +keys+         : KeyParts of the declared path relative to the context
    # +terminal_kind+ only describes the last component of +keys+
    def declare(context, keys, terminal_kind, is_array_of_tables, forced_source = nil,
                explicit_header: false, from_inline: false)
      path = normalize_parts(context) + normalize_parts(keys)
      path.each_with_index.inject(@keys) do |nodes, (part, index)|
        terminal = index == path.length - 1
        kind = terminal ? terminal_kind : :implicit_table
        source = forced_source if terminal && forced_source
        source ||= Source.new(kind, part.location || Location.new(nil, nil))

        node = nodes[part.name]
        if node
          node.declare(kind, source, terminal, is_array_of_tables, from_inline)
        else
          # An intermediate table component is later promotable to an
          # explicit header only when it was created by another explicit
          # header, or by a dotted key at the document root.
          promotable_later =
            if terminal
              false
            elsif explicit_header
              true
            else
              # The first component of a dotted key at the document root is
              # allowed to be defined as a super-table later on; components
              # created inside an already open table are not.
              context.empty? && index.zero?
            end
          node = Key.new(
            part.name,
            kind == :value ? :pair : :table,
            terminal,
            source,
            kind,
            promotable_later: promotable_later
          )
          node.clear_children if is_array_of_tables && terminal
          nodes[part.name] = node
        end
        node.children
      end
    end

    def normalize_parts(parts)
      parts.map do |part|
        part.is_a?(KeyPart) ? part : KeyPart.new(part, nil)
      end
    end
  end

  class Key
    class KeyConflict < ParseError
      attr_reader :key_name, :current, :previous

      def self.conflict(key_name, current_source, previous_source)
        new(build_message(key_name, current_source, previous_source), key_name, current_source, previous_source)
      end

      def self.build_message(key_name, current_source, previous_source)
        "Key #{key_name.inspect} is already used: " \
          "redeclared as #{current_source}, but was previously declared as #{previous_source}"
      end

      def initialize(message, key_name = nil, current_source = nil, previous_source = nil)
        super(message)
        @key_name = key_name
        @current = current_source
        @previous = previous_source
      end
    end

    attr_reader :key, :type, :source, :children, :kind

    def initialize(key, type, declared = false, source = nil, kind = nil, promotable_later: false)
      @key = key
      @type = type
      @declared = declared
      @source = source
      @kind = kind
      @promotable_later = promotable_later
      @children = {}
    end

    def declared?
      @declared
    end

    def clear_children
      @children.clear
    end

    # Applies an incoming declaration to this node.
    def declare(kind, source, terminal, is_array_of_tables, traversing_inline = false)
      incoming_type = kind == :value ? :pair : :table

      if terminal
        declare_terminal(kind, incoming_type, source, is_array_of_tables)
      else
        # Parent components of inline-table entries are already established
        # by other entries (or by the inline table value itself), so a
        # shared implicit parent is merely traversed.
        if traversing_inline && @type == :table
          return
        end

        declare_intermediate(kind, source)
      end
    end

    private

    def declare_intermediate(_kind, source)
      # An existing value or closed inline table can never become a parent
      # table for new keys; an existing (open) table can.
      return if @type == :table && @kind != :inline_table

      raise KeyConflict.conflict(@key, source, @source)
    end

    def declare_terminal(kind, incoming_type, source, is_array_of_tables)
      # A fresh array-of-tables instance reuses the same header node but
      # starts a new subtree, so its own keys must not clash with previous
      # instances.
      if is_array_of_tables && @kind == :explicit_table && @type == :table
        @source = source
        clear_children
        return
      end

      if incoming_type != @type
        raise KeyConflict.conflict(@key, source, @source)
      end

      # Once closed, an inline table can neither be extended nor reused.
      if @kind == :inline_table || kind == :inline_table
        raise KeyConflict.conflict(@key, source, @source)
      end

      # An implicit parent table created by a dotted key may be promoted to
      # an explicit table header later on, but only while it is still
      # implicit.
      promotable = !@declared && @promotable_later && kind == :explicit_table
      if @declared || !promotable
        raise KeyConflict.conflict(@key, source, @source)
      end

      @declared = true
      @kind = kind
      @source = source
    end
  end
end
