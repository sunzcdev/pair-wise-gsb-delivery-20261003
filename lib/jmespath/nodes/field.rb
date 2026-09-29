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
        if value.is_a?(Missing)
          value.child(@key)
        elsif value.respond_to?(:to_ary) && @key.is_a?(Integer)
          array = value.to_ary
          if Diagnostic.active? && (@key >= array.size || @key < -array.size)
            Missing.new([@key])
          else
            array[@key]
          end
        elsif value.respond_to?(:to_hash)
          value = value.to_hash
          if Diagnostic.active?
            if value.key?(@key)
              value[@key]
            elsif @key_sym && value.key?(@key_sym)
              value[@key_sym]
            else
              Missing.new([@key])
            end
          elsif !(v = value[@key]).nil?
            v
          elsif @key_sym && !(v = value[@key_sym]).nil?
            v
          end
        elsif value.is_a?(Struct) && value.respond_to?(@key)
          value[@key]
        elsif Diagnostic.active? && value.is_a?(Struct)
          Missing.new([@key])
        end
      end

      def chains_with?(other)
        other.is_a?(Field)
      end

      def chain(other)
        ChainedField.new([@key, *other.keys])
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
        diagnostic = Diagnostic.active?
        path = []
        @keys.reduce(obj) do |value, key|
          path << key
          if value.is_a?(Missing)
            value.child(key)
          elsif value.respond_to?(:to_ary) && key.is_a?(Integer)
            array = value.to_ary
            if diagnostic && (key >= array.size || key < -array.size)
              Missing.new(path.dup)
            else
              array[key]
            end
          elsif value.respond_to?(:to_hash)
            value = value.to_hash
            if diagnostic
              if value.key?(key)
                value[key]
              elsif (sym = @key_syms[key]) && value.key?(sym)
                value[sym]
              else
                Missing.new(path.dup)
              end
            elsif !(v = value[key]).nil?
              v
            elsif (sym = @key_syms[key]) && !(v = value[sym]).nil?
              v
            end
          elsif value.is_a?(Struct) && value.respond_to?(key)
            value[key]
          elsif diagnostic && value.is_a?(Struct)
            Missing.new(path.dup)
          end
        end
      end

      def chain(other)
        ChainedField.new([*@keys, *other.keys])
      end

      private

      attr_reader :keys
    end
  end
end
