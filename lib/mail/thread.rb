# encoding: utf-8
# frozen_string_literal: true

module Mail

  # Mail::Thread arranges a set of Mail::Message objects into a read-only
  # conversation tree, using only their Message-ID, References and
  # In-Reply-To headers. Messages are never merged by subject similarity,
  # and neither Date headers nor the system clock are ever consulted, so
  # the resulting structure depends only on the set of messages, not on
  # the order in which they were added.
  #
  # == Parent resolution
  #
  # * If a message has a References header, its ids form an ancestor
  #   chain: the last id is the direct parent, earlier ids are its
  #   ancestors. Missing ids become placeholder nodes.
  # * Otherwise, if it has an In-Reply-To header, the last id is used as
  #   the parent (a note is recorded when several ids are present).
  # * Otherwise the message is a root of the conversation.
  #
  # == Duplicate Message-IDs
  #
  # The first message added with a given Message-ID wins. Later messages
  # with the same id are ignored and a note is recorded on the node.
  #
  # == Placeholder nodes
  #
  # Referenced ids that have not been seen yet become placeholder nodes
  # (Node#placeholder?). A placeholder is filled in when the real message
  # arrives and keeps its children, so adding a late parent afterwards
  # yields the same tree as building from the complete set at once.
  # Placeholders that end up with neither a message nor children are
  # pruned.
  #
  # == Conflict resolution
  #
  # * A real message's own headers always decide its parent, overriding
  #   any position inferred from other messages' References (noted).
  # * When two messages imply different parents for the same placeholder,
  #   the lexicographically smaller parent id wins (noted).
  # * Self references are ignored (noted).
  # * Reference cycles are broken by keeping (or moving) the
  #   lexicographically smaller id as the ancestor, even when that
  #   means moving a node that was already placed by its own headers
  #   (noted).
  #
  # All conflict notes are available per node via Node#notes and
  # aggregated for the whole tree via Thread#conflicts.
  #
  # == Read-only result
  #
  # The tree is built only through #add / #<<. Node exposes a read-only
  # API; Node#children and Thread#roots return sorted, frozen arrays.
  # Children and roots are sorted by Message-ID so the presentation is
  # stable regardless of insertion order.
  #
  # == Usage
  #
  #   thread = Mail::Thread.new(messages)   # or Mail::Thread.new + #add
  #   thread.add(message)                   # incremental; late parents OK
  #   thread.roots                          # => [Mail::Thread::Node, ...]
  #   thread['id@example']                  # => node or nil
  #   thread.conflicts                      # => [String, ...]
  class Thread
    include Enumerable

    # A node in a conversation tree. Nodes are read-only: the tree is
    # mutated exclusively by Mail::Thread while building.
    class Node
      include Enumerable

      # The normalized Message-ID of this node (String), or nil for a
      # message that has no Message-ID header.
      attr_reader :message_id

      # The Mail::Message held by this node, or nil while it is a
      # placeholder for a message that has not arrived (yet).
      attr_reader :message

      # The parent node, or nil when this node is a conversation root.
      attr_reader :parent

      # Conflict notes recorded for this node (Array of String).
      attr_reader :notes

      def initialize(message_id, message, sequence) # :nodoc:
        @message_id = message_id
        @message = message
        @sequence = sequence
        @parent = nil
        @children = []
        @notes = []
        @locked = false
      end

      # True when this node stands in for a message that was referenced
      # but has not been added to the thread.
      def placeholder?
        @message.nil?
      end

      def root?
        @parent.nil?
      end

      # Child nodes, sorted by Message-ID. The returned array is frozen.
      def children
        @children.sort_by { |child| [child.message_id || '', child.send(:sequence)] }.freeze
      end

      # Depth of this node within its conversation (roots are 0).
      def depth
        depth = 0
        node = self
        while (node = node.parent)
          depth += 1
        end
        depth
      end

      # Pre-order traversal of this node and its descendants.
      def each(&block)
        return enum_for(:each) unless block_given?
        yield self
        children.each { |child| child.each(&block) }
      end

      # Display label used in conflict notes, e.g. "<abc@example>".
      def label
        message_id ? "<#{message_id}>" : '(message without Message-ID)'
      end

      def inspect
        format('#<%s id=%p placeholder=%p children=%d>', self.class.name, message_id, placeholder?, @children.size)
      end

      private

      attr_reader :sequence
      attr_writer :message
      attr_writer :parent

      def lock!
        @locked = true
      end

      def locked?
        @locked
      end

      def add_child(node)
        @children << node
      end

      def remove_child(node)
        @children.delete(node)
      end

      def add_note(text)
        @notes << text
      end
    end

    def self.build(messages = [])
      new(messages)
    end

    def initialize(messages = [])
      @nodes = {}     # normalized Message-ID => Node (real messages and placeholders)
      @anonymous = [] # nodes for messages without a Message-ID
      @sequence = 0
      messages.each { |message| add(message) }
    end

    # Add a message to the thread. Returns the Node that represents it
    # (the existing node when the Message-ID was already present).
    def add(message)
      id = self.class.normalize_id(safe_field(message, :message_id))
      references = self.class.extract_ids(message, :references)
      in_reply_to = self.class.extract_ids(message, :in_reply_to)

      if id && (node = @nodes[id])
        if node.message
          record(node, "duplicate Message-ID #{node.label}: keeping the first copy, ignoring a later message")
          return node
        end
        node.send(:message=, message)
      elsif id
        node = Node.new(id, message, next_sequence)
        @nodes[id] = node
      else
        node = Node.new(nil, message, next_sequence)
        @anonymous << node
      end

      place(node, references, in_reply_to)
      node
    end

    def <<(message)
      add(message)
      self
    end

    # Find a node by Message-ID (with or without angle brackets).
    def find(message_id)
      @nodes[self.class.normalize_id(message_id)]
    end
    alias_method :[], :find

    # Root nodes of all conversations, sorted by Message-ID, frozen.
    def roots
      all_nodes.select(&:root?).sort_by { |node| [node.message_id || '', node.send(:sequence)] }.freeze
    end

    # Pre-order traversal over every node, placeholders included.
    def each(&block)
      return enum_for(:each) unless block_given?
      roots.each { |root| root.each(&block) }
    end

    # Number of real messages held by the thread (placeholders and
    # ignored duplicates are not counted).
    def size
      @anonymous.size + @nodes.values.count { |node| !node.placeholder? }
    end

    # All conflict notes recorded while building, oldest conversation
    # first, as "label: note" strings.
    def conflicts
      list = []
      each do |node|
        node.notes.each { |note| list << "#{node.label}: #{note}" }
      end
      list
    end

    def inspect
      format('#<%s messages=%d roots=%d>', self.class.name, size, roots.size)
    end

    # Normalize a Message-ID: strip whitespace and angle brackets.
    # Returns nil when nothing usable remains.
    def self.normalize_id(value)
      return nil if value.nil?
      id = value.to_s.strip
      id = id[1..-2].to_s.strip if id.length > 1 && id.start_with?('<') && id.end_with?('>')
      id.empty? ? nil : id
    end

    # Extract a list of normalized ids from a References/In-Reply-To
    # style accessor, tolerating String, Array and nil results as well
    # as messages whose header parsing raises.
    def self.extract_ids(message, field)
      value = message.public_send(field)
      list = value.is_a?(Array) ? value : [value]
      list.map { |item| normalize_id(item) }.compact
    rescue StandardError
      []
    end

    private

    def all_nodes
      @nodes.values + @anonymous
    end

    def next_sequence
      @sequence += 1
    end

    def safe_field(message, field)
      message.public_send(field)
    rescue StandardError
      nil
    end

    def node_for(id)
      @nodes[id] ||= Node.new(id, nil, next_sequence)
    end

    def record(node, text)
      node.send(:add_note, text)
    end

    # Resolve the parent of +node+ from its own headers and lock the
    # placement. Called exactly once per kept message.
    def place(node, references, in_reply_to)
      chain = references
      if chain.empty? && !in_reply_to.empty?
        if in_reply_to.size > 1
          record(node, "multiple In-Reply-To ids; using the last one <#{in_reply_to.last}> as parent")
        end
        chain = [in_reply_to.last]
      end

      if node.message_id && chain.include?(node.message_id)
        record(node, "self-reference #{node.label} ignored")
        chain = chain - [node.message_id]
      end

      if chain.empty?
        if node.parent
          record(node, "has no References/In-Reply-To of its own; detached from inferred parent #{node.parent.label}")
          detach(node)
        end
        node.send(:lock!)
        return
      end

      previous = nil
      chain.each do |ref_id|
        ref_node = node_for(ref_id)
        link(previous, ref_node) if previous
        previous = ref_node
      end
      link(previous, node, force: true)
      node.send(:lock!)
    end

    # Establish (or reconcile) a parent/child edge inferred from
    # References. With force: true the edge comes from the child's own
    # headers and always wins over an inferred position.
    def link(parent, child, force: false)
      return if parent.nil? || parent.equal?(child)

      if child.send(:locked?)
        unless child.parent.equal?(parent)
          current = child.parent ? child.parent.label : 'root'
          record(child, "inferred parent #{parent.label} ignored; already placed under #{current} by its own headers")
        end
        return
      end

      if ancestor?(child, parent)
        break_cycle(parent, child)
        return
      end

      current = child.parent
      if current.nil?
        attach(parent, child)
      elsif current.equal?(parent)
        # already in place
      elsif force
        record(child, "placed under #{parent.label} by its own headers, overriding inferred parent #{current.label}")
        attach(parent, child)
      elsif smaller_id?(parent.message_id, current.message_id)
        record(child, "conflicting inferred parents: #{parent.label} wins over #{current.label} (smaller id)")
        attach(parent, child)
      else
        record(child, "conflicting inferred parents: keeping #{current.label}, ignoring #{parent.label} (smaller id wins)")
        prune(parent)
      end
    end

    # child is an ancestor of parent, so linking them would create a
    # cycle. Break it deterministically: the lexicographically smaller id
    # ends up as the ancestor.
    def break_cycle(parent, child)
      if smaller_or_equal_id?(child.message_id, parent.message_id)
        record(child, "reference cycle with #{parent.label}: keeping #{child.label} as ancestor (smaller id), ignoring the edge")
        prune(parent)
        return
      end
      record(child, "reference cycle with #{parent.label}: moving smaller id #{parent.label} above #{child.label}")
      old_parent = child.parent
      detach(parent)
      attach(old_parent, parent) if old_parent
      attach(parent, child)
    end

    def ancestor?(ancestor, node)
      while node
        return true if node.equal?(ancestor)
        node = node.parent
      end
      false
    end

    def attach(parent, child)
      old = child.parent
      if old
        old.send(:remove_child, child)
        child.send(:parent=, nil)
      end
      child.send(:parent=, parent)
      parent.send(:add_child, child)
      prune(old) if old
    end

    def detach(child)
      old = child.parent
      return unless old
      old.send(:remove_child, child)
      child.send(:parent=, nil)
      prune(old)
    end

    # Remove placeholders that have neither a message nor children.
    def prune(node)
      while node && node.placeholder? && node.children.empty?
        parent = node.parent
        node.send(:parent=, nil)
        parent.send(:remove_child, node) if parent
        @nodes.delete(node.message_id) if node.message_id
        node = parent
      end
    end

    def smaller_id?(left, right)
      (left || '') < (right || '')
    end

    def smaller_or_equal_id?(left, right)
      (left || '') <= (right || '')
    end
  end
end