# frozen_string_literal: true

module Tomlrb
  # Handler for the unchanged racc grammar that emits structural events
  # instead of materializing the whole document as a Hash.
  #
  # Every completed scalar or container is handed to the consumer exactly once
  # and is then replaced on the grammar stack by a small sentinel. After an
  # event has been emitted the handler only retains:
  #
  # - the {Keys} trie used for duplicate declaration checks,
  # - the active table path and array-of-tables element counters,
  # - open arrays/inline tables with their path and current element index,
  # - the grammar markers required to validate the syntax of what follows.
  class EventHandler
    # Grammar-stack markers for already streamed values. They are never
    # mistaken for keys because path collection only gathers String objects.
    ELEMENT = :__tomlrb_stream_element__.freeze

    # Returned in place of a completed array. #compact returns self so the
    # grammar's end_array `.compact` call keeps the sentinel instead of
    # collecting elements that were already streamed.
    class ContainerMarker
      def initialize(name)
        @name = name
      end

      def compact
        self
      end

      def to_s
        @name
      end
    end

    ARRAY_DONE = ContainerMarker.new('array')
    INLINE_DONE = ContainerMarker.new('inline')

    Frame = Struct.new(:kind, :path, :index, keyword_init: true)

    # Per-inline-table duplicate declaration tracker. It keeps key names and
    # their kind (pair/table) but never the values.
    class InlineChecker
      TABLE = :__tomlrb_inline_table__

      def initialize
        @root = {}
      end

      def register(components, container)
        node = @root
        components.each_with_index do |key, index|
          leaf = index == components.length - 1
          existing = node[key]

          if leaf
            raise Key::KeyConflict, "Inline key #{key} is already used" if existing

            node[key] = container ? TABLE : true
          else
            if existing == true
              raise Key::KeyConflict, "Inline key #{key} is already used"
            end

            node = node[key] ||= {}
          end
        end
      end
    end

    attr_reader :symbolize_keys

    # @param consumer [#call] callable receiving every emitted event
    def initialize(consumer:, **options)
      @consumer = consumer
      @symbolize_keys = options[:symbolize_keys]
      @stack = []
      @frames = []
      @keys = Keys.new
      @scope = []
      @current_table = []
      @array_names = []
      @array_indices = Hash.new(0)
      @inline_value_just_finished = false
    end

    # @api internal - grammar callback
    def start_(type)
      case type
      when :array
        enter_container(:array)
      when :inline
        path = pending_slot_path
        frame = Frame.new(kind: :inline, path: path, index: 0)
        parent = @frames.last
        parent.index += 1 if parent&.kind == :array
        @frames << frame
        @stack << frame << InlineChecker.new
        emit(:inline_table_start, ev_path(path))
      else
        @stack << type
      end
    end

    # @api internal - grammar callback
    def end_(type)
      case type
      when :array
        frame = leave_container(:array)
        emit(:array_end, ev_path(frame.path))
        ARRAY_DONE
      when :inline
        frame = leave_container(:inline)
        @stack.pop if @stack.last.is_a?(InlineChecker)
        emit(:inline_table_end, ev_path(frame.path))
        @inline_value_just_finished = true
        INLINE_DONE
      when :inline_keys
        finish_inline_pair
      else
        pop_until(type)
      end
    end

    # @api internal - grammar callback
    def push(object)
      frame = @frames.last
      if frame&.kind == :array && scalar?(object)
        path = frame.path + [frame.index]
        frame.index += 1
        emit(:array_element, ev_path(path), object)
        @stack << ELEMENT
      else
        @stack << object
      end
    end

    # @api internal - grammar callback
    def push_inline(_entries)
      # Content was streamed entry by entry; nothing real is retained.
      @stack << INLINE_DONE
      @inline_value_just_finished = true
      INLINE_DONE
    end

    # @api internal - grammar callback
    def validate_value(value)
      raise ParseError, 'Value must be present' if value.nil?
    end

    # @api internal - grammar callback
    def assign(keys)
      # +keys+ is the very array end_(:keys) returned and the stack copies
      # reference the same strings; capture the path before clearing either.
      path = table_path + keys
      value = @stack.pop
      pop_key_components
      return if value.nil?

      inline_value = @inline_value_just_finished || inside_inline?
      @inline_value_just_finished = false
      unless inline_value
        @keys.add_pair_key(keys.map(&:dup), @current_table)
        emit(:key, ev_path(path), value) unless streamed?(value)
      end
      value
    end

    # @api internal - grammar callback
    def set_context(identifiers, is_array_of_tables: false)
      raise ParseError, 'Array needs a name' if identifiers.empty?

      @keys.add_table_key(identifiers.dup, is_array_of_tables)
      @current_table = identifiers.dup

      if is_array_of_tables
        @array_names << identifiers.join('.')
        switch_scope(identifiers[0...-1], identifiers.last, true)
      else
        if @array_names.include?(identifiers.join('.'))
          raise ParseError, 'Cannot define a normal table with the same name as an already established array'
        end
        switch_scope(identifiers[0...-1], identifiers.last, false)
      end
      nil
    end

    # @api internal - builds a precise error for a construct left open when
    # EOF arrived, or +nil+ when the parse failed for an unrelated reason.
    def unclosed_structure_error
      return unclosed_error(:array) if open_frame?(:array)
      return unclosed_error(:inline) if open_frame?(:inline)
      return unclosed_error(:keys) if @stack.include?(:keys)
      return unclosed_error(:inline_keys) if @stack.include?(:inline_keys)
      return unclosed_error(:table) if @stack.include?(:table)
      return unclosed_error(:array_of_tables) if @stack.include?(:array_of_tables)

      nil
    end

    def open_frame?(kind)
      @stack.any? { |item| item.is_a?(Frame) && item.kind == kind }
    end

    public :unclosed_structure_error

    private

    def streamed?(value)
      value.equal?(ARRAY_DONE) || value.equal?(INLINE_DONE)
    end

    def scalar?(object)
      !object.equal?(ARRAY_DONE) && !object.equal?(INLINE_DONE) &&
        !object.is_a?(Frame) && !object.is_a?(InlineChecker) &&
        object != ELEMENT
    end

    def enter_container(kind)
      path = pending_slot_path
      frame = Frame.new(kind: kind, path: path, index: 0)
      parent = @frames.last
      parent.index += 1 if parent&.kind == :array
      @frames << frame
      @stack << frame
      emit(:"#{kind}_start", ev_path(path))
    end

    def leave_container(kind)
      frame = nil
      loop do
        raise unclosed_error(kind) if @stack.empty?

        item = @stack.pop
        if item.is_a?(Frame) && item.kind == kind
          frame = item
          break
        end
      end
      @frames.delete_if { |candidate| candidate.equal?(frame) }
      frame
    end

    # Grammar: `keys = end_(:inline_keys); value = keys.pop; push(value)`.
    # The returned array must expose the finished value through #pop while its
    # key components are consumed by the outer inline grammar.
    def finish_inline_pair
      items = pop_until(:inline_keys)
      value = items.pop
      components = items.grep(String)
      frame = innermost_inline_frame

      checker = nearest_checker(frame)
      checker&.register(components, streamed?(value))

      emit(:inline_table_entry, ev_path(frame.path + components), value) unless streamed?(value)

      [value]
    end

    def nearest_checker(frame)
      frame_index = @stack.rindex(frame)
      return nil unless frame_index

      @stack[(frame_index + 1)..].find { |item| item.is_a?(InlineChecker) }
    end

    def pop_until(marker)
      collected = []
      loop do
        raise unclosed_error(marker) if @stack.empty?

        item = @stack.pop
        break if item == marker

        collected.unshift(item) unless item.is_a?(InlineChecker)
      end
      collected
    end

    def unclosed_error(kind)
      name = {
        array: 'array',
        inline: 'inline table',
        table: 'table header',
        array_of_tables: 'array of tables header',
        keys: 'key-value pair',
        inline_keys: 'inline key-value pair'
      }.fetch(kind, kind.to_s)
      ParseError.new("Unclosed #{name} (end of file reached)")
    end

    # Absolute path of the slot a container start is about to occupy. The key
    # components currently being reduced are temporarily lifted off the stack
    # and put back afterwards so the grammar reduction is unaffected.
    def pending_slot_path
      frame = @frames.last
      return frame.path + [frame.index] if frame&.kind == :array

      if @stack.include?(:keys) || @stack.include?(:inline_keys)
        components = pop_key_components
        base = @frames.last ? @frames.last.path : table_path
        path = base + components
        components.each { |key| @stack << key }
        path
      else
        table_path
      end
    end

    def pop_key_components
      components = []
      while @stack.last.is_a?(String)
        components.unshift(@stack.pop)
      end
      components
    end

    def innermost_inline_frame
      @frames.reverse.find { |frame| frame.kind == :inline }
    end

    def inside_inline?
      !innermost_inline_frame.nil?
    end

    def table_path
      @scope.last ? @scope.last.path : []
    end

    # Moves from the currently active table to the table named by the header.
    # Only frames that are no longer active receive :table_leave; shared
    # parents are kept (leaving a table does not close it forever), and new
    # segments receive :table_enter.
    def switch_scope(parents, last_identifier, is_array_of_tables)
      prefix_size = common_prefix_size(parents)

      while @scope.any? &&
            string_projection(@scope.last.path).length > prefix_size
        frame = @scope.pop
        emit(:table_leave, ev_path(frame.path))
      end

      # The deepest remaining frame may be an array element (path ends in an
      # integer index); its full path - including that index - is the base.
      deepest_strings = @scope.any? ? string_projection(@scope.last.path) : []
      base = @scope.last ? @scope.last.path.dup : []
      new_parents =
        if deepest_strings.length >= prefix_size
          parents[prefix_size..] || []
        else
          parents
        end

      new_parents.each do |key|
        base += [key]
        enter_table(base.dup, :implicit)
      end

      if is_array_of_tables
        array_path = base + [last_identifier]
        index = @array_indices[array_path]
        @array_indices[array_path] += 1
        element_path = array_path + [index]
        enter_table(element_path, :array_element)
        emit(:table_declare, ev_path(element_path), :array_element)
      else
        base += [last_identifier]
        enter_table(base.dup, :standard)
        emit(:table_declare, ev_path(base), :standard)
      end
    end

    def common_prefix_size(parents)
      projected = @scope.any? ? string_projection(@scope.last.path) : []
      size = 0
      parents.each_with_index do |key, index|
        break unless projected[index] == key

        size = index + 1
      end
      size
    end

    def string_projection(path)
      path.select { |segment| segment.is_a?(String) }
    end

    def string_depth(path)
      path.count { |segment| segment.is_a?(String) }
    end

    def enter_table(path, mode)
      @scope << Frame.new(kind: :table, path: path, index: nil)
      emit(:table_enter, ev_path(path), mode)
    end

    def ev(key)
      @symbolize_keys ? key.to_sym : key
    end

    def ev_path(path)
      path.map { |segment| segment.is_a?(String) ? ev(segment) : segment }
    end

    def emit(*event)
      @consumer.call(event)
    end
  end
end
