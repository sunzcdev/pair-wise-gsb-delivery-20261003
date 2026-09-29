# frozen_string_literal: true
module JMESPath
  # @api private
  module Nodes
    class Field < Node
      def initialize(key)
        @key = key
        @key_sym = key.respond_to?(:to_sym) ? key.to_sym : nil
      end

      def visit(value)
        return visit_diagnostic(value) if DiagnosticContext.active?

        if value.respond_to?(:to_ary) && @key.is_a?(Integer)
          value.to_ary[@key]
        elsif value.respond_to?(:to_hash)
          value = value.to_hash
          if !(v = value[@key]).nil?
            v
          elsif @key_sym && !(v = value[@key_sym]).nil?
            v
          end
        elsif value.is_a?(Struct) && value.respond_to?(@key)
          value[@key]
        end
      end

      def chains_with?(other)
        other.is_a?(Field)
      end

      def chain(other)
        ChainedField.new([@key, *other.keys])
      end

      private

      # Diagnostic mode field access. Distinguishes a missing key/index
      # (recorded, returns MISSING) from an explicit `null` (returns nil).
      def visit_diagnostic(value)
        if value.equal?(MISSING)
          value
        elsif value.nil?
          nil
        elsif value.respond_to?(:to_ary) && @key.is_a?(Integer)
          array = value.to_ary
          if @key < array.length && @key >= -array.length
            array[@key]
          else
            record_missing(append_diagnostic_path(+'', @key), 'index_out_of_bounds')
          end
        elsif value.respond_to?(:to_hash)
          hash = value.to_hash
          if hash.key?(@key)
            hash[@key]
          elsif @key_sym && hash.key?(@key_sym)
            hash[@key_sym]
          else
            record_missing(append_diagnostic_path(+'', @key))
          end
        elsif value.is_a?(Struct)
          if value.respond_to?(@key)
            value[@key]
          else
            record_missing(append_diagnostic_path(+'', @key))
          end
        else
          record_missing(append_diagnostic_path(+'', @key), 'not_traversable')
        end
      end

      protected

      def keys
        [@key]
      end
    end

    class ChainedField < Field
      def initialize(keys)
        @keys = keys
        @key_syms = keys.each_with_object({}) do |k, syms|
          syms[k] = k.to_sym if k.respond_to?(:to_sym)
        end
      end

      def visit(obj)
        return visit_diagnostic(obj) if DiagnosticContext.active?

        @keys.reduce(obj) do |value, key|
          if value.respond_to?(:to_ary) && key.is_a?(Integer)
            value.to_ary[key]
          elsif value.respond_to?(:to_hash)
            value = value.to_hash
            if !(v = value[key]).nil?
              v
            elsif (sym = @key_syms[key]) && !(v = value[sym]).nil?
              v
            end
          elsif value.is_a?(Struct) && value.respond_to?(key)
            value[key]
          end
        end
      end

      def chain(other)
        ChainedField.new([*@keys, *other.keys])
      end

      private

      # Diagnostic mode chain access. Records the full path up to the
      # first missing key, then propagates MISSING down the chain.
      def visit_diagnostic(obj)
        value = obj
        path = +''
        @keys.each do |key|
          return value if value.equal?(MISSING) || value.nil?

          value =
            if value.respond_to?(:to_ary) && key.is_a?(Integer)
              array = value.to_ary
              if key < array.length && key >= -array.length
                array[key]
              else
                record_missing(append_diagnostic_path(path, key), 'index_out_of_bounds')
              end
            elsif value.respond_to?(:to_hash)
              hash = value.to_hash
              if hash.key?(key)
                hash[key]
              elsif (sym = @key_syms[key]) && hash.key?(sym)
                hash[sym]
              else
                record_missing(append_diagnostic_path(path, key))
              end
            elsif value.is_a?(Struct)
              if value.respond_to?(key)
                value[key]
              else
                record_missing(append_diagnostic_path(path, key))
              end
            else
              record_missing(append_diagnostic_path(path, key), 'not_traversable')
            end
          path = append_diagnostic_path(path, key)
        end
        value
      end

      attr_reader :keys
    end
  end
end
