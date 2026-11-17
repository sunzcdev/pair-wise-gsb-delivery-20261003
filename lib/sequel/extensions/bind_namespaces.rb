# frozen-string-literal: true
#
# Dataset methods backing the bind_namespaces extension.  See
# Sequel::BindNamespaces for the overview and examples.

require_relative "bind_namespaces/rewriter"

module Sequel
  module BindNamespaces
    # Methods added to every Sequel::Dataset when the extension is loaded.
    module DatasetMethods
      # Return a dataset in which every unqualified bind placeholder (such as
      # :$id) is rewritten into the isolated +namespace+ (for example
      # :orders__id).  Reusable datasets that happen to use the same placeholder
      # names can therefore be combined without their values colliding.
      #
      # +values+ is an optional hash of original placeholder names to the
      # values to bind for them (e.g. <tt>id: 1</tt>).  Values are recorded in
      # the dataset's namespace registry and become part of the final binding
      # map produced by #compile_bindings.
      #
      #   ds = DB[:orders].where(id: :$id).bind_namespace(:orders, id: 1)
      #
      # Calling this again on an already namespaced dataset is idempotent: the
      # existing qualified names are not prefixed a second time.
      def bind_namespace(namespace, values=Sequel::OPTS)
        namespace_dataset(namespace, values, false)
      end

      # Return a dataset whose placeholders belong to a namespace that may be
      # shared with other datasets on purpose.  All datasets sharing the same
      # namespace name resolve a given placeholder to one final bind name, so
      # they use one value.
      #
      # If the same shared placeholder is registered with values of
      # incompatible types (for example an Integer in one dataset and a String
      # in another), an +Sequel::Error+ is raised immediately rather than
      # letting the last merged hash value overwrite the earlier one.
      def shared_bind_namespace(namespace, values=Sequel::OPTS)
        namespace_dataset(namespace, values, true)
      end

      # Return a dataset whose namespace registry also contains the registries
      # of the given datasets.  Use this when combining datasets by hand (for
      # instance through custom SQL) rather than through CTE/subquery clauses,
      # so #compile_bindings can see every placeholder in the final query.
      # Shared namespaces keep their type-compatibility guarantees.
      def merge_bind_namespaces(*datasets)
        registry = merged_registry(datasets)
        clone(:bind_namespace_registry=>registry)
      end

      # Compile the dataset's parameter namespaces into a
      # Sequel::BindNamespaces::CompiledQuery containing:
      #
      # * the final SQL with positional +?+ placeholders,
      # * the final mapping of qualified placeholder names to values,
      # * the ordered bind arguments matching the placeholders.
      #
      # Placeholders reachable through nested CTEs, subqueries and literal SQL
      # arguments are all included; text inside string constants is never
      # rewritten.  An error is raised if the query still contains an
      # unqualified placeholder that no namespace registered a value for.
      #
      # +type+ defaults to +:select+.  Pass +:insert+, +:update+ or +:delete+
      # (or another type accepted by Dataset#prepare) to compile another
      # statement shape; +values+ are forwarded to insert/update statements.
      def compile_bindings(type=:select, *values)
        registry = self.registry_with_values
        collect_nested_registries(self, registry)
        apply_bind_vars_to_namespaces(registry)

        # Drive Sequel's emulated prepared-statement machinery.  It walks the
        # whole query (including nested CTEs, subqueries and literal SQL
        # arguments), recording each placeholder use in encounter order into
        # prepared_args while emitting positional "?" placeholders.
        prepared = send(:to_prepared_statement, type, values,
                        :extend=>[Sequel::Dataset::EmulatePreparedStatementMethods]).
          send(:emulated_prepared_statement, type, nil, values)

        ordered_names = prepared.prepared_args.dup
        verify_placeholders!(ordered_names, registry)

        bindings = {}
        arguments = ordered_names.map do |name|
          key = :"$#{name}"
          unless registry.key?(key)
            raise Sequel::Error, "no value registered for bind placeholder #{key.inspect}"
          end
          value = registry.fetch(key)
          if value.nil?
            raise Sequel::Error,
              "bind placeholder #{key.inspect} has no value registered; supply one via #bind_namespace/#shared_bind_namespace or #bind"
          end
          bindings[key] = value
        end

        CompiledQuery.new(final_compiled_sql(prepared, ordered_names.length),
                          bindings, arguments, prepared)
      end

      # Reassemble the SQL recorded by the emulated prepared statement, joining
      # its fragments with the adapter's native bind placeholder (the unnamed
      # "?" argument marker by default).  The fragment count is always one
      # greater than the placeholder count.
      def final_compiled_sql(prepared, placeholder_count)
        frags = prepared.opts[:prepared_sql_frags]
        return prepared.prepared_sql unless frags.is_a?(::Array)
        unless frags.length == placeholder_count + 1
          raise Sequel::Error,
            "bind placeholder count mismatch: #{placeholder_count} names for #{frags.length - 1} SQL positions"
        end
        marker = prepared.send(:prepared_arg_placeholder).to_s
        sql = String.new
        frags.each_with_index do |frag, i|
          sql << frag
          sql << marker unless i == placeholder_count
        end
        sql
      end

      # The hash of qualified placeholder names to values carried by the
      # dataset's namespace, or an empty hash when the dataset is not
      # namespaced.
      def bind_namespace_registry
        @opts[:bind_namespace_registry] || Sequel::OPTS
      end

      private

      # Shared implementation for isolated and shared namespaces.
      def namespace_dataset(namespace, values, shared)
        registry = Hash[@opts[:bind_namespace_registry] || []]
        values.each do |name, value|
          qualified = :"$#{namespace}#{BindNamespaces::NAMESPACE_SEPARATOR}#{name}"
          if shared && registry.key?(qualified)
            existing = registry.fetch(qualified)
            if !existing.nil? && !BindNamespaces.compatible_types?(existing, value)
              raise Sequel::Error,
                "incompatible bind types for shared placeholder #{qualified.inspect}: " \
                "#{existing.class} (#{existing.inspect}) and #{value.class} (#{value.inspect})"
            end
            registry[qualified] = value if existing.nil?
          else
            registry[qualified] = value unless registry.key?(qualified)
          end
        end

        rewritten = BindNamespaces.rewrite(self, namespace, registry, shared)
        # rewrite() returns the receiver untouched when it has no traversed
        # options, but the registry still has to be attached.
        rewritten = clone if rewritten.equal?(self)
        rewritten.clone(:bind_namespace_registry=>registry.freeze)
      end

      # Merge the registries of the receiver and the given datasets, enforcing
      # shared type compatibility for duplicated names.
      def merged_registry(datasets)
        registry = {}
        ([self] + datasets.flatten).each do |ds|
          next unless ds.respond_to?(:bind_namespace_registry)
          ds.bind_namespace_registry.each do |name, value|
            if registry.key?(name)
              existing = registry.fetch(name)
              if !value.nil? && !existing.nil? && !BindNamespaces.compatible_types?(existing, value)
                raise Sequel::Error,
                  "incompatible bind types for shared placeholder #{name.inspect}: " \
                  "#{existing.class} (#{existing.inspect}) and #{value.class} (#{value.inspect})"
              end
              registry[name] = value if existing.nil? && !value.nil?
            else
              registry[name] = value
            end
          end
        end
        registry
      end

      # Return the registry, including values registered through Dataset#bind
      # (stored in :bind_vars) keyed by their placeholder names.
      def registry_with_values
        registry = Hash[@opts[:bind_namespace_registry] || []]
        if bv = @opts[:bind_vars]
          bv.each do |name, value|
            key = name.to_s.start_with?("$") ? name.to_sym : :"$#{name}"
            registry[key] = value unless registry.key?(key)
          end
        end
        registry
      end

      # Fill namespaced placeholders that still have no value (nil) from the
      # dataset's :bind_vars, matching by the placeholder's original trailing
      # name.  A bind var such as <tt>bind(a: 77)</tt> therefore supplies the
      # value for every :$<namespace>__a placeholder without naming the
      # namespace explicitly.
      def apply_bind_vars_to_namespaces(registry)
        return unless bv = @opts[:bind_vars]
        normalized = {}
        bv.each do |name, value|
          normalized[name.to_s.sub(/\A\$/, "").to_sym] = value
        end
        registry.each do |qualified, value|
          next unless value.nil?
          trailing = qualified.to_s.sub(/\A\$/, "").split(BindNamespaces::NAMESPACE_SEPARATOR).last.to_sym
          registry[qualified] = normalized[trailing] if normalized.key?(trailing)
        end
      end

      # Walk the dataset's query tree (CTEs, FROM/JOIN sources, subqueries,
      # compounds and expressions) and merge every nested dataset's namespace
      # registry into +registry+, enforcing shared type compatibility.
      def collect_nested_registries(dataset, registry)
        BindNamespaces::TRAVERSED_DATASET_OPTS.each do |key|
          next unless dataset.opts.key?(key)
          gather_registries(dataset.opts[key], registry)
        end
        registry
      end

      def gather_registries(obj, registry)
        case obj
        when Sequel::Dataset
          if stored = obj.opts[:bind_namespace_registry]
            stored.each { |name, value| merge_registry_entry(registry, name, value) }
          end
          collect_nested_registries(obj, registry)
        when Sequel::SQL::Expression
          obj.class.comparison_attrs.each do |attr|
            gather_registries(obj.public_send(attr), registry)
          end
        when ::Array
          obj.each { |v| gather_registries(v, registry) }
        when ::Hash
          # Only values can contain nested datasets/expressions.
          obj.each_value { |v| gather_registries(v, registry) }
        end
      end

      def merge_registry_entry(registry, name, value)
        if registry.key?(name)
          existing = registry.fetch(name)
          if !value.nil? && !existing.nil? && !BindNamespaces.compatible_types?(existing, value)
            raise Sequel::Error,
              "incompatible bind types for shared placeholder #{name.inspect}: " \
              "#{existing.class} (#{existing.inspect}) and #{value.class} (#{value.inspect})"
          end
          registry[name] = value if existing.nil? && !value.nil?
        else
          registry[name] = value
        end
      end

      # Every captured placeholder must be a qualified name produced by a
      # namespace and have a registered value.  A bare name means the
      # placeholder was never assigned to a namespace.
      def verify_placeholders!(names, registry)
        names.each do |name|
          if !name.to_s.include?(BindNamespaces::NAMESPACE_SEPARATOR)
            raise Sequel::Error,
              "unqualified placeholder #{:"$#{name}".inspect} is not assigned to a bind namespace"
          end
        end
      end
    end
  end

  Dataset.register_extension(:bind_namespaces, BindNamespaces::DatasetMethods)
end


