# frozen_string_literal: true

module Grape
  class Entity
    # Graph representation mode.
    #
    # Instead of nesting associated entities inline (which loops forever on
    # cyclic object graphs and duplicates shared objects), graph mode emits a
    # flat structure:
    #
    #   { data: { _ref: 'UserEntity:1' },
    #     included: { 'UserEntity:1' => { id: 1, ... }, ... } }
    #
    # Entities declare their business identity with +identity :key+. Every
    # occurrence of an entity with a declared identity is rendered as a link
    # (+{ _ref: '...' }+); the full representation of the first occurrence is
    # stored once in the +included+ collection, keyed by the same reference.
    #
    # Entities sharing the same identity are expanded only once, even when
    # they are different Ruby objects. If two objects with the same identity
    # serialize to different content, a ConflictError is raised instead of
    # silently picking one of them.
    module Graph
      # Raised when two objects share the same declared identity but their
      # representations differ.
      class ConflictError < StandardError; end

      # Tracks the entities already seen while representing an object graph
      # and collects their full representations for the +included+ collection.
      class Registry
        attr_reader :included

        def initialize
          @bodies = {}   # ref key => serialized representation
          @labels = {}   # ref key => String reference used in links
          @pending = {}  # ref key => true while its representation is being built
          @included = {} # String reference => serialized representation
          @comparing = 0
        end

        # Returns a link for the given object (or an array of links for
        # collections, or nil for nil), registering first-seen entities into
        # the +included+ collection.
        def reference_for(entity_class, object, options)
          return nil if object.nil?

          if object.respond_to?(:to_ary)
            item_options = options.reverse_merge(collection: true)
            return object.to_ary.map { |item| reference_for(entity_class, item, item_options) }
          end

          register(entity_class, object, options)
        end

        private

        def register(entity_class, object, options)
          ref_key, label = reference_for_object(entity_class, object)

          if @bodies.key?(ref_key)
            verify_consistency!(entity_class, object, options, ref_key, label) if verifiable?(ref_key)
            return link(label)
          end

          # Already being expanded (cyclic graph) or currently compared
          # against a stored representation: emit the link only.
          return link(label) if @pending[ref_key] || @comparing.positive?

          @pending[ref_key] = true
          begin
            @bodies[ref_key] = entity_class.new(object, options).serializable_hash
          ensure
            @pending.delete(ref_key)
          end
          @labels[ref_key] = label
          @included[label] = @bodies[ref_key]

          link(label)
        end

        def verifiable?(ref_key)
          @comparing.zero? && !@pending[ref_key]
        end

        # Re-serializes the candidate object and compares it against the
        # stored representation. Nested associations are rendered as links
        # during comparison, so only the entity's own content is checked.
        def verify_consistency!(entity_class, object, options, ref_key, label)
          @comparing += 1
          begin
            candidate = entity_class.new(object, options).serializable_hash
          ensure
            @comparing -= 1
          end

          return if candidate == @bodies[ref_key]

          raise ConflictError,
                "Conflicting representations for #{label}: " \
                "#{candidate.inspect} does not match #{@bodies[ref_key].inspect}"
        end

        def reference_for_object(entity_class, object)
          delegator = Delegator.new(object)
          values = entity_class.identity.map do |key|
            if delegator.accepts_options?
              delegator.delegate(key, **entity_class.delegation_opts)
            else
              delegator.delegate(key)
            end
          end

          label = "#{entity_class.name || entity_class.inspect}:#{values.join('.')}"
          [[entity_class, values], label]
        end

        def link(label)
          { _ref: label }
        end
      end

      # Represents +objects+ with +entity_class+ in graph mode.
      #
      # @return [Hash] +{ data: ..., included: { ref => representation } }+
      def self.represent(entity_class, objects, options)
        registry = Registry.new
        inner_options = options.reject { |key, _| key == :graph }.merge(graph_registry: registry)

        data =
          if entity_class.identity
            registry.reference_for(entity_class, objects, Options.new(inner_options))
          elsif objects.respond_to?(:to_ary) && !entity_class.instance_variable_get(:@present_collection)
            objects.to_ary.map do |object|
              entity_class.new(object, inner_options.reverse_merge(collection: true)).serializable_hash
            end
          else
            entity_class.new(objects, inner_options).serializable_hash
          end

        { data: data, included: registry.included }
      end
    end
  end
end
