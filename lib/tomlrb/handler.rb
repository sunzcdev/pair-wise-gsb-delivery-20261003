# frozen-string-literal: true

module Tomlrb
  # A single component of a dotted key path, together with the place it
  # was declared. Keeping the quoting information around preserves the
  # difference between the bare path a.b and the quoted key "a.b".
  class KeyPart
    attr_reader :name, :lineno, :quoted

    def initialize(name, lineno = nil, quoted = false)
      @name = name.to_s
      @lineno = lineno
      @quoted = quoted
    end

    def to_s
      @name
    end

    def render
      @quoted ? @name.inspect : @name
    end
  end

  # Structured information about how a given key path segment was
  # declared the first time it became a real node.
  class Declaration
    KINDS = %i[value explicit_table implicit_parent inline_table array_table].freeze

    attr_reader :kind, :lineno

    def initialize(kind, lineno = nil)
      @kind = kind
      @lineno = lineno
    end

    def description
      case @kind
      when :value then 'a value'
      when :explicit_table then 'an explicit table'
      when :implicit_parent then 'an implicit parent table'
      when :inline_table then 'an enclosed inline table'
      when :array_table then 'an array of tables instance'
      end
    end

    def location
      @lineno ? "line #{@lineno}" : 'an unknown location'
    end
  end

  class Handler
    attr_reader :output, :symbolize_keys

    def initialize(**options)
      @output = {}
      @current = @output
      @stack = []
      @current_table = []
      @keys = Keys.new
      @symbolize_keys = options[:symbolize_keys]
    end

    def set_context(identifier_parts, is_array_of_tables: false)
      if identifier_parts.empty?
        raise ParseError, 'Array needs a name'
      end

      @current_table = Key.normalize_parts(identifier_parts)
      @keys.add_table_key identifier_parts, is_array_of_tables
      @current = @output

      deal_with_array_of_tables(@current_table.dup, is_array_of_tables) do |identifiers|
        identifiers.each do |k|
          k = k.to_s
          k = k.to_sym if @symbolize_keys
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
      last_identifier = identifiers.pop if is_array_of_tables

      yield(identifiers)

      if is_array_of_tables
        last_identifier = last_identifier.to_s
        last_identifier = last_identifier.to_sym if @symbolize_keys
        @current[last_identifier] ||= []
        raise ParseError, "Cannot use key #{last_identifier} for both table and array at once" unless @current[last_identifier].respond_to?(:<<)
        @current[last_identifier] << {}
        @current = @current[last_identifier].last
      end
    end

    def assign(k)
      key_parts = k.map { |part| part.is_a?(KeyPart) ? part : KeyPart.new(part) }
      value = @stack.pop
      @keys.add_pair_key key_parts, @current_table, terminal_kind(value)
      @keys.seal_inline_table(key_parts, @current_table.map(&:to_s)) if value.is_a?(Hash)
      current = @current
      while (key = key_parts.shift)
        key = key.to_s
        key = key.to_sym if @symbolize_keys
        current = assign_key_path(current, key, key_parts.empty?, value)
      end
    end

    def push(o)
      @stack << o
    end

    def push_inline(inline_arrays)
      merged_inline = {}
      validate_inline_arrays(inline_arrays)

      inline_arrays.each do |inline_array|
        current = merged_inline
        value = inline_array.pop
        parts = Key.normalize_parts(inline_array)

        parts.each_with_index do |inline_key, inline_index|
          raw_key = inline_key.to_s
          raw_key = raw_key.to_sym if @symbolize_keys
          last_key = inline_index == parts.size - 1

          if last_key
            current[raw_key] = value
          else
            current[raw_key] ||= {}
            current = current[raw_key]
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

        array.unshift(value)
      end
      array
    end

    def validate_value(value)
      if value.nil?
        raise ParseError, 'Value must be present'
      end
    end

    private

    def terminal_kind(value)
      value.is_a?(Hash) ? :inline_table : :value
    end

    def validate_inline_arrays(inline_arrays)
      groups = Hash.new { |hash, key| hash[key] = [] }
      inline_arrays.each do |array|
        value = array.last
        key_parts = array[0...-1].map { |part| part.is_a?(KeyPart) ? part : KeyPart.new(part) }
        if value.is_a?(Array)
          validate_inline_array_elements(value)
        else
          key = key_parts.first&.to_s
          groups[key] << [value, key_parts]
        end
      end
      groups.each_value do |entries|
        local_keys = Keys.new
        entries.each do |value, parts|
          kind = value.is_a?(Hash) ? :inline_table : :value
          local_keys.add_inline_pair_key parts, [], kind
          register_inline_hash(local_keys, value, parts) if value.is_a?(Hash)
        end
      end
    end

    def validate_inline_array_elements(array)
      array.each do |element|
        next unless element.is_a?(Hash)

        local_keys = Keys.new
        register_inline_hash(local_keys, element, [])
      end
    end

    def register_inline_hash(local_keys, hash, prefix)
      hash.each do |name, value|
        parts = prefix + [KeyPart.new(name)]
        kind = value.is_a?(Hash) ? :inline_table : :value
        if value.is_a?(Array)
          validate_inline_array_elements(value)
        else
          local_keys.add_inline_pair_key parts, [], kind
          register_inline_hash(local_keys, value, parts) if value.is_a?(Hash)
        end
      end
    end

    def assign_key_path(current, key, key_emptied, value)
      existed = current.key?(key)
      raise ParseError, "Cannot overwrite value with key #{key}" if existed && !current[key].is_a?(Hash)
      if key_emptied
        raise ParseError, "Cannot overwrite value with key #{key}" unless current.is_a?(Hash)

        raise ParseError, "Cannot overwrite value with key #{key}" if current[key].is_a?(Hash) && !value.is_a?(Hash)
        current[key] = value
        return current
      end
      current[key] ||= {}
      current[key]
    end
  end

  class Keys
    def initialize
      @root = Key.root
    end

    def add_table_key(parts, is_array_of_tables = false, in_array: false)
      parts = Key.normalize_parts(parts)
      @root.declare_tables(parts, is_array_of_tables, in_array: in_array)
    end

    def add_pair_key(parts, context, terminal_kind = :value)
      parts = Key.normalize_parts(parts)
      context = context.map(&:to_s)
      declare_with_context(parts, context, terminal_kind, false)
    end

    def add_inline_pair_key(parts, context, terminal_kind = :value)
      parts = Key.normalize_parts(parts)
      context = context.map(&:to_s)
      declare_with_context(parts, context, terminal_kind, true)
    end

    def seal_inline_table(parts, context)
      parts = Key.normalize_parts(parts)
      node = @root
      context.each do |context_part|
        node = node.find_child_string(context_part)
      end
      node.seal_path(parts)
    end

    private

    def declare_with_context(parts, context, terminal_kind, inline)
      node = @root
      context.each do |context_part|
        node = node.find_child_string(context_part)
      end
      method_name = inline ? :declare_inline_pair_path : :declare_pair_path
      node.public_send(method_name, parts, terminal_kind)
    end
  end

  class Key
    class KeyConflict < ParseError
      attr_reader :key_path, :current, :previous

      def self.raise!(key_path, current, previous)
        message =
          "Key #{key_path} is already used: " \
          "current declaration #{current.description} at #{current.location}, " \
          "previously declared #{previous.description} at #{previous.location}"
        raise new(message, key_path: key_path, current: current, previous: previous)
      end

      def initialize(message = nil, key_path: nil, current: nil, previous: nil)
        @key_path = key_path
        @current = current
        @previous = previous
        super(message)
      end
    end

    attr_reader :name, :declaration, :children
    alias key name

    def self.root
      new(nil)
    end

    def self.normalize_parts(parts)
      parts.map { |part| part.is_a?(KeyPart) ? part : KeyPart.new(part) }
    end

    def initialize(name, declaration = nil)
      @name = name
      @declaration = declaration
      @children = {}
    end

    def declared?
      !@declaration.nil?
    end

    def type
      case @declaration&.kind
      when :value then :pair
      when :explicit_table, :implicit_parent, :inline_table, :array_table then :table
      end
    end

    def declare_tables(parts, is_array_of_tables, in_array: false)
      part = parts.first
      rest = parts.drop(1)
      if rest.empty?
        kind = is_array_of_tables ? :array_table : :explicit_table
        node = traverse(part, Declaration.new(kind, part.lineno), in_array: in_array)
        node.clear_children if is_array_of_tables
        node
      else
        node = traverse(part, Declaration.new(:explicit_table, part.lineno), in_array: in_array, traversing: true)
        node.declare_tables(rest, is_array_of_tables, in_array: node.arrayish?)
      end
    end

    def open_context(part)
      existed = @children[part.to_s]
      return existed if existed

      node = self.class.new(part.to_s, Declaration.new(:implicit_parent, part.lineno))
      @children[part.to_s] = node
      node
    end

    def find_child_string(name)
      @children[name] || open_context(KeyPart.new(name))
    end

    def declare_pair_path(parts, terminal_kind)
      part = parts.first
      rest = parts.drop(1)
      if rest.empty?
        traverse(part, Declaration.new(terminal_kind, part.lineno))
      else
        node = traverse(part, Declaration.new(:implicit_parent, part.lineno))
        node.declare_pair_path(rest, terminal_kind)
      end
    end

    def declare_inline_pair_path(parts, terminal_kind)
      part = parts.first
      rest = parts.drop(1)
      if rest.empty?
        traverse(part, Declaration.new(terminal_kind, part.lineno))
      else
        node = traverse(part, Declaration.new(:implicit_parent, part.lineno))
        node.declare_inline_pair_path(rest, terminal_kind)
      end
    end

    def clear_children
      @children.clear
    end

    def seal_path(parts)
      part = parts.first
      rest = parts.drop(1)
      node = find_child_string(part.to_s)
      if rest.empty?
        node.seal!
        node
      else
        node.seal_path(rest)
      end
    end

    def seal!
      @sealed_inline = true
    end

    private

    def traverse(part, candidate, in_array: false, traversing: false)
      existed = @children[part.to_s]
      if existed
        existed.reconcile(part, candidate, in_array: in_array, traversing: traversing)
      else
        node = self.class.new(part.to_s, candidate)
        @children[part.to_s] = node
        node
      end
    end

    protected

    def reconcile(part, candidate, in_array: false, traversing: false)
      if @sealed_inline && candidate.kind != :implicit_parent
        KeyConflict.raise!(part.render, candidate, inline_declaration)
      end
      unless compatible?(candidate, in_array, traversing)
        KeyConflict.raise!(part.render, candidate, @declaration)
      end

      upgrade(candidate)
      if @sealed_inline && candidate.kind == :implicit_parent
        KeyConflict.raise!(part.render, candidate, inline_declaration)
      end
      self
    end

    def inline_declaration
      Declaration.new(:inline_table, @declaration&.lineno)
    end

    def compatible?(candidate, in_array = false, traversing = false)
      case candidate.kind
      when :implicit_parent
        tableish? || arrayish?
      when :array_table
        arrayish?
      when :explicit_table
        return true if traversing && tableish?
        return arrayish? if in_array

        @declaration.nil? || @declaration.kind == :implicit_parent
      when :inline_table
        !declared?
      else
        !declared?
      end
    end

    def upgrade(candidate)
      return if solid?

      @declaration = candidate unless candidate.kind == :implicit_parent
    end

    def solid?
      kind = @declaration&.kind
      kind && kind != :implicit_parent
    end

    def tableish?
      kind = @declaration&.kind
      kind == :explicit_table || kind == :implicit_parent ||
        kind == :inline_table || kind == :array_table
    end

    def arrayish?
      @declaration&.kind == :array_table
    end
  end
end
