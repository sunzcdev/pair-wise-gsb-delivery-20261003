# frozen_string_literal: true
require 'mustermann/ast/node'

module Mustermann
  # @see Mustermann::AST::Pattern
  module AST
    # Creates a deep copy of a pattern AST with capture names rebound to new
    # names. Only capture names are touched: literal text, separators and
    # constraint payloads are left as they are.
    # @!visibility private
    module Rename
      class << self
        # @param [Mustermann::AST::Node] node the tree to copy
        # @param [Hash{String => String}] mapping old capture names mapped to new names
        # @return [Mustermann::AST::Node] renamed copy of the tree
        # @!visibility private
        def rename(node, mapping)
          case node
          when Array         then node.map { |element| rename(element, mapping) }
          when Node::Splat   then rename_splat(node, mapping)
          when Node::Capture then rename_capture(node, mapping)
          when Node          then rename_node(node, mapping)
          else node
          end
        end

        private

        # @!visibility private
        def rename_capture(node, mapping)
          return node unless new_name = mapping[node.name]
          copy_with_name(node, new_name)
        end

        # @!visibility private
        def rename_splat(node, mapping)
          return node unless new_name = mapping[node.name]
          return copy_with_name(node, new_name) if node.is_a? Node::NamedSplat

          # an unnamed splat turns into a named splat when rebound
          named = Node::NamedSplat.new(new_name)
          node.instance_variables.each do |ivar|
            named.instance_variable_set(ivar, node.instance_variable_get(ivar)) unless ivar == :@payload
          end
          named
        end

        # @!visibility private
        def rename_node(node, mapping)
          copy = node.dup
          copy.payload = rename(node.payload, mapping) if node.payload.is_a?(Node) or node.payload.is_a?(Array)
          copy.head    = rename(node.head, mapping)    if node.is_a?(Node::WithLookAhead)
          copy
        end

        # @!visibility private
        def copy_with_name(node, new_name)
          node.dup.tap { |copy| copy.payload = new_name }
        end
      end
    end
  end
end
