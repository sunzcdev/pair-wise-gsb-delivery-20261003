# frozen_string_literal: true

require 'digest'
require 'set'

module Mail
  class Thread
    class Node
      attr_reader :id, :message, :messages, :parent, :children

      def initialize(id, messages = [])
        @id = id
        @messages = messages.dup.freeze
        @message = @messages.first
        @parent = nil
        @children = []
      end

      def missing?
        @message.nil?
      end
      alias_method :placeholder?, :missing?

      def duplicate?
        @messages.length > 1
      end

      def root?
        @parent.nil?
      end

      def freeze
        @messages.freeze unless @messages.frozen?
        @children.freeze unless @children.frozen?
        super
      end

      def assign_parent(parent)
        @parent = parent
      end

      def add_child(child)
        @children << child
      end
      private :assign_parent, :add_child
    end

    class Conflict
      attr_reader :type, :message_id, :parent_id, :rejected_parent_id, :details, :description

      def initialize(type, message_id:, parent_id: nil, rejected_parent_id: nil, details: {}, description: nil)
        @type = type
        @message_id = message_id
        @parent_id = parent_id
        @rejected_parent_id = rejected_parent_id
        @details = deep_freeze(details)
        @description = description || default_description
        freeze
      end

      def to_a
        [type, message_id, parent_id, rejected_parent_id, details, description]
      end

      private

      def default_description
        case type
        when :duplicate_message_id
          "Duplicate Message-ID #{message_id.inspect}; retaining the canonically ordered message."
        when :conflicting_parent
          "Conflicting parent for #{message_id.inspect}: using #{parent_id.inspect}, ignoring #{rejected_parent_id.inspect}."
        when :reference_cycle
          "Reference cycle #{details.fetch(:cycle).inspect}; breaking edge from #{rejected_parent_id.inspect} to #{message_id.inspect}."
        when :self_reference
          "Message-ID #{message_id.inspect} references itself in #{details.fetch(:field).inspect}."
        when :multiple_in_reply_to
          "Message-ID #{message_id.inspect} has multiple In-Reply-To values; using the first valid parent."
        when :malformed_header
          "Malformed #{details.fetch(:field).inspect} header on message #{message_id.inspect}."
        else
          "#{type}: #{message_id.inspect}"
        end
      end

      def deep_freeze(value)
        case value
        when Hash
          value.each { |key, item| deep_freeze(key); deep_freeze(item) }
          value.freeze
        when Array
          value.each { |item| deep_freeze(item) }
          value.freeze
        else
          value.freeze
        end
      end
    end

    Record = Struct.new(:message, :object_key, :id, :references, :in_reply_to, :content_key)

    attr_reader :roots, :nodes, :conflicts

    def self.build(*messages)
      new.add(*messages)
    end

    def initialize
      @seen_messages = {}.compare_by_identity
      @records = {}
      @header_conflicts = []
      @roots = [].freeze
      @nodes = [].freeze
      @conflicts = [].freeze
    end

    def add(*messages)
      messages.flatten.each do |message|
        add_message(message)
      end
      rebuild
      self
    end
    alias_method :<<, :add

    def find(message_id)
      @node_map.fetch(message_id, nil)
    end

    def size
      @nodes.length
    end

    private

    def add_message(message)
      return if @seen_messages.key?(message)

      @seen_messages[message] = true
      declared_id, id_conflicts = extract_message_id(message)
      id = declared_id || "mail-thread:synthetic:#{message.object_id}"
      references, reference_conflicts = extract_references(message, id, declared_id.nil?)
      in_reply_to, in_reply_to_conflicts = extract_in_reply_to(message, id, declared_id.nil?)
      record = Record.new(
        message,
        message.object_id,
        id,
        references,
        in_reply_to,
        content_fingerprint(message)
      )

      @records[id] ||= []
      @records[id] << record
      @header_conflicts.concat(id_conflicts + reference_conflicts + in_reply_to_conflicts)
    end

    def rebuild
      conflicts = @header_conflicts.dup
      canonical_records = canonicalize_records(conflicts)
      node_ids = collect_node_ids(canonical_records)
      parent_map, inferred_edges, direct_edges = build_parent_candidates(canonical_records)

      resolve_inferred_parents(parent_map, inferred_edges, direct_edges, conflicts)
      break_cycles(parent_map, node_ids, conflicts)

      nodes = build_nodes(canonical_records, node_ids, parent_map)
      sort_result(nodes, conflicts)
    end

    def canonicalize_records(conflicts)
      canonical_records = {}

      @records.each do |id, records|
        ordered = records.sort_by { |record| [record.content_key, record.object_key] }
        canonical = ordered.first
        canonical_records[id] = canonical

        ordered[1..].each do |record|
          conflicts << Conflict.new(
            :duplicate_message_id,
            message_id: id,
            details: {
              canonical_fingerprint: canonical.content_key,
              duplicate_fingerprint: record.content_key,
              identical: canonical.content_key == record.content_key,
              canonical_references: canonical.references,
              canonical_in_reply_to: canonical.in_reply_to,
              duplicate_references: record.references,
              duplicate_in_reply_to: record.in_reply_to
            }
          )
        end
      end

      canonical_records
    end

    def collect_node_ids(records)
      node_ids = records.keys.to_set

      records.each_value do |record|
        record.references.each { |id| node_ids << id }
        record.in_reply_to.each { |id| node_ids << id }
      end

      node_ids
    end

    def build_parent_candidates(records)
      parent_map = {}
      inferred_edges = Hash.new { |hash, key| hash[key] = Hash.new { |parents, parent| parents[parent] = [] } }
      direct_edges = {}

      records.each_value do |record|
        record.references.each_cons(2) do |parent_id, child_id|
          inferred_edges[child_id][parent_id] << record.id
        end

        if record.references.any?
          parent_map[record.id] = record.references.last
          direct_edges[record.id] = record.references.last
        elsif record.in_reply_to.any?
          parent_map[record.id] = record.in_reply_to.first
          direct_edges[record.id] = record.in_reply_to.first
        end
      end

      [parent_map, inferred_edges, direct_edges]
    end

    def resolve_inferred_parents(parent_map, inferred_edges, direct_edges, conflicts)
      inferred_edges.keys.sort.each do |child_id|
        parents = inferred_edges[child_id].keys.sort
        selected_parent = direct_edges[child_id] || parents.first

        parents.each do |parent_id|
          next if parent_id == selected_parent

          conflicts << Conflict.new(
            :conflicting_parent,
            message_id: child_id,
            parent_id: selected_parent,
            rejected_parent_id: parent_id,
            details: { sources: inferred_edges[child_id][parent_id].sort.uniq }
          )
        end

        parent_map[child_id] ||= selected_parent
      end
    end

    def break_cycles(parent_map, node_ids, conflicts)
      node_ids.length.times do
        removed = remove_one_cycle(parent_map, conflicts)
        break unless removed
      end
    end

    def remove_one_cycle(parent_map, conflicts)
      node_ids = parent_map.keys.sort

      node_ids.each do |start_id|
        path = []
        positions = {}
        current_id = start_id

        while current_id
          if positions.key?(current_id)
            cycle = path[positions[current_id]..-1]
            cycle_start = cycle.each_with_index.min_by(&:first).last
            ordered_cycle = cycle[cycle_start..-1] + cycle[0...cycle_start]
            dropped_child = cycle.max
            dropped_parent = parent_map[dropped_child]

            conflicts << Conflict.new(
              :reference_cycle,
              message_id: dropped_child,
              parent_id: nil,
              rejected_parent_id: dropped_parent,
              details: { cycle: ordered_cycle }
            )
            parent_map.delete(dropped_child)
            return true
          end

          positions[current_id] = path.length
          path << current_id
          current_id = parent_map[current_id]
        end
      end

      false
    end

    def build_nodes(records, node_ids, parent_map)
      node_map = {}

      node_ids.sort.each do |id|
        node_records = records.key?(id) ? @records.fetch(id).sort_by { |record| [record.content_key, record.object_key] }.map(&:message) : []
        node_map[id] = Node.new(id, node_records)
      end

      parent_map.each do |child_id, parent_id|
        child = node_map.fetch(child_id)
        parent = node_map.fetch(parent_id)
        child.send(:assign_parent, parent)
        parent.send(:add_child, child)
      end

      node_map.values.each do |node|
        node.children.sort_by!(&:id)
        node.freeze
      end

      @node_map = node_map
      node_map.values
    end

    def sort_result(nodes, conflicts)
      @nodes = nodes.sort_by(&:id).freeze
      @roots = @nodes.select(&:root?).freeze
      @conflicts = conflicts.sort_by do |conflict|
        conflict.to_a.map { |value| value.is_a?(Hash) ? value.inspect : value.to_s }
      end.freeze
    end

    def extract_message_id(message)
      ids, malformed = ids_from_field(message, 'Message-ID')
      id = ids.first unless ids.empty?
      conflicts = []

      if malformed
        conflicts << malformed_header_conflict('Message-ID', id || "mail-thread:synthetic:#{message.object_id}")
      end

      [id, conflicts]
    end

    def extract_references(message, id, synthetic)
      raw_ids, malformed = ids_from_field(message, 'References')
      conflicts = relation_conflicts(raw_ids, id, synthetic, 'References', malformed)
      unique_ids = relation_ids(raw_ids, id, synthetic)

      [unique_ids, conflicts]
    end

    def extract_in_reply_to(message, id, synthetic)
      ids, malformed = ids_from_field(message, 'In-Reply-To')
      conflicts = relation_conflicts(ids, id, synthetic, 'In-Reply-To', malformed)
      unique_ids = relation_ids(ids, id, synthetic)

      if unique_ids.length > 1
        conflicts << Conflict.new(:multiple_in_reply_to, message_id: id, details: { field: 'In-Reply-To' })
      end

      [unique_ids, conflicts]
    end

    def relation_conflicts(ids, id, synthetic, field_name, malformed)
      conflicts = []

      if malformed
        conflicts << malformed_header_conflict(field_name, id)
      end

      if !synthetic && ids.include?(id)
        conflicts << Conflict.new(:self_reference, message_id: id, details: { field: field_name })
      end

      conflicts
    end

    def relation_ids(ids, id, synthetic)
      unique_ids = ids.uniq
      return unique_ids if synthetic

      unique_ids.reject { |referenced_id| referenced_id == id }
    end

    def malformed_header_conflict(field_name, id)
      Conflict.new(
        :malformed_header,
        message_id: id,
        details: { field: field_name }
      )
    end

    def ids_from_field(message, field_name)
      field = message.header.fields.detect do |candidate|
        candidate.responsible_for?(field_name) &&
          !Mail::Utilities.blank?(candidate.unparsed_value)
      end
      return [[], false] unless field

      [Array(field).first.message_ids.reject { |id| id.nil? || id.to_s.strip.empty? }, false]
    rescue => error
      raw_value =
        if field.nil?
          ''
        elsif field.respond_to?(:unparsed_value)
          field.unparsed_value.to_s
        else
          Array(field).first.to_s
        end
      [parse_message_ids(raw_value), true].tap do |ids, _|
        ids.define_singleton_method(:parse_error) { error.class.name }
      end
    end

    def parse_message_ids(value)
      value.to_s.scan(/<([^>\s]+)>/).flatten
    end

    def content_fingerprint(message)
      digest = Digest::SHA256.new
      ignored_fields = %w[date received message-id]

      message.header.fields.map do |field|
        next if ignored_fields.include?(field.name.to_s.downcase)

        [field.name.to_s.downcase, field.unparsed_value.to_s]
      end.compact.sort.each do |name, value|
        digest.update(name.b)
        digest.update("\0".b)
        digest.update(value.b)
        digest.update("\n".b)
      end

      raw_body = message.body.instance_variable_get(:@raw_source)
      raw_body = message.body.to_s if raw_body.nil?
      digest.update("body\0".b)
      digest.update(raw_body.to_s.b)
      digest.hexdigest
    end

  end
end
