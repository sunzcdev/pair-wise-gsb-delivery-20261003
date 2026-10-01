# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class NestingExposure
        class OutputBuilder < SimpleDelegator
          def initialize(entity, options = nil)
            @entity = entity
            @options = options
            @strict = !options.nil? && !!options[:strict_conflicts]
            @output_hash = {}
            @output_collection = []

            if @strict
              validate_conflict_resolvers!
              # Tracks which exposure produced each key that actually takes
              # part in the representation, so conflicts can report both
              # source paths instead of silently overwriting a value.
              @sources = {}
            end

            super(entity)
          end

          def add(exposure, result)
            # Save a result array in collections' array if it should be merged
            if result.is_a?(Array) && exposure.for_merge
              @output_collection << result
            elsif exposure.for_merge
              # If we have an array which should not be merged - save it with a key as a hash
              # If we have hash which should be merged - save it without a key (merge)
              return unless result

              if strict?
                add_merged_keys(exposure, result)
              else
                @output_hash.merge! result, &merge_strategy(exposure.for_merge)
              end
            elsif strict?
              write_key(exposure.key(@entity), result, exposure)
            else
              @output_hash[exposure.key(@entity)] = result
            end
          end

          def kind_of?(klass)
            klass == output.class || super
          end
          alias is_a? kind_of?

          def __getobj__
            output
          end

          private

          def strict?
            @strict
          end

          # If output_collection contains at least one element we have to represent the output as a collection
          def output
            if @output_collection.empty?
              output = @output_hash
            else
              output = @output_collection
              output << @output_hash unless @output_hash.empty?
              output.flatten!
            end
            output
          end

          # In case if we want to solve collisions providing lambda to :merge option
          def merge_strategy(for_merge)
            if for_merge.respond_to? :call
              for_merge
            else
              -> {}
            end
          end

          # A `merge: ->(key, old_val, new_val) { ... }` callable is an explicit
          # adjudication declared by the caller for the merged keys, so it is
          # honored in strict mode. A bare `merge: true` is not.
          def add_merged_keys(exposure, result)
            adjudicator = exposure.for_merge if exposure.for_merge.respond_to?(:call)
            result.each do |key, value|
              write_key(key, value, exposure, adjudicator: adjudicator)
            end
          end

          def write_key(key, value, exposure, adjudicator: nil)
            unless @sources.key?(key)
              @output_hash[key] = value
              @sources[key] = source_path_for(exposure)
              return
            end

            existing_source = @sources[key]
            incoming_source = source_path_for(exposure)

            if adjudicator
              @output_hash[key] = adjudicator.call(key, @output_hash[key], value)
            else
              resolver = resolver_for(key)
              raise Grape::Entity::KeyConflictError.new(key, [existing_source, incoming_source]) unless resolver

              @output_hash[key] = resolver.call(key, @output_hash[key], value, [existing_source, incoming_source])
            end
            @sources[key] = "#{incoming_source} (adjudicated)"
          end

          def resolver_for(key)
            resolvers = @options[:conflict_resolvers]
            return unless resolvers

            resolvers[key] ||
              (key.respond_to?(:to_sym) && resolvers[key.to_sym]) ||
              (key.respond_to?(:to_s) && resolvers[key.to_s]) ||
              nil
          end

          # Adjudication must be declared per key. A single callable applied to
          # the whole output would be an indiscriminate last-write-wins rule,
          # which is exactly what strict conflict mode exists to prevent.
          def validate_conflict_resolvers!
            resolvers = @options[:conflict_resolvers]
            return if resolvers.nil?

            unless resolvers.is_a?(Hash) && resolvers.values.all? { |resolver| resolver.respond_to?(:call) }
              raise ArgumentError,
                    '`conflict_resolvers` must be a Hash mapping output keys to callables, ' \
                    'e.g. `{ name: ->(key, existing, incoming, sources) { ... } }`. ' \
                    'Blanket resolution rules are not supported in strict conflict mode.'
            end
          end

          # Builds a path identifying the exposure a key came from, e.g.
          # `MyEntity.title (as: name)`, `MyEntity.extra (merge)` or, for
          # collection elements, `MyEntity.name[2]`.
          def source_path_for(exposure)
            entity_name = @entity.class.name || @entity.class.to_s
            attribute = exposure.attribute || exposure.key(@entity)
            path = +"#{entity_name}.#{attribute}"

            if exposure.for_merge
              path << ' (merge)'
            else
              key = exposure.key(@entity)
              path << " (as: #{key})" if key.to_s != attribute.to_s
            end

            index = @entity.options[:collection_index]
            path << "[#{index}]" if index
            path
          end
        end
      end
    end
  end
end
