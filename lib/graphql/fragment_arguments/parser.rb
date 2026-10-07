# frozen_string_literal: true

require "graphql/language/parser"
require "graphql/language/nodes"

module GraphQL
  module FragmentArguments
    # A parser which accepts the (non-standard) fragment-arguments syntax:
    #
    #   fragment MyFrag($first: Int = 10) on MyType { ... }
    #   ...MyFrag(first: 20)
    #
    # It produces the same AST nodes as the standard parser plus
    # {GraphQL::Language::Nodes::FragmentArgumentDefinition} and
    # {GraphQL::Language::Nodes::FragmentArgument}. When this extension is
    # disabled the regular {GraphQL::Language::Parser} is used and the new
    # syntax keeps raising its normal parse errors.
    #
    # @see GraphQL::FragmentArguments
    class Parser < GraphQL::Language::Parser
      private

      def definition
        if at?(:FRAGMENT)
          loc = pos
          expect_token :FRAGMENT
          f_name = if !at?(:ON)
            parse_name
          end

          # Optional fragment parameter declaration, e.g. `($first: Int = 10)`.
          fragment_arguments = if at?(:LPAREN)
            parse_fragment_argument_definitions
          else
            EmptyObjects::EMPTY_ARRAY
          end

          expect_token :ON
          f_type = parse_type_name
          directives = parse_directives
          selections = selection_set
          Language::Nodes::FragmentDefinition.new(
            pos: loc,
            name: f_name,
            type: f_type,
            directives: directives,
            selections: selections,
            fragment_arguments: fragment_arguments,
            filename: @filename,
            source: self
          )
        else
          super
        end
      end

      def parse_fragment_argument_definitions
        expect_token(:LPAREN)
        defs = []
        while !at?(:RPAREN)
          loc = pos
          expect_token(:VAR_SIGN)
          arg_name = parse_name
          expect_token(:COLON)
          arg_type = self.type || raise_parse_error("Missing type definition for fragment argument: $#{arg_name}")
          default_value = if at?(:EQUALS)
            advance_token
            value
          end
          directives = parse_directives
          defs << Language::Nodes::FragmentArgumentDefinition.new(
            pos: loc,
            name: arg_name,
            type: arg_type,
            default_value: default_value,
            directives: directives,
            filename: @filename,
            source: self
          )
        end
        expect_token(:RPAREN)
        defs
      end

      def selection_set
        # The stock implementation builds a FragmentSpread directly; reimplement
        # just the spread branch so that `...Frag(a: 1)` keeps its arguments.
        expect_token(:LCURLY)
        selections = []
        while @token_name != :RCURLY
          selections << if at?(:ELLIPSIS)
            loc = pos
            advance_token
            case token_name
            when :ON, :DIR_SIGN, :LCURLY
              if_type = if at?(:ON)
                advance_token
                parse_type_name
              else
                nil
              end

              directives = parse_directives

              Language::Nodes::InlineFragment.new(pos: loc, type: if_type, directives: directives, selections: selection_set, filename: @filename, source: self)
            else
              name = parse_name_without_on
              fragment_arguments = if at?(:LPAREN)
                parse_fragment_arguments
              else
                EmptyObjects::EMPTY_ARRAY
              end
              directives = parse_directives

              Language::Nodes::FragmentSpread.new(pos: loc, name: name, directives: directives, fragment_arguments: fragment_arguments, filename: @filename, source: self)
            end
          else
            loc = pos
            name = parse_name

            field_alias = nil

            if at?(:COLON)
              advance_token
              field_alias = name
              name = parse_name
            end

            arguments = at?(:LPAREN) ? parse_arguments : nil
            directives = at?(:DIR_SIGN) ? parse_directives : nil
            selection_set = at?(:LCURLY) ? self.selection_set : nil

            Language::Nodes::Field.new(pos: loc, field_alias: field_alias, name: name, arguments: arguments, directives: directives, selections: selection_set, filename: @filename, source: self)
          end
        end
        expect_token(:RCURLY)
        selections
      end

      def parse_fragment_arguments
        expect_token(:LPAREN)
        args = []
        while !at?(:RPAREN)
          loc = pos
          name = parse_name
          expect_token(:COLON)
          args << Language::Nodes::FragmentArgument.new(pos: loc, name: name, value: value, filename: @filename, source: self)
        end
        if args.empty?
          expect_token(:ARGUMENT_NAME) # At least one argument is required
        end
        expect_token(:RPAREN)
        args
      end
    end
  end
end
