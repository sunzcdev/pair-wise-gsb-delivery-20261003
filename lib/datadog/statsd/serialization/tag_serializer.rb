# frozen_string_literal: true

module Datadog
  class Statsd
    module Serialization
      class TagSerializer
        def initialize(global_tags = [], env = ENV)
          defaults = default_tags(env)

          # Keep the original key identity straight from the caller-provided
          # tags (arrays may carry several ordered values for one key) for
          # merge-enabled metrics.
          @global_tag_set = TagSet.build(defaults, global_tags)

          # Convert to hash
          global_tags = to_tags_hash(global_tags)

          # Merge with default tags
          global_tags = defaults.merge(global_tags)

          # Convert to tag list and set
          @global_tags = to_tags_list(global_tags)
          if @global_tags.any?
            @global_tags_formatted = @global_tags.join(',')
          else
            @global_tags_formatted = nil
          end
        end

        def format(message_tags)
          if !message_tags || message_tags.empty?
            return @global_tags_formatted
          end

          tags = if @global_tags_formatted
                   [@global_tags_formatted, to_tags_list(message_tags)]
                 else
                   to_tags_list(message_tags)
                 end

          tags.join(',')
        end

        # Formats a metric whose tags follow the identity-aware merge rules.
        #
        # The global tag identity set is copied, never mutated, so merge
        # requests cannot rewrite the tags used by ordinary calls. `remove`
        # drops keys by their original identity, `set` replaces single-valued
        # keys, and message tags are appended as extra values. Distinct
        # remaining original keys that escape to the same wire key raise a
        # `TagConflictError` instead of silently overwriting one another.
        def format_merged(message_tags = nil, remove: nil, set: nil, metric_name: nil)
          merged = global_tag_set

          merged.remove(remove) if remove
          apply_merged_values(merged, set) if set
          append_tags(merged, message_tags) if message_tags && !message_tags.empty?

          conflicts = merged.conflicts
          raise TagConflictError.new(conflicts, metric_name) unless conflicts.empty?

          list = merged.to_list
          list.empty? ? nil : list.join(',')
        end

        attr_reader :global_tags

        # Independent copy of the identity-preserving global tags.
        def global_tag_set
          @global_tag_set.dup
        end

        private

        def apply_merged_values(tag_set, values)
          case values
          when Hash
            values.each { |key, value| tag_set.set(key, value) }
          when Array
            values.each do |tag|
              parsed = TagSet.parse(tag)
              tag_set.set(parsed[0], parsed[1]) if parsed
            end
          else
            raise ArgumentError, 'merged tag values must be a Hash or an Array'
          end
        end

        def append_tags(tag_set, message_tags)
          case message_tags
          when Hash
            message_tags.each { |key, value| tag_set.add_value(key, value) }
          when Array
            message_tags.each do |tag|
              parsed = TagSet.parse(tag)
              tag_set.add_value(parsed[0], parsed[1]) if parsed
            end
          else
            raise ArgumentError, 'tags must be an array of string tags or a Hash'
          end
        end

        def to_tags_hash(tags)
          case tags
          when Hash
            tags.dup
          when Array
            Hash[
              tags.map do |string|
                tokens = string.split(':')
                tokens << nil if tokens.length == 1
                tokens.length == 2 ? tokens : nil
              end.compact
            ]
          else
            {}
          end
        end

        def to_tags_list(tags)
          case tags
          when Hash
            tags.map do |name, value|
              if value.nil?
                escape_tag_content(name)
              else
                escape_tag_content("#{name}:#{value}")
              end
            end
          when Array
            tags.map { |tag| escape_tag_content(tag) }
          else
            []
          end
        end

        def escape_tag_content(tag)
          tag = tag.to_s
          return tag unless tag.include?('|') || tag.include?(',')
          tag.delete('|,')
        end

        def dd_tags(env = ENV)
          return {} unless dd_tags = env['DD_TAGS']

          to_tags_hash(dd_tags.split(','))
        end

        def default_tags(env = ENV)
          dd_tags(env).tap do |tags|
            tags['dd.internal.entity_id'] = env['DD_ENTITY_ID'] if env.key?('DD_ENTITY_ID')
            tags['env'] = env['DD_ENV'] if env.key?('DD_ENV')
            tags['service'] = env['DD_SERVICE'] if env.key?('DD_SERVICE')
            tags['version'] = env['DD_VERSION'] if env.key?('DD_VERSION')
          end
        end
      end
    end
  end
end
