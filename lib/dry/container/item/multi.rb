# frozen_string_literal: true

require "dry/container/item/factory"

module Dry
  class Container
    class Item
      # A multi-binding: an ordered collection of named, tagged members
      # registered under a single container key
      #
      # Members can be resolved together as a collection (optionally filtered
      # by tags) or individually by name. Each member keeps its own item, so
      # the `call` and `memoize` options are honoured per member, and a
      # memoized member resolved through the collection is the same object as
      # the one resolved individually.
      #
      # @api public
      class Multi
        # A single member of a multi-binding
        #
        # @api private
        class Member
          # @return [String] the stable member name
          attr_reader :name

          # @return [Array] the member tags
          attr_reader :tags

          # @return [Integer] the member order within the collection
          attr_reader :order

          # @return [Integer] the registration sequence, used as a stable
          #   tie-breaker for members sharing the same order
          attr_reader :sequence

          # @return [Dry::Container::Item] the wrapped item honouring the
          #   member's own call/memoize options
          attr_reader :item

          # @api private
          def initialize(name:, tags:, order:, sequence:, item:)
            @name = name
            @tags = tags
            @order = order
            @sequence = sequence
            @item = item
          end

          # @api private
          def call
            item.call
          end
        end

        # @return [String] the container key the multi-binding is registered under
        attr_reader :key

        # @api private
        def initialize(key, factory: Factory.new)
          @key = key.to_s
          @factory = factory
          @members = []
          @members_by_name = {}
        end

        # Add a member to the multi-binding
        #
        # @param [Mixed] name
        #   The stable member name (used to resolve the member individually)
        # @param [Mixed] item
        #   The item to register (if no block given)
        # @param [Hash] options
        # @option options [Array] :tags
        #   Tags used to resolve a subset of the collection
        # @option options [Integer] :order
        #   Position of the member within the collection (defaults to 0, with
        #   registration order as a tie-breaker)
        # @yield
        #   If a block is given, item will be ignored and the block will be
        #   registered instead
        #
        # @raise [Dry::Container::Error]
        #   If a member with the same name is already registered
        #
        # @return [Dry::Container::Item::Multi] self
        #
        # @api public
        def add(name, item = nil, options = {}, &block)
          if block
            options = item if item.is_a?(::Hash)
            item = block
          end

          name = name.to_s

          if @members_by_name.key?(name)
            raise Error,
                  "There is already a member named #{name.inspect} registered in the " \
                  "#{key.inspect} multi-binding"
          end

          options = options.dup
          tags = Kernel.Array(options.delete(:tags)).freeze
          order = options.delete(:order) || 0

          member = Member.new(
            name: name,
            tags: tags,
            order: order,
            sequence: @members.length,
            item: @factory.call(item, options)
          )

          @members << member
          @members_by_name[name] = member

          self
        end

        # Resolve all members in their stable order
        #
        # @return [Array]
        #
        # @api public
        def call
          resolve
        end

        # Resolve the members matching the given tags, in their stable order
        #
        # Members are ordered by their `:order` option (using registration
        # order as a tie-breaker), never by hash traversal or resolution
        # order. Only the matching members are called, so factories of
        # members filtered out are never executed.
        #
        # @param [Array, nil] tags
        #   When given, only members carrying all of the tags are resolved
        #
        # @return [Array]
        #
        # @api public
        def resolve(tags: nil)
          selected = ordered_members

          unless tags.nil?
            wanted = Kernel.Array(tags)
            selected = selected.select { |member| (wanted - member.tags).empty? }
          end

          selected.map(&:call)
        end

        # Resolve a single member by name
        #
        # @param [Mixed] name
        #   The member name
        #
        # @raise [Dry::Container::Error]
        #   If no member is registered with the given name
        #
        # @return [Mixed]
        #
        # @api public
        def resolve_member(name)
          member(name).call
        end

        # Check whether a member is registered with the given name
        #
        # @param [Mixed] name
        #   The member name
        #
        # @return [Bool]
        #
        # @api public
        def member?(name)
          @members_by_name.key?(name.to_s)
        end

        # @return [Array<String>] the member names in their stable order
        #
        # @api public
        def member_names
          ordered_members.map(&:name)
        end

        private

        # @api private
        def member(name)
          @members_by_name.fetch(name.to_s) do
            raise Error,
                  %(There is no member named "#{name}" registered in the "#{key}" multi-binding)
          end
        end

        # @api private
        def ordered_members
          @members.sort_by { |member| [member.order, member.sequence] }
        end
      end
    end
  end
end
