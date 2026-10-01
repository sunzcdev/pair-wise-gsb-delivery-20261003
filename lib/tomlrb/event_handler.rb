# frozen-string-literal: true

module Tomlrb
  # A Handler that emits parse events to a consumer instead of building
  # the whole document as a nested Hash.
  #
  # Events are arrays of one of these shapes:
  #
  #   [:table, path]            a [table] declaration was parsed
  #   [:array_of_tables, path]  a [[table]] declaration was parsed; each
  #                             event opens a new element of the array
  #   [:value, path, value]     a key/value pair was completed; path is the
  #                             full dotted path from the document root
  #
  # Protocol notes:
  #
  # * There is intentionally no "table closed" event. A table declaration
  #   only switches the context that following :value events belong to; it
  #   does not mean the previously current table is finished forever. A
  #   normal table may be declared *after* values under its path were
  #   already emitted ([a.b] ... [a]), and an array of tables may gain a
  #   new element (and new sub-tables) at any later [[...]] header.
  # * Only explicit [...]/[[...]] headers produce declaration events.
  #   Tables created implicitly on the way to a declared one do not.
  # * Nested containers (arrays, inline tables) are delivered as a single
  #   :value event once the container is complete.
  # * A value is handed to the consumer exactly once. The handler keeps no
  #   reference to it: the only retained state is what syntax and
  #   duplicate-declaration checks require (key names, container kinds and
  #   the current table path), so processed values can be garbage
  #   collected. See the shape tree below.
  class EventHandler < Handler
    # Marker stored in the shape tree where the full parser would store a
    # terminal (scalar or array) value.
    VALUE = Object.new

    # Shape of an array of tables. Only the current element is kept:
    # previous elements are forgotten as soon as a new one starts.
    class ArrayShape
      attr_reader :element

      def initialize
        new_element!
      end

      def new_element!
        @element = {}
      end
    end

    def initialize(consumer, **options)
      @consumer = consumer
      @current_table = []
      @stack = []
      @array_names = []
      @keys = Keys.new
      @root = {}
      @current = @root
      @symbolize_keys = options[:symbolize_keys]
    end

    def set_context(identifiers, is_array_of_tables: false)
      if identifiers.empty?
        raise ParseError, 'Array needs a name'
      end

      @current_table = identifiers.dup
      @keys.add_table_key identifiers, is_array_of_tables
      @current = @root

      deal_with_array_of_tables(identifiers, is_array_of_tables) do |identifierz|
        identifierz.each do |k|
          node = @current[k]
          @current = case node
                     when ArrayShape then node.element
                     when nil then @current[k] = {}
                     else node
                     end
        end
      end

      emit(is_array_of_tables ? :array_of_tables : :table, @current_table)
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
        node = @current[last_identifier]
        if node.nil?
          node = @current[last_identifier] = ArrayShape.new
        elsif !node.is_a?(ArrayShape)
          raise ParseError, "Cannot use key #{last_identifier} for both table and array at once"
        end
        node.new_element!
        @current = node.element
      end
    end

    def assign(k)
      @keys.add_pair_key k, @current_table
      path = @current_table + k
      current = @current
      while (key = k.shift)
        current = assign_key_path(current, key, k.empty?, path)
      end
    end

    private

    def assign_key_path(current, key, key_emptied, path)
      existed = current.key?(key)
      node = current[key]
      raise ParseError, "Cannot overwrite value with key #{key}" if existed && !node.is_a?(Hash)
      if key_emptied
        raise ParseError, "Cannot overwrite value with key #{key}" unless current.is_a?(Hash)

        value = @stack.pop
        raise ParseError, "Cannot overwrite value with key #{key}" if node.is_a?(Hash) && !value.is_a?(Hash)
        current[key] = shape_of(value)
        emit(:value, path, value)
        return current
      end
      current[key] ||= {}
      current[key]
    end

    # Records only the kind of each assigned value in the shape tree:
    # inline tables keep their key structure (they may be extended by
    # later dotted keys, mirroring Handler), anything else is a terminal
    # VALUE marker. The value itself is never retained.
    def shape_of(value)
      if value.is_a?(Hash)
        value.each_with_object({}) { |(k, v), shape| shape[k.to_s] = shape_of(v) }
      else
        VALUE
      end
    end

    def emit(type, path, value = nil)
      path = path.map(&:to_sym) if @symbolize_keys
      event = value.nil? ? [type, path] : [type, path, value]
      @consumer.call(event)
    end
  end
end
