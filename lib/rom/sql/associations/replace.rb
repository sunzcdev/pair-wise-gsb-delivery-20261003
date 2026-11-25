# frozen_string_literal: true

module ROM
  module SQL
    module Associations
      # Set-based replacement of many-to-many members
      #
      # A join relation typically owns its own attributes, like a rank that is
      # recorded when a member is added. Replacing a member set must therefore
      # touch only the edges that are actually added or removed, and it must
      # never rewrite the edges that are kept.
      #
      # @api public
      module Replace
        # Replace members of the association with the provided tuples
        #
        # Only join tuples that are added or removed get written. Join tuples
        # for members that stay keep all their values, including extra columns
        # such as a rank, and keep their current order.
        #
        # Extra attributes provided in a member tuple are used only when that
        # member is inserted. For a member that is already associated they are
        # ignored, so a replacement never updates an existing edge.
        #
        # All statements are restricted to the provided source tuples, so join
        # tuples of other sources are invisible to the change, even when they
        # point at the same target tuple.
        #
        # The change is made inside a transaction, or inside a savepoint when a
        # transaction is already open. When inserting a new join tuple fails,
        # the change is rolled back as a whole and the error is re-raised as a
        # ROM::SQL error, so the source keeps its previous members.
        #
        # The arguments are ordered like the arguments of #persist: the source
        # tuples come first and the desired member tuples come second.
        #
        # @example keeping the rank of a member that stays
        #   members = [{id: 2, rank: 10}, {id: 3, rank: 20}]
        #   tasks.associations[:tags].replace([{id: 1}], members)
        #   # => {added: [{task_id: 1, tag_id: 3, rank: 20}],
        #   #     removed: [{task_id: 1, tag_id: 4, rank: 7}]}
        #
        # @param [Array<Hash>, Hash] sources Source tuples identifying the owners of the set
        # @param [Array<Hash>, Hash] members Desired members, each with its primary key
        #
        # @return [Hash, Array<Hash>] Added and removed join tuples, a single hash for a
        #                             single source and a list of hashes otherwise
        #
        # @api public
        def replace(sources, members)
          source_tuples = tuples(sources)
          member_tuples = tuples(members)

          changes =
            if source_tuples.empty?
              []
            else
              in_transaction do
                source_tuples.map { |source_tuple| replace_for(source_tuple, member_tuples) }
              end
            end

          changes.size == 1 ? changes.first : changes
        rescue *ERROR_MAP.keys => e
          raise ERROR_MAP.fetch(e.class, Error), e
        end

        private

        # Change the members of a single source tuple
        #
        # @api private
        def replace_for(source_tuple, members)
          source_id = tuple_pk(source_tuple, source.schema, source_pk_column, 'source tuple')
          current = current_join_tuples(source_id)
          desired = desired_join_tuples(source_id, members)

          removed = select_join_tuples(current, desired)
          added = select_join_tuples(desired, current)

          delete_join_tuples(source_id, removed)
          join_relation.multi_insert(added) unless added.empty?

          {added: added, removed: removed}
        end

        # Select tuples whose target is missing in the other list
        #
        # @api private
        def select_join_tuples(tuples, others)
          other_ids = others.map { |tuple| tuple[target_join_column] }

          tuples.reject { |tuple| other_ids.include?(tuple[target_join_column]) }
        end

        # @api private
        def current_join_tuples(source_id)
          join_relation.where(source_join_column => source_id).to_a
        end

        # Join tuples for the desired members of a source
        #
        # Extra attributes are taken from the member tuple so that they are
        # stored only for the members that are actually inserted.
        #
        # @api private
        def desired_join_tuples(source_id, members)
          members.each_with_object([]) do |member, tuples|
            values = tuple_values(member, target_pk_column)
            member_id = tuple_pk(values, target.schema, target_pk_column, 'member tuple')

            next if tuples.any? { |tuple| tuple[target_join_column].eql?(member_id) }

            tuples << {
              source_join_column => source_id,
              target_join_column => member_id
            }.merge(extra_join_attributes(values))
          end
        end

        # @api private
        def delete_join_tuples(source_id, tuples)
          return if tuples.empty?

          join_relation
            .where(
              source_join_column => source_id,
              target_join_column => tuples.map { |tuple| tuple[target_join_column] }
            )
            .delete
        end

        # Extra join attributes provided in a member tuple
        #
        # @api private
        def extra_join_attributes(values)
          excluded = [source_join_column, target_join_column, target_pk_column]

          values.select { |key, _| join_columns.include?(key) && !excluded.include?(key) }
        end

        # @api private
        def tuple_pk(values, schema, name, label)
          keys = [name, schema[name].key].uniq
          key = keys.find { |candidate| values.key?(candidate) }

          raise KeyError, "#{name.inspect} is missing in #{label}: #{values.inspect}" unless key

          values[key]
        end

        # @api private
        def tuple_values(tuple, name)
          if tuple.respond_to?(:to_h) && !tuple.is_a?(Array)
            tuple.to_h { |key, value| [key.to_sym, value] }
          else
            {name => tuple}
          end
        end

        # @api private
        def tuples(input)
          input.is_a?(Array) ? input : [input]
        end

        # Run the change atomically
        #
        # @api private
        def in_transaction(&)
          Transaction.run(join_relation.dataset.db, &)
        end

        # Join keys of the association as [source, target] pairs
        #
        # @api private
        def join_key_pairs
          unless join_key_map.size == 2 && join_key_map.all? { |pair| pair.size == 2 }
            raise ArgumentError,
                  "replacing members requires a single-column join key, got #{join_key_map.inspect}"
          end

          join_key_map
        end

        # @api private
        def source_pk_column
          join_key_pairs[0][0]
        end

        # @api private
        def source_join_column
          join_key_pairs[0][1]
        end

        # @api private
        def target_join_column
          join_key_pairs[1][0]
        end

        # @api private
        def target_pk_column
          join_key_pairs[1][1]
        end

        # @api private
        def join_columns
          join_relation.schema.map(&:name)
        end
      end
    end
  end
end
