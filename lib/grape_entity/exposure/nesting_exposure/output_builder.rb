# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class NestingExposure
        class OutputBuilder < SimpleDelegator
          def initialize(entity, options = nil)
            @entity = entity
            @options = options
            @output_hash = {}
            @output_collection = []
            @key_sources = {}

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

              add_merged(exposure, result)
            else
              add_keyed(exposure, result)
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

          def add_keyed(exposure, result)
            key = exposure.key(@entity)
            existing_key = conflicting_key(key)

            if existing_key.nil?
              @output_hash[key] = result
              track_source(key, exposure)
            else
              @output_hash[existing_key] = adjudicate(existing_key, exposure, @output_hash[existing_key], result)
            end
          end

          def add_merged(exposure, result)
            # A callable :merge option is already an explicit adjudication
            # strategy, so it stays allowed even in strict mode.
            unless strict_key_conflicts? && !exposure.for_merge.respond_to?(:call)
              @output_hash.merge! result, &merge_strategy(exposure.for_merge)
              return
            end

            result.each do |key, value|
              existing_key = conflicting_key(key)

              if existing_key.nil?
                @output_hash[key] = value
                track_source(key, exposure)
              else
                @output_hash[existing_key] = adjudicate(existing_key, exposure, @output_hash[existing_key], value)
              end
            end
          end

          # Returns the key already present in the output that would collide
          # with the given one on the JSON level, or nil when there is no
          # conflict. Only enforced in strict mode.
          def conflicting_key(key)
            return nil unless strict_key_conflicts?

            candidates = [key]
            candidates << key.to_sym if key.respond_to?(:to_sym)
            candidates << key.to_s unless key.is_a?(String)

            candidates.find { |candidate| @output_hash.key?(candidate) }
          end

          def adjudicate(key, exposure, existing_value, incoming_value)
            resolver = conflict_resolver_for(key)
            unless resolver
              raise Grape::Entity::KeyConflictError.new(
                key, @key_sources[key], source_path(exposure, key)
              )
            end

            resolver.call(existing_value, incoming_value, @entity)
          end

          def conflict_resolver_for(key)
            resolvers = @entity.class.key_conflict_resolvers
            resolvers[key] || (resolvers[key.to_sym] if key.respond_to?(:to_sym))
          end

          def strict_key_conflicts?
            if @options && @options.key?(:strict_key_conflicts)
              @options[:strict_key_conflicts]
            else
              @entity.class.strict_key_conflicts?
            end
          end

          def track_source(key, exposure)
            return unless strict_key_conflicts?

            @key_sources[key] = source_path(exposure, key)
          end

          # Builds a human readable path to the exposure that produced a key,
          # e.g. `[2].author.name (from attribute :full_name)`. The attribute
          # path stack already ends with the exposure's own key, so only keys
          # coming out of a merged hash need to be appended explicitly.
          def source_path(exposure, key)
            parts = []
            if @options
              index = @options[:collection_index]
              parts << "[#{index}]" unless index.nil?
              attr_path = @options[:attr_path] || []
              parts.concat(attr_path)
              parts << key if exposure.for_merge && attr_path.last != key
            end
            parts << key if parts.empty?

            path = parts.join('.')
            attribute = exposure.attribute
            path += " (from attribute :#{attribute})" if attribute && attribute.to_s != key.to_s
            path
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
        end
      end
    end
  end
end
