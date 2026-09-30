# frozen_string_literal: true

require 'set'

module Grape
  class Entity
    # A Graph renders an object network with bidirectional references as a
    # finite document. Associations point at their targets through links and
    # the first full rendering of every business identity is collected once.
    #
    # Entities opt into the graph by declaring an +identity+ (one or more
    # attributes that uniquely identify the represented business object).
    class Graph
      class Error < StandardError; end

      # Raised when two distinct Ruby objects report the same business
      # identity but render different content. The representation refuses to
      # pick one of the conflicting versions silently.
      class ConflictError < Error; end

      # Raised when an association targets an entity class that did not
      # declare an +identity+; without one a stable link cannot be emitted.
      class MissingIdentityError < Error; end

      def initialize
        @payloads = {}
        @building = Set.new
        @root_keys = []
        @pending = Hash.new { |hash, key| hash[key] = [] }
        @readonly_depth = 0
      end

      # Renders a collection root. Each entry becomes a full payload in
      # +data+, every other reachable entity lands in +included+.
      def render(objects, entity_class, options)
        list = objects.to_ary
        data = list.map { |object| render_root(object, entity_class, options) }
        drain_pending_checks
        { data: data, included: included_payloads }
      end

      # Renders a single root object as a full payload in +data+.
      def render_one(object, entity_class, options)
        data = render_root(object, entity_class, options)
        drain_pending_checks
        { data: data, included: included_payloads }
      end

      # Emits a link for an association value, fully expanding the target the
      # first time its identity is reached. Returns +nil+ for +nil+, a link
      # hash for a single object, or an array of links for a collection.
      def embed(value, entity_class, options)
        return if value.nil?
        return value.to_ary.map { |item| embed_one(item, entity_class, options) } if value.respond_to?(:to_ary)

        embed_one(value, entity_class, options)
      end

      # While true, an association target is rendered for comparison only:
      # links are still produced but no new payload is registered, which keeps
      # the check itself from mutating the document or looping forever.
      def readonly?
        @readonly_depth.positive?
      end

      private

      def render_root(object, entity_class, options)
        key = entity_class.graph_key(object)

        if @payloads.key?(key)
          verify!(key, object, entity_class, options)
        else
          @root_keys << key
          build_and_store(key, object, entity_class, options)
        end
      end

      def embed_one(object, entity_class, options)
        key = entity_class.graph_key(object)
        link = link_for(key, entity_class)

        if @payloads.key?(key)
          verify!(key, object, entity_class, options) unless same_object?(key, object)
        elsif readonly?
          # The target identity is not part of the document yet and this is a
          # comparison pass: emit the link without registering or expanding
          # it, otherwise the check could mutate the graph or loop forever.
        elsif @building.include?(key)
          # Another object with this identity is currently expanding. Emit the
          # link now and compare the contents once expansion finishes.
          @pending[key] << [object, entity_class, options] unless same_object?(key, object)
        else
          build_and_store(key, object, entity_class, options)
        end

        link
      end

      def build_and_store(key, object, entity_class, options)
        @payloads[key] = nil
        stored_objects[key] = object
        @building.add(key)

        begin
          @payloads[key] = entity_class.new(object, options).graph_payload(options)
        ensure
          @building.delete(key)
        end

        run_pending_for(key)
        @payloads[key]
      end

      # Compares a newly reached object for an existing identity against the
      # payload that was rendered first. A mismatch is a data conflict rather
      # than a duplicate reference.
      def verify!(key, object, entity_class, options)
        pair = [key, object.object_id]
        @seen ||= {}
        return if @seen[pair]

        @seen[pair] = true

        stored = @payloads[key]
        if stored.nil?
          @pending[key] << [object, entity_class, options]
          return
        end

        return if same_object?(key, object)

        candidate = readonly_build(object, entity_class, options)
        raise ConflictError, conflict_message(key, entity_class) unless candidate == stored
      end

      def run_pending_for(key)
        pending = @pending.delete(key) || []
        pending.each do |object, entity_class, options|
          next if same_object?(key, object)

          candidate = readonly_build(object, entity_class, options)
          raise ConflictError, conflict_message(key, entity_class) unless candidate == @payloads[key]
        end
      end

      # Any identity still pending at the end was never completed on the main
      # pass; verify it now without registering anything.
      def drain_pending_checks
        @pending.each_key { |key| run_pending_for(key) }
      end

      def readonly_build(object, entity_class, options)
        @readonly_depth += 1
        begin
          entity_class.new(object, options).graph_payload(options)
        ensure
          @readonly_depth -= 1
        end
      end

      def same_object?(key, object)
        stored_objects[key].equal?(object)
      end

      def stored_objects
        @stored_objects ||= {}
      end

      def link_for(key, entity_class)
        type = entity_class.graph_type
        id = key.size == 2 ? key.last : key[1..]
        { type: type, id: id }
      end

      def included_payloads
        @payloads.each_with_object([]) do |(key, payload), list|
          next if @root_keys.include?(key) || payload.nil?

          list << payload
        end
      end

      def conflict_message(key, entity_class)
        type = entity_class.graph_type
        id = key.size == 2 ? key.last : key[1..]
        "Conflicting representations for #{type} #{id.inspect}: the same identity produced different content"
      end
    end
  end
end
