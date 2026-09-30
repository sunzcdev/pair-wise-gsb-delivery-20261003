# frozen_string_literal: true

module Dry
  class Container
    # An ordered set of {Member}s bound under the same key. Members keep a
    # stable name, are sorted deterministically (explicit order first, then
    # registration order) and are shared with the individual member bindings
    # so collections and single-member resolution use the same cached object.
    #
    # @api public
    class Collection
      # @return [String] tag the collection is resolved by
      attr_reader :tag

      # @api private
      def initialize(tag, members = [])
        @tag = tag.to_s.dup.freeze
        @members = []
        @by_name = {}
        @mutex = ::Mutex.new
        members.each { |member| add(member) }
      end

      # Add a member to the collection
      #
      # @param [Dry::Container::Member] member
      #
      # @raise [Dry::Container::Error]
      #   If a member with the same name is already registered
      #
      # @return [Dry::Container::Member]
      #
      # @api private
      def add(member)
        @mutex.synchronize do
          existing = @by_name[member.name]

          if existing
            raise Error,
                  "There is already a member named #{member.name.inspect} " \
                  "in collection #{tag.inspect} " \
                  "(registered from #{existing.source.inspect} and " \
                  "#{member.source.inspect})"
          end

          member.sequence = @members.length
          @members << member
          @by_name[member.name] = member
          member
        end
      end

      # Merge members of another collection into this one
      #
      # @param [Dry::Container::Collection] other
      #
      # @return [Dry::Container::Collection] self
      #
      # @api private
      def merge!(other)
        other.members.each { |member| add(member) }
        self
      end

      # @param [Mixed] name
      #
      # @return [Dry::Container::Member, nil]
      #
      # @api private
      def member(name)
        @by_name[name.to_s]
      end

      # @return [Array<Dry::Container::Member>] members in deterministic order
      #
      # @api private
      def members
        @members
          .sort_by.with_index { |member, index|
            [member.order.nil? ? 1 : 0, member.order || index, member.sequence]
          }
      end

      # Resolve every member (optionally filtered) in deterministic order
      #
      # @param [Array<String>] tags
      #   Only keep members carrying one of these tags (empty means all)
      # @yield [Dry::Container::Member] optional filter evaluated before calling
      #
      # @return [Array<Mixed>]
      #
      # @api public
      def call(tags = EMPTY_TAGS, &filter)
        selected = members

        unless tags.empty?
          selected = selected.select { |member| tags.any? { |tag| member.tagged?(tag) } }
        end

        selected = selected.select(&filter) if filter

        selected.map(&:call)
      end

      # @return [Dry::Container::Collection] an independent copy
      #
      # @api private
      def dup
        copy = super
        copy.instance_variable_set(:@mutex, ::Mutex.new)
        members = @members.map(&:dup)
        copy.instance_variable_set(:@members, members)
        copy.instance_variable_set(:@by_name, members.to_h { |member| [member.name, member] })
        copy
      end

      # Build a copy with every key rewritten under a namespace prefix
      #
      # @return [Dry::Container::Collection]
      #
      # @api private
      def prefix(namespace, separator)
        prefixed_tag = [namespace, tag].join(separator)
        collection = self.class.new(prefixed_tag)

        members.each do |member|
          copy = member.dup
          copy.instance_variable_set(
            :@tags, [prefixed_tag, member.name, *(member.tags[2..] || [])].freeze
          )
          collection.add(copy)
        end

        collection
      end

      private

      EMPTY_TAGS = [].freeze
      private_constant :EMPTY_TAGS
    end
  end
end
