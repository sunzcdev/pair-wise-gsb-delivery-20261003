# frozen_string_literal: true

require "strscan"
require "graphql/language/nodes"
require "graphql/tracing/null_trace"

module GraphQL
  module Language
    class Parser
      include GraphQL::Language::Nodes
      include EmptyObjects

      class << self
        attr_accessor :cache

        def parse(graphql_str, filename: nil, trace: Tracing::NullTrace, max_tokens: nil)
          self.new(graphql_str, filename: filename, trace: trace, max_tokens: max_tokens).parse
        end

        def parse_file(filename, trace: Tracing::NullTrace)
          if cache
            cache.fetch(filename) do
              parse(File.read(filename), filename: filename, trace: trace)
            end
          else
            parse(File.read(filename), filename: filename, trace: trace)
          end
        end
      end

      def initialize(graphql_str, filename: nil, trace: Tracing::NullTrace, max_tokens: nil)
        if graphql_str.nil?
          raise GraphQL::ParseError.new("No query string was present", nil, nil, nil)
        end
        @lexer = Lexer.new(graphql_str, filename: filename, max_tokens: max_tokens)
        @graphql_str = graphql_str
        @filename = filename
        @trace = trace
        @dedup_identifiers = false
        @lines_at = nil
        @tolerant = false
      end

      def parse
        @document ||= begin
          @trace.parse(query_string: @graphql_str) do
            document
          end
        rescue SystemStackError
          raise GraphQL::ParseError.new("This query is too large to execute.", nil, nil, @query_str, filename: @filename)
        end
      end

      def tokens_count
        parse
        @lexer.tokens_count
      end

      def line_at(pos)
        line = lines_at.bsearch_index { |l| l >= pos }
        if line.nil?
          @lines_at.size + 1
        else
          line + 1
        end
      end

      def column_at(pos)
        next_line_idx = lines_at.bsearch_index { |l| l >= pos } || 0
        if next_line_idx > 0
          line_pos = @lines_at[next_line_idx - 1]
          pos - line_pos
        else
          pos + 1
        end
      end

      private

      # Raised internally by the diagnostic parser when too many errors were
      # reported, so that recovery can't spiral into dozens of cascading
      # diagnostics for one broken document.
      class DiagnosticAbort < StandardError
      end

      # @return [Array<Integer>] Positions of each line break in the original string
      def lines_at
        @lines_at ||= begin
          la = []
          idx = 0
          while idx
            idx = @graphql_str.index("\n", idx)
            if idx
              la << idx
              idx += 1
            end
          end
          la
        end
      end

      attr_reader :token_name

      def advance_token
        @token_name = @lexer.advance
      end

      def pos
        @lexer.pos
      end

      def document
        any_tokens = advance_token
        defns = []
        if any_tokens
          add_next_definition(defns)
        else
          # Only ignored characters is not a valid document
          raise GraphQL::ParseError.new("Unexpected end of document", nil, nil, @graphql_str)
        end
        if @tolerant
          until @lexer.finished?
            break unless add_next_definition(defns)
          end
        else
          while !@lexer.finished?
            add_next_definition(defns)
          end
        end
        build_document(defns)
      rescue DiagnosticAbort
        raise unless @tolerant
        build_document(defns)
      end

      # @return [Boolean] false when parsing must stop (EOF in diagnostic mode)
      def add_next_definition(definitions)
        defn = nil
        status = parse_recoverable(DEFINITION_STOPS, definition_start_only: true) do
          defn = parse_document_definition
        end
      rescue DiagnosticAbort
        return false
      else
        definitions << defn if defn
        if defn.nil? && @tolerant
          # The definition failed to parse and its error was already
          # recorded; skip junk tokens up to the next definition boundary.
          return false if @lexer.finished?
          skip_to_next_definition
          return false if @lexer.finished?
        end
        status != :eof
      end

      # Hook for the diagnostic parser to mark partial definitions
      # and decide whether a recovered definition should be kept.
      def parse_document_definition
        definition
      end

      def build_document(definitions)
        Document.new(pos: 0, definitions: definitions, filename: @filename, source: self)
      end

      def definition
        case token_name
        when :FRAGMENT
          loc = pos
          expect_token :FRAGMENT
          f_name = if !at?(:ON)
            parse_name
          end
          expect_token :ON
          f_type = parse_type_name
          directives = parse_directives
          selections = selection_set
          Nodes::FragmentDefinition.new(
            pos: loc,
            name: f_name,
            type: f_type,
            directives: directives,
            selections: selections,
            filename: @filename,
            source: self
          )
        when :QUERY, :MUTATION, :SUBSCRIPTION, :LCURLY
          op_loc = pos
          op_type = case token_name
          when :LCURLY
            "query"
          else
            parse_operation_type
          end

          op_name = case token_name
          when :LPAREN, :LCURLY, :DIR_SIGN
            nil
          else
            parse_name
          end

          variable_definitions = if at?(:LPAREN)
            expect_token(:LPAREN)
            defs = []
            while !at?(:RPAREN)
              status = parse_recoverable([:RPAREN]) do
                loc = pos
                expect_token(:VAR_SIGN)
                var_name = parse_name
                expect_token(:COLON)
                var_type = self.type || raise_parse_error("Missing type definition for variable: $#{var_name}")
                default_value = if at?(:EQUALS)
                  advance_token
                  value
                end

                directives = parse_directives

                defs << Nodes::VariableDefinition.new(
                  pos: loc,
                  name: var_name,
                  type: var_type,
                  default_value: default_value,
                  directives: directives,
                  filename: @filename,
                  source: self
                )
              end
              break if status != :continue
            end
            expect_token(:RPAREN)
            defs
          else
            EmptyObjects::EMPTY_ARRAY
          end

          directives = parse_directives

          OperationDefinition.new(
            pos: op_loc,
            operation_type: op_type,
            name: op_name,
            variables: variable_definitions,
            directives: directives,
            selections: selection_set,
            filename: @filename,
            source: self
          )
        when :EXTEND
          loc = pos
          advance_token
          case token_name
          when :SCALAR
            advance_token
            name = parse_name
            directives = parse_directives
            ScalarTypeExtension.new(pos: loc, name: name, directives: directives, filename: @filename, source: self)
          when :TYPE
            advance_token
            name = parse_name
            implements_interfaces = parse_implements
            directives = parse_directives
            field_defns = at?(:LCURLY) ? parse_field_definitions : EMPTY_ARRAY

            ObjectTypeExtension.new(pos: loc, name: name, interfaces: implements_interfaces, directives: directives, fields: field_defns, filename: @filename, source: self)
          when :INTERFACE
            advance_token
            name = parse_name
            directives = parse_directives
            interfaces = parse_implements
            fields_definition = at?(:LCURLY) ? parse_field_definitions : EMPTY_ARRAY
            InterfaceTypeExtension.new(pos: loc, name: name, directives: directives, fields: fields_definition, interfaces: interfaces, filename: @filename, source: self)
          when :UNION
            advance_token
            name = parse_name
            directives = parse_directives
            union_member_types = parse_union_members
            UnionTypeExtension.new(pos: loc, name: name, directives: directives, types: union_member_types, filename: @filename, source: self)
          when :ENUM
            advance_token
            name = parse_name
            directives = parse_directives
            enum_values_definition = parse_enum_value_definitions
            Nodes::EnumTypeExtension.new(pos: loc, name: name, directives: directives, values: enum_values_definition, filename: @filename, source: self)
          when :INPUT
            advance_token
            name = parse_name
            directives = parse_directives
            input_fields_definition = parse_input_object_field_definitions
            InputObjectTypeExtension.new(pos: loc, name: name, directives: directives, fields: input_fields_definition, filename: @filename, source: self)
          when :SCHEMA
            advance_token
            directives = parse_directives
            query = mutation = subscription = nil
            if at?(:LCURLY)
              advance_token
              while !at?(:RCURLY)
                status = parse_recoverable([:RCURLY]) do
                  if at?(:QUERY)
                    advance_token
                    expect_token(:COLON)
                    query = parse_name
                  elsif at?(:MUTATION)
                    advance_token
                    expect_token(:COLON)
                    mutation = parse_name
                  elsif at?(:SUBSCRIPTION)
                    advance_token
                    expect_token(:COLON)
                    subscription = parse_name
                  else
                    expect_one_of([:QUERY, :MUTATION, :SUBSCRIPTION])
                  end
                end
                break if status != :continue
              end
              expect_token :RCURLY
            end
            SchemaExtension.new(
              subscription: subscription,
              mutation: mutation,
              query: query,
              directives: directives,
              pos: loc,
              filename: @filename,
              source: self,
            )
          else
            expect_one_of([:SCHEMA, :SCALAR, :TYPE, :ENUM, :INPUT, :UNION, :INTERFACE])
          end
        else
          loc = pos
          desc = at?(:STRING) ? string_value : nil
          defn_loc = pos
          case token_name
          when :SCHEMA
            advance_token
            directives = parse_directives
            query = mutation = subscription = nil
            expect_token :LCURLY
            while !at?(:RCURLY)
              status = parse_recoverable([:RCURLY]) do
                if at?(:QUERY)
                  advance_token
                  expect_token(:COLON)
                  query = parse_name
                elsif at?(:MUTATION)
                  advance_token
                  expect_token(:COLON)
                  mutation = parse_name
                elsif at?(:SUBSCRIPTION)
                  advance_token
                  expect_token(:COLON)
                  subscription = parse_name
                else
                  expect_one_of([:QUERY, :MUTATION, :SUBSCRIPTION])
                end
              end
              break if status != :continue
            end
            expect_token :RCURLY
            SchemaDefinition.new(pos: loc, definition_pos: defn_loc, query: query, mutation: mutation, subscription: subscription, directives: directives, filename: @filename, source: self)
          when :DIRECTIVE
            advance_token
            expect_token :DIR_SIGN
            name = parse_name
            arguments_definition = parse_argument_definitions
            repeatable = if at?(:REPEATABLE)
              advance_token
              true
            else
              false
            end
            expect_token :ON
            directive_locations = [DirectiveLocation.new(pos: pos, name: parse_name, filename: @filename, source: self)]
            while at?(:PIPE)
              advance_token
              directive_locations << DirectiveLocation.new(pos: pos, name: parse_name, filename: @filename, source: self)
            end
            DirectiveDefinition.new(pos: loc, definition_pos: defn_loc, description: desc, name: name, arguments: arguments_definition, locations: directive_locations, repeatable: repeatable, filename: @filename, source: self)
          when :TYPE
            advance_token
            name = parse_name
            implements_interfaces = parse_implements
            directives = parse_directives
            field_defns = at?(:LCURLY) ? parse_field_definitions : EMPTY_ARRAY

            ObjectTypeDefinition.new(pos: loc, definition_pos: defn_loc, description: desc, name: name, interfaces: implements_interfaces, directives: directives, fields: field_defns, filename: @filename, source: self)
          when :INTERFACE
            advance_token
            name = parse_name
            interfaces = parse_implements
            directives = parse_directives
            fields_definition = parse_field_definitions
            InterfaceTypeDefinition.new(pos: loc, definition_pos: defn_loc, description: desc, name: name, directives: directives, fields: fields_definition, interfaces: interfaces, filename: @filename, source: self)
          when :UNION
            advance_token
            name = parse_name
            directives = parse_directives
            union_member_types = parse_union_members
            UnionTypeDefinition.new(pos: loc, definition_pos: defn_loc, description: desc, name: name, directives: directives, types: union_member_types, filename: @filename, source: self)
          when :SCALAR
            advance_token
            name = parse_name
            directives = parse_directives
            ScalarTypeDefinition.new(pos: loc, definition_pos: defn_loc, description: desc, name: name, directives: directives, filename: @filename, source: self)
          when :ENUM
            advance_token
            name = parse_name
            directives = parse_directives
            enum_values_definition = parse_enum_value_definitions
            Nodes::EnumTypeDefinition.new(pos: loc, definition_pos: defn_loc, description: desc, name: name, directives: directives, values: enum_values_definition, filename: @filename, source: self)
          when :INPUT
            advance_token
            name = parse_name
            directives = parse_directives
            input_fields_definition = parse_input_object_field_definitions
            InputObjectTypeDefinition.new(pos: loc, definition_pos: defn_loc, description: desc, name: name, directives: directives, fields: input_fields_definition, filename: @filename, source: self)
          else
            expect_one_of([:SCHEMA, :SCALAR, :TYPE, :ENUM, :INPUT, :UNION, :INTERFACE])
          end
        end
      end

      def parse_input_object_field_definitions
        if at?(:LCURLY)
          expect_token :LCURLY
          list = []
          while !at?(:RCURLY)
            item = nil
            status = parse_recoverable([:RCURLY]) { item = parse_input_value_definition }
            list << item if item
            break if status != :continue
          end
          expect_token :RCURLY
          list
        else
          EMPTY_ARRAY
        end
      end

      def parse_enum_value_definitions
        if at?(:LCURLY)
          expect_token :LCURLY
          list = []
          while !at?(:RCURLY)
            enum_defn = nil
            status = parse_recoverable([:RCURLY]) do
              v_loc = pos
              description = if at?(:STRING); string_value; end
              defn_loc = pos
              # Any identifier, but not true, false, or null
              enum_value = if at?(:TRUE) || at?(:FALSE) || at?(:NULL)
                expect_token(:IDENTIFIER)
              else
                parse_name
              end
              v_directives = parse_directives
              enum_defn = EnumValueDefinition.new(pos: v_loc, definition_pos: defn_loc, description: description, name: enum_value, directives: v_directives, filename: @filename, source: self)
            end
            list << enum_defn if enum_defn
            break if status != :continue
          end
          expect_token :RCURLY
          list
        else
          EMPTY_ARRAY
        end
      end

      def parse_union_members
        if at?(:EQUALS)
          expect_token :EQUALS
          if at?(:PIPE)
            advance_token
          end
          list = [parse_type_name]
          while at?(:PIPE)
            advance_token
            list << parse_type_name
          end
          list
        else
          EMPTY_ARRAY
        end
      end

      def parse_implements
        if at?(:IMPLEMENTS)
          advance_token
          list = []
          while true
            advance_token if at?(:AMP)
            break unless at?(:IDENTIFIER)
            list << parse_type_name
          end
          list
        else
          EMPTY_ARRAY
        end
      end

      def parse_field_definitions
        expect_token :LCURLY
        list = []
        while !at?(:RCURLY)
          field_defn = nil
          status = parse_recoverable([:RCURLY]) do
            loc = pos
            description = if at?(:STRING); string_value; end
            defn_loc = pos
            name = parse_name
            arguments_definition = parse_argument_definitions
            expect_token :COLON
            type = self.type
            directives = parse_directives

            field_defn = FieldDefinition.new(pos: loc, definition_pos: defn_loc, description: description, name: name, arguments: arguments_definition, type: type, directives: directives, filename: @filename, source: self)
          end
          list << field_defn if field_defn
          break if status != :continue
        end
        expect_token :RCURLY
        list
      end

      def parse_argument_definitions
        if at?(:LPAREN)
          advance_token
          list = []
          while !at?(:RPAREN)
            item = nil
            status = parse_recoverable([:RPAREN]) { item = parse_input_value_definition }
            list << item if item
            break if status != :continue
          end
          expect_token :RPAREN
          list
        else
          EMPTY_ARRAY
        end
      end

      def parse_input_value_definition
        loc = pos
        description = if at?(:STRING); string_value; end
        defn_loc = pos
        name = parse_name
        expect_token :COLON
        type = self.type
        default_value = if at?(:EQUALS)
          advance_token
          value
        else
          nil
        end
        directives = parse_directives
        InputValueDefinition.new(pos: loc, definition_pos: defn_loc, description: description, name: name, type: type, default_value: default_value, directives: directives, filename: @filename, source: self)
      end

      def type
        parsed_type = case token_name
        when :IDENTIFIER
          parse_type_name
        when :LBRACKET
          list_type
        else
          nil
        end

        if at?(:BANG) && parsed_type
          parsed_type = Nodes::NonNullType.new(pos: pos, of_type: parsed_type, source: self)
          expect_token(:BANG)
        end
        parsed_type
      end

      def list_type
        loc = pos
        expect_token(:LBRACKET)
        inner_type = self.type
        parsed_list_type = if inner_type
          Nodes::ListType.new(pos: loc, of_type: inner_type, source: self)
        else
          nil
        end
        expect_token(:RBRACKET)
        parsed_list_type
      end

      def parse_operation_type
        val = if at?(:QUERY)
          "query"
        elsif at?(:MUTATION)
          "mutation"
        elsif at?(:SUBSCRIPTION)
          "subscription"
        else
          expect_one_of([:QUERY, :MUTATION, :SUBSCRIPTION])
        end
        advance_token
        val
      end

      def selection_set
        expect_token(:LCURLY)
        selections = []
        while @token_name != :RCURLY
          selection = nil
          status = parse_recoverable([:RCURLY]) do
            selection = if at?(:ELLIPSIS)
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

                Nodes::InlineFragment.new(pos: loc, type: if_type, directives: directives, selections: selection_set, filename: @filename, source: self)
              else
                name = parse_name_without_on
                directives = parse_directives

                # Can this ever happen?
                # expect_token(:IDENTIFIER) if at?(:ON)

                FragmentSpread.new(pos: loc, name: name, directives: directives, filename: @filename, source: self)
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

              Nodes::Field.new(pos: loc, field_alias: field_alias, name: name, arguments: arguments, directives: directives, selections: selection_set, filename: @filename, source: self)
            end
          end
          selections << selection if selection
          break if status != :continue
        end
        expect_token(:RCURLY)
        selections
      end

      def parse_name
        case token_name
        when :IDENTIFIER
          expect_token_value(:IDENTIFIER)
        when :SCHEMA
          advance_token
          "schema"
        when :SCALAR
          advance_token
          "scalar"
        when :IMPLEMENTS
          advance_token
          "implements"
        when :INTERFACE
          advance_token
          "interface"
        when :UNION
          advance_token
          "union"
        when :ENUM
          advance_token
          "enum"
        when :INPUT
          advance_token
          "input"
        when :DIRECTIVE
          advance_token
          "directive"
        when :TYPE
          advance_token
          "type"
        when :QUERY
          advance_token
          "query"
        when :MUTATION
          advance_token
          "mutation"
        when :SUBSCRIPTION
          advance_token
          "subscription"
        when :TRUE
          advance_token
          "true"
        when :FALSE
          advance_token
          "false"
        when :FRAGMENT
          advance_token
          "fragment"
        when :REPEATABLE
          advance_token
          "repeatable"
        when :NULL
          advance_token
          "null"
        when :ON
          advance_token
          "on"
        when :EXTEND
          advance_token
          "extend"
        else
          expect_token(:NAME)
        end
      end

      def parse_name_without_on
        if at?(:ON)
          expect_token(:IDENTIFIER)
        else
          parse_name
        end
      end

      def parse_type_name
        TypeName.new(pos: pos, name: parse_name, filename: @filename, source: self)
      end

      def parse_directives
        if at?(:DIR_SIGN)
          dirs = []
          while at?(:DIR_SIGN)
            loc = pos
            advance_token
            name = parse_name
            arguments = parse_arguments

            dirs << Nodes::Directive.new(pos: loc, name: name, arguments: arguments, filename: @filename, source: self)
          end
          dirs
        else
          EMPTY_ARRAY
        end
      end

      def parse_arguments
        if at?(:LPAREN)
          advance_token
          args = []
          while !at?(:RPAREN)
            argument = nil
            status = parse_recoverable([:RPAREN]) do
              loc = pos
              name = parse_name
              expect_token(:COLON)
              argument = Nodes::Argument.new(pos: loc, name: name, value: value, filename: @filename, source: self)
            end
            args << argument if argument
            break if status != :continue
          end
          if args.empty? && !@tolerant
            expect_token(:ARGUMENT_NAME) # At least one argument is required
          end
          expect_token(:RPAREN)
          args
        else
          EMPTY_ARRAY
        end
      end

      def string_value
        token_value = @lexer.string_value
        expect_token :STRING
        token_value
      end

      def value
        case token_name
        when :INT
          expect_token_value(:INT).to_i
        when :FLOAT
          expect_token_value(:FLOAT).to_f
        when :STRING
          string_value
        when :TRUE
          advance_token
          true
        when :FALSE
          advance_token
          false
        when :NULL
          advance_token
          NullValue.new(pos: pos, name: "null", filename: @filename, source: self)
        when :IDENTIFIER
          Nodes::Enum.new(pos: pos, name: expect_token_value(:IDENTIFIER), filename: @filename, source: self)
        when :LBRACKET
          advance_token
          list = []
          while !at?(:RBRACKET)
            item = nil
            status = parse_recoverable([:RBRACKET]) { item = value }
            list << item unless item.nil?
            break if status != :continue
          end
          expect_token(:RBRACKET)
          list
        when :LCURLY
          start = pos
          advance_token
          args = []
          while !at?(:RCURLY)
            input_arg = nil
            status = parse_recoverable([:RCURLY]) do
              loc = pos
              n = parse_name
              expect_token(:COLON)
              input_arg = Argument.new(pos: loc, name: n, value: value, filename: @filename, source: self)
            end
            args << input_arg if input_arg
            break if status != :continue
          end
          expect_token(:RCURLY)
          InputObject.new(pos: start, arguments: args, filename: @filename, source: self)
        when :VAR_SIGN
          loc = pos
          advance_token
          VariableIdentifier.new(pos: loc, name: parse_name, filename: @filename, source: self)
        when :SCHEMA
          advance_token
          Nodes::Enum.new(pos: pos, name: "schema", filename: @filename, source: self)
        when :SCALAR
          advance_token
          Nodes::Enum.new(pos: pos, name: "scalar", filename: @filename, source: self)
        when :IMPLEMENTS
          advance_token
          Nodes::Enum.new(pos: pos, name: "implements", filename: @filename, source: self)
        when :INTERFACE
          advance_token
          Nodes::Enum.new(pos: pos, name: "interface", filename: @filename, source: self)
        when :UNION
          advance_token
          Nodes::Enum.new(pos: pos, name: "union", filename: @filename, source: self)
        when :ENUM
          advance_token
          Nodes::Enum.new(pos: pos, name: "enum", filename: @filename, source: self)
        when :INPUT
          advance_token
          Nodes::Enum.new(pos: pos, name: "input", filename: @filename, source: self)
        when :DIRECTIVE
          advance_token
          Nodes::Enum.new(pos: pos, name: "directive", filename: @filename, source: self)
        when :TYPE
          advance_token
          Nodes::Enum.new(pos: pos, name: "type", filename: @filename, source: self)
        when :QUERY
          advance_token
          Nodes::Enum.new(pos: pos, name: "query", filename: @filename, source: self)
        when :MUTATION
          advance_token
          Nodes::Enum.new(pos: pos, name: "mutation", filename: @filename, source: self)
        when :SUBSCRIPTION
          advance_token
          Nodes::Enum.new(pos: pos, name: "subscription", filename: @filename, source: self)
        when :FRAGMENT
          advance_token
          Nodes::Enum.new(pos: pos, name: "fragment", filename: @filename, source: self)
        when :REPEATABLE
          advance_token
          Nodes::Enum.new(pos: pos, name: "repeatable", filename: @filename, source: self)
        when :ON
          advance_token
          Nodes::Enum.new(pos: pos, name: "on", filename: @filename, source: self)
        when :EXTEND
          advance_token
          Nodes::Enum.new(pos: pos, name: "extend", filename: @filename, source: self)
        else
          expect_token(:VALUE)
        end
      end

      def at?(expected_token_name)
        @token_name == expected_token_name
      end

      def expect_token(expected_token_name)
        unless @token_name == expected_token_name
          if @tolerant && @recovery_stops
            stop_key = [@token_name, @lexer.pos]
            if @recovery_stops.delete(stop_key)
              # An inner recovery already reported why this closing token is
              # here; consume it without adding a cascading diagnostic.
              advance_token
              return nil
            end
          end
          message = "Expected #{expected_token_name}, #{@token_name == false ? "not end of file" : "actual: #{@token_name} (#{debug_token_value.inspect})"}"
          if @tolerant && @token_name == false
            # The document ended before this construct was closed: report it
            # once and let the construct unwind with what was parsed so far.
            advance_token
            return handle_unexpected_eof(message)
          end
          raise_parse_error(message)
        end
        advance_token
      end

      # Hook for the diagnostic parser to record an unexpected-EOF error and
      # suppress any repeats (only the first missing delimiter is reported).
      def handle_unexpected_eof(message)
        raise_parse_error(message)
      end

      def expect_one_of(token_names)
        raise_parse_error("Expected one of #{token_names.join(", ")}, actual: #{token_name || "NOTHING"} (#{debug_token_value.inspect})")
      end

      # Run one item in a repeating construct (`selection_set`, argument
      # lists, variable definitions, ...).
      #
      # In a normal parse errors propagate immediately and fast-fail. In a
      # diagnostic parse the error is handed to {#recover_from_parse_error},
      # which resynchronizes the token stream at a bracket boundary, so that
      # following, independent items can still be parsed.
      #
      # `stops` lists the tokens which close this construct. Recovery returns:
      # - `:continue` when positioned at a fresh sibling item,
      # - `:stop` when positioned at (or beyond) the construct's closing token,
      # - `:eof` when the stream ended mid-construct.
      def parse_recoverable(stops, definition_start_only: false)
        unless @tolerant
          # Ordinary parse: fast-fail exactly as before recovery existed.
          yield
          return :continue
        end

        # Diagnostic parse: one item may require a couple of resync attempts,
        # but the position must always move forward: a token that can't start
        # a valid item is dropped exactly once, so a missing delimiter can
        # never fan out into dozens of repeated diagnostics.
        last_pos = nil
        loop do
          yield
          return :eof if @lexer.finished?
          return :continue
        rescue GraphQL::ParseError
          return :eof if @lexer.finished?
          result = recover_from_parse_error($!, stops, definition_start_only: definition_start_only)
          return result unless result == :continue
          return :eof if @lexer.finished?

          if @lexer.pos == last_pos
            # The same token keeps failing; skip it before trying again.
            advance_token
            return :eof if @lexer.finished?
          end
          last_pos = @lexer.pos
        end
      end

      # Resynchronize after a syntax error.
      #
      # Recovery only considers delimiters outside every balanced pair.
      # Delimiters never surface as tokens while inside a string or block
      # string -- the lexer emits a single `:STRING` token for the whole
      # literal -- so brackets in string contents can never be mistaken for
      # recovery points.
      def recover_from_parse_error(err, stops, definition_start_only: false)
        depth = 0
        loop do
          tok = @token_name
          return :eof if tok == false

          if depth == 0 && stops.include?(tok)
            # Positioned at the token that closes this construct.
            (@recovery_stops ||= {})[[tok, @lexer.pos]] = true
            return :stop
          end

          case tok
          when :LCURLY, :LPAREN, :LBRACKET
            depth += 1
          when :RCURLY, :RPAREN, :RBRACKET
            if depth == 0
              if definition_start_only
                # A dangling delimiter at document level is junk; skip it.
                advance_token
                next
              else
                # An unmatched closer: unwind to an outer construct, which
                # will see its closing token or EOF and end exactly once.
                (@recovery_stops ||= {})[[tok, @lexer.pos]] = true
                return :stop
              end
            end
            depth -= 1
          end

          if depth == 0 && (definition_start_only ? at_definition_start? : true)
            return :continue
          end

          advance_token
        end
      end

      # Tokens which may begin a top-level definition (including an optional
      # description string before SDL definitions).
      DEFINITION_START_TOKENS = [
        :LCURLY,
        :QUERY, :MUTATION, :SUBSCRIPTION,
        :FRAGMENT,
        :EXTEND,
        :SCHEMA, :SCALAR, :TYPE, :INTERFACE, :UNION, :ENUM, :INPUT,
        :DIRECTIVE,
        :STRING,
      ].freeze

      # Document-level definitions have no enclosing delimiter.
      DEFINITION_STOPS = [].freeze

      def at_definition_start?
        DEFINITION_START_TOKENS.include?(@token_name)
      end

      # After a failed top-level definition, advance until the next token that
      # may start a definition. Balanced delimiter pairs are skipped whole and
      # unmatched closers are consumed; string contents are never inspected
      # (they arrive as single `:STRING` tokens).
      def skip_to_next_definition
        depth = 0
        until @lexer.finished?
          tok = @token_name
          if depth == 0 && at_definition_start?
            return
          end
          case tok
          when :LCURLY, :LPAREN, :LBRACKET
            depth += 1
          when :RCURLY, :RPAREN, :RBRACKET
            depth -= 1 if depth > 0
          end
          advance_token
        end
      end

      def raise_parse_error(message)
        message += " at [#{@lexer.line_number}, #{@lexer.column_number}]"
        raise GraphQL::ParseError.new(
          message,
          @lexer.line_number,
          @lexer.column_number,
          @graphql_str,
          filename: @filename,
        )

      end

      # Only use when we care about the expected token's value
      def expect_token_value(tok)
        token_value = @lexer.token_value
        if @dedup_identifiers
          token_value = -token_value
        end
        expect_token(tok)
        token_value
      end

      # token_value works for when the scanner matched something
      # which is usually fine and it's good for it to be fast at that.
      def debug_token_value
        @lexer.debug_token_value(token_name)
      end
      class SchemaParser < Parser
        def initialize(*args, **kwargs)
          super
          @dedup_identifiers = true
        end
      end

      # A resynchronizing parser.
      #
      # Unlike {Parser}, which fast-fails on the first syntax error, this
      # parser keeps going: it records every independent error in
      # {#parse_errors} and preserves as much of the surrounding document
      # as possible. Documents returned from here are always flagged
      # ({Nodes::Document#malformed?}) when any error was reported, so they
      # can't accidentally be executed as if they were valid.
      #
      # Use {GraphQL.parse_with_diagnostics} rather than instantiating this
      # class directly.
      class DiagnosticParser < Parser
        # Maximum number of diagnostics reported for one document. Past this,
        # recovery aborts to avoid cascading into dozens of repeated errors.
        MAX_DIAGNOSTICS = 100

        def self.parse(graphql_str, filename: nil, trace: Tracing::NullTrace, max_tokens: nil)
          self.new(graphql_str, filename: filename, trace: trace, max_tokens: max_tokens).parse_with_diagnostics
        end

        attr_reader :parse_errors

        def initialize(graphql_str, filename: nil, trace: Tracing::NullTrace, max_tokens: nil)
          super(graphql_str, filename: filename, trace: trace, max_tokens: max_tokens)
          @tolerant = true
          @parse_errors = []
          @seen_positions = {}
          @reported_eof = false
        end

        # @return [Nodes::Document] The recovered document; its
        #   {Nodes::Document#parse_errors} contains all reported syntax errors.
        def parse_with_diagnostics
          document
        rescue GraphQL::ParseError => err
          record_error(err)
          build_document([])
        rescue DiagnosticAbort
          build_document([])
        end

        private

        def advance_token
          super
        rescue GraphQL::ParseError => err
          record_error(err)
          @lexer.advance_after_error
          @token_name = @lexer.finished? ? false : @lexer.advance
        end

        def raise_parse_error(message)
          super
        rescue GraphQL::ParseError => parse_err
          record_error(parse_err)
          raise
        end

        def handle_unexpected_eof(message)
          unless @reported_eof
            @reported_eof = true
            record_error(GraphQL::ParseError.new(
              message,
              @lexer.line_number,
              @lexer.column_number,
              @graphql_str,
              filename: @filename,
            ))
          end
          nil
        end

        def parse_document_definition
          errors_before = @parse_errors.size
          defn = definition
          if @parse_errors.size > errors_before
            # Keep the partial definition for tooling, but never let it be
            # mistaken for a valid one.
            defn.mark_malformed!
          end
          defn
        rescue GraphQL::ParseError
          nil
        end

        def build_document(definitions)
          doc = super
          doc.parse_errors = @parse_errors
          doc.mark_malformed! unless @parse_errors.empty?
          doc
        end

        def record_error(err)
          # Secondary errors at the same position are cascades from one broken
          # spot (e.g. a missing `)` first surfaces as a bad argument, then as
          # a missing closer). Keep only the first diagnostic at each spot.
          position_key = [err.line, err.col]
          return if @seen_positions.key?(position_key)
          @seen_positions[position_key] = true
          @parse_errors << err
          if @parse_errors.size >= MAX_DIAGNOSTICS
            raise DiagnosticAbort, err.message
          end
        end
      end
    end
  end
end
