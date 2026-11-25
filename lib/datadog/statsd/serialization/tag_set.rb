# frozen_string_literal: true

module Datadog
  class Statsd
    module Serialization
      # Describes one collision between distinct original tag keys that all
      # serialize to the same wire key.
      TagConflict = Struct.new(:wire_key, :original_keys)

      # Raised when a merge-enabled metric ends up with two distinct original
      # tag keys that escape to the same wire key. Raising keeps both sources
      # visible instead of silently dropping one of them.
      class TagConflictError < Error
        attr_reader :conflicts
        attr_reader :metric_name

        def initialize(conflicts, metric_name = nil)
          @conflicts = conflicts
          @metric_name = metric_name
          super(build_message)
        end

        private

        def build_message
          details = conflicts.map do |conflict|
            keys = conflict.original_keys.map(&:inspect).join(', ')
            "#{keys} all serialize to #{conflict.wire_key.inspect}"
          end
          prefix = metric_name ? "metric #{metric_name.inspect}: " : ''
          "tag conflict on #{prefix}#{details.join('; ')}"
        end
      end

      # Raised when a merge request tries to collapse a key that currently
      # carries several values into a single value without saying which one to
      # keep. Both the values and the original key identity are kept intact.
      class TagMergeError < Error
      end

      # Ordered collection of tags grouped by their original (pre-escaping) key.
      #
      # Unlike a Hash, this keeps the original key identity and every value of a
      # multi-valued key, so tags that only collapse once escaped (for example
      # `s|egment` and `segment`) stay distinguishable. Wire escaping is applied
      # only when the tag list is rendered.
      class TagSet
        Entry = Struct.new(:key, :values)

        # Parses one `key:value` (or key-only) tag string the same way the
        # legacy tag serializer does, including dropping tags with more than
        # one colon.
        def self.parse(tag)
          return nil if tag.nil?

          tokens = tag.to_s.split(':')
          return [tokens[0], nil] if tokens.length == 1
          return [tokens[0], tokens[1]] if tokens.length == 2

          nil
        end

        def self.escape(tag)
          tag = tag.to_s
          return tag unless tag.include?('|') || tag.include?(',')

          tag.delete('|,')
        end

        # Builds the identity-preserving view of the default tags followed by
        # the caller-provided global tags.
        #
        # `default_tags` is always a Hash, so every one of its keys is declared
        # single-valued. Global tags given as a Hash are single-valued too;
        # global tags given as an Array may repeat a key to carry several
        # ordered values.
        def self.build(default_tags, global_tags)
          set = new

          default_tags.each do |key, value|
            set.assign(key, value)
          end

          case global_tags
          when Hash
            global_tags.each do |key, value|
              set.assign(key, value)
            end
          when Array
            declared = {}
            global_tags.each do |tag|
              parsed = parse(tag)
              next unless parsed

              key = parsed[0].to_s
              if declared[key]
                set.add_value(key, parsed[1])
              else
                declared[key] = true
                set.assign(key, parsed[1])
              end
            end
          end

          set
        end

        def initialize(entries = [])
          @entries = entries
        end

        def initialize_copy(other)
          super
          @entries = other.entries.map { |entry| Entry.new(entry.key, entry.values.dup) }
        end

        def empty?
          to_list.empty?
        end

        def keys
          @entries.map(&:key)
        end

        def key?(key)
          !index_of(key).nil?
        end

        # Values of a key, looked up by its original identity. Returns nil when
        # the key is absent.
        def values_for(key)
          index = index_of(key)
          index.nil? ? nil : @entries[index].values.dup
        end

        # Replaces whatever the key currently holds, keeping its position.
        def assign(key, value)
          key = key.to_s
          values = value.is_a?(Array) ? value.dup : [value]
          index = index_of(key)

          if index
            @entries[index] = Entry.new(key, values)
          else
            @entries << Entry.new(key, values)
          end

          self
        end

        # Adds one more value to a key, keeping the values in order. The key is
        # created when absent.
        def add_value(key, value)
          key = key.to_s
          index = index_of(key)

          if index
            @entries[index].values << value
          else
            @entries << Entry.new(key, [value])
          end

          self
        end

        # Replaces the value of a single-valued key. Multi-valued keys must be
        # removed first (or replaced with an explicit list), otherwise the
        # request is rejected instead of silently losing values.
        def set(key, value)
          key = key.to_s
          index = index_of(key)

          if index
            entry = @entries[index]
            if value.is_a?(Array)
              @entries[index] = Entry.new(key, value.dup)
            elsif entry.values.length > 1
              raise TagMergeError,
                "cannot replace multi-valued tag #{key.inspect} with a single value; " \
                'remove it first or pass a list of values'
            else
              @entries[index] = Entry.new(key, [value])
            end
          else
            @entries << Entry.new(key, value.is_a?(Array) ? value.dup : [value])
          end

          self
        end

        # Removes whole keys, matched on their original identity.
        def remove(keys)
          Array(keys).each do |key|
            index = index_of(key)
            @entries.delete_at(index) if index
          end

          self
        end

        def conflicts
          by_wire_key = {}

          @entries.each do |entry|
            next if entry.values.empty?

            wire_key = TagSet.escape(entry.key)
            (by_wire_key[wire_key] ||= []) << entry.key
          end

          by_wire_key.each_with_object([]) do |(wire_key, original_keys), conflicts|
            next if original_keys.length < 2

            conflicts << TagConflict.new(wire_key, original_keys)
          end
        end

        def to_list
          list = []

          @entries.each do |entry|
            entry.values.each do |value|
              list << TagSet.escape(value.nil? ? entry.key : "#{entry.key}:#{value}")
            end
          end

          list
        end

        def to_h
          result = {}
          @entries.each { |entry| result[entry.key] = entry.values.dup }
          result
        end

        protected

        attr_reader :entries

        private

        def index_of(key)
          key = key.to_s
          @entries.index { |entry| entry.key == key }
        end
      end
    end
  end
end
