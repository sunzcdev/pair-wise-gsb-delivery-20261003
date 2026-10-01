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

        # Parse `graphql_str` in diagnostic mode.
        #
        # Unlike {.parse}, this doesn't stop at the first syntax error:
        # recognizable structure is kept, errors after the first one are
        # reported too, and unparseable sections are replaced with
        # {Nodes::UnrecoverableDefinition} (or {Nodes::UnrecoverableSelection})
        # nodes.
        #
        # The returned document is marked as invalid ({Nodes::Document#valid?}
        # returns false) when any errors were found, so it can't be mistaken
        # for an executable document.
        #
        # @return [Nodes::Document]
        def parse_with_diagnostics(graphql_str, filename: nil, trace: Tracing::NullTrace, max_tokens: nil)
          if graphql_str.nil?
            raise GraphQL::ParseError.new("No query string was present", nil, nil, nil)
          end
          parse_errors = []
          lexer = Lexer.new(
            graphql_str, filename: filename, max_tokens: max_tokens,
            diagnostic: true, errors: parse_errors
          )
          self.new(
            graphql_str, filename: filename, trace: trace, max_tokens: max_tokens,
            lexer: lexer, diagnostic: true, parse_errors: parse_errors
          ).parse
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

      def initialize(
        graphql_str, filename: nil, trace: Tracing::NullTrace, max_tokens: nil,
        lexer: nil, diagnostic: false, parse_errors: nil
      )
        if graphql_str.nil?
          raise GraphQL::ParseError.new("No query string was present", nil, nil, nil)
        end
        @lexer = lexer || Lexer.new(graphql_str, filename: filename, max_tokens: max_tokens)
        @graphql_str = graphql_str
        @filename = filename
        @trace = trace
        @dedup_identifiers = false
        @lines_at = nil
        @diagnostic = diagnostic
        @parse_errors = parse_errors
        @error_positions = {}
        @definition_boundary_reached = false
        @definition_boundary_reported = false
      end

      def parse
        @document ||= begin
          @trace.parse(query_string: @graphql_str) do
            doc = document
            if @diagnostic
              doc.parse_errors = parse_errors
            end
            doc
          end
        rescue SystemStackError
          raise GraphQL::ParseError.new("This query is too large to execute.", nil, nil, @query_str, filename: @filename)
        end
      end

      attr_reader :parse_errors

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
          defns << parse_definition_with_recovery
        else
          if @diagnostic
            # Only ignored characters is not a valid document
            err = GraphQL::ParseError.new(
              "Unexpected end of document", nil, nil, @graphql_str, filename: @filename
            )
            record_parse_error(err)
          else
            # Only ignored characters is not a valid document
            raise GraphQL::ParseError.new("Unexpected end of document", nil, nil, @graphql_str)
          end
        end
        while !@lexer.finished?
          before = @lexer.tokens_count
          defn = parse_definition_with_recovery
          defns << defn unless defn.nil?
          # Recovery couldn't advance (e.g. deeply malformed SDL at EOF); stop.
          break if @lexer.tokens_count == before
        end
        Document.new(pos: 0, definitions: defns, filename: @filename, source: self)
      end

      DEFINITION_START_TOKENS = [
        :FRAGMENT, :QUERY, :MUTATION, :SUBSCRIPTION, :LCURLY, :EXTEND, :STRING,
        :SCHEMA, :SCALAR, :TYPE, :INTERFACE, :UNION, :ENUM, :INPUT, :DIRECTIVE,
      ].freeze

      DEFINITION_KEYWORD_TOKENS = (DEFINITION_START_TOKENS - [:LCURLY, :STRING]).freeze

      # Parse one top-level definition. If an error occurs within it, record the
      # error, replace the whole definition with an {Nodes::UnrecoverableDefinition}
      # marker and skip ahead (tracking nested brackets) to the next definition.
      def parse_definition_with_recovery
        return definition unless @diagnostic
        @definition_boundary_reached = false
        @definition_boundary_reported = false
        # A leftover closer from a recovered (already reported) section.
        if CLOSE_TOKENS.include?(token_name) && @lexer.bracket_depth == 0
          record_parse_error(build_parse_error("Unexpected #{token_name}"))
          advance_token
          return nil
        end
        start_pos = pos
        begin
          definition
        rescue GraphQL::ParseError => err
          record_parse_error(err)
          # A leftover closer after a successfully-recovered definition: skip
          # it without inserting an extra marker definition.
          if CLOSE_TOKENS.include?(token_name) && @lexer.bracket_depth <= 1
            advance_token
            return nil
          end
          # The broken definition ran into the next definition's keyword
          # while its brackets were still open: attribute the missing closer
          # to the broken definition before synchronizing.
          if DEFINITION_KEYWORD_TOKENS.include?(token_name) && @lexer.bracket_depth == 1
            @lexer.reset_bracket_depth_for_recovery
          end
          synchronize_to_definition unless @lexer.finished?
          Nodes::UnrecoverableDefinition.new(
            pos: start_pos,
            source_text: source_since(start_pos),
            parse_error: err,
            filename: @filename,
            source: self,
          )
        end
      end

      # Skip tokens until the lexer is back at definition nesting depth and the
      # next token could begin a new definition. Brackets (and only brackets,
      # since strings are consumed whole by the lexer) are depth-tracked so a
      # `}` inside a string or a nested selection set can't fool recovery.
      def synchronize_to_definition
        # If a definition starts while brackets are still open from the broken
        # definition, the missing closer belongs to that definition (e.g. a
        # dropped `}`). Treat the nesting as having been closed here so the
        # following definition isn't parsed at the wrong depth.
        if DEFINITION_KEYWORD_TOKENS.include?(token_name) && @lexer.bracket_depth == 1
          @lexer.reset_bracket_depth_for_recovery
        end
        advance_token while !@lexer.finished? &&
          (@lexer.bracket_depth > 0 || !(DEFINITION_START_TOKENS - [:LCURLY]).include?(token_name))
      end

      def source_since(start_pos)
        end_pos = pos || @graphql_str.bytesize
        @graphql_str.byteslice(start_pos, end_pos - start_pos)
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
            start_depth = @diagnostic ? @lexer.bracket_depth : nil
            loop do
              break if at?(:RPAREN) || @lexer.finished? || (@diagnostic && @lexer.bracket_depth < start_depth)
              if @diagnostic
                begin
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
                rescue GraphQL::ParseError => err
                  break if recover_list_error(err, start_depth, :RPAREN, [:RPAREN, :VAR_SIGN]) == :stop
                end
              else
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
            end
            expect_close_token(:RPAREN, start_depth)
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
            list << parse_input_value_definition
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
            list << EnumValueDefinition.new(pos: v_loc, definition_pos: defn_loc, description: description, name: enum_value, directives: v_directives, filename: @filename, source: self)
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
          loc = pos
          description = if at?(:STRING); string_value; end
          defn_loc = pos
          name = parse_name
          arguments_definition = parse_argument_definitions
          expect_token :COLON
          type = self.type
          directives = parse_directives

          list << FieldDefinition.new(pos: loc, definition_pos: defn_loc, description: description, name: name, arguments: arguments_definition, type: type, directives: directives, filename: @filename, source: self)
        end
        expect_token :RCURLY
        list
      end

      def parse_argument_definitions
        if at?(:LPAREN)
          advance_token
          list = []
          while !at?(:RPAREN)
            list << parse_input_value_definition
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
        start_depth = @diagnostic ? @lexer.bracket_depth : nil
        boundary_from_inner = false
        outer_depth = @diagnostic ? start_depth - 1 : nil
        loop do
          if at?(:RCURLY) || @lexer.finished? ||
              (@diagnostic && (@lexer.bracket_depth < outer_depth || @definition_boundary_reached || definition_boundary_at?(start_depth)))
            boundary_from_inner = @definition_boundary_reached
            break
          end
          if @diagnostic
            field_start_pos = pos
            selection = begin
              parse_selection
            rescue GraphQL::ParseError => err
              record_parse_error(err)
              if @definition_boundary_reached
                # An inner selection set unwound to a definition boundary.
                return selections
              elsif @lexer.finished? ||
                  definition_boundary_at?(start_depth) ||
                  (at?(:RCURLY) && @lexer.bracket_depth < start_depth) ||
                  (CLOSE_TOKENS.include?(token_name) && @lexer.bracket_depth < outer_depth)
                # Truncated input or a missing closing brace consumed by the
                # outer levels: stop here with what was parsed so far.
                expect_close_token(:RCURLY, start_depth)
                return selections
              end
              synchronize_within_selection(start_depth)
              Nodes::UnrecoverableSelection.new(
                pos: field_start_pos,
                source_text: source_since(field_start_pos),
                parse_error: err,
                filename: @filename,
                source: self,
              )
            end
            selections << selection
            if at?(:RCURLY) || @lexer.finished? || @lexer.bracket_depth < start_depth ||
                @definition_boundary_reached || definition_boundary_at?(start_depth)
              boundary_from_inner = @definition_boundary_reached ||
                (at?(:RCURLY) && @lexer.bracket_depth < outer_depth) ||
                definition_boundary_at?(start_depth)
              break
            end
          else
            selections << parse_selection
          end
        end
        if !@diagnostic
          expect_token(:RCURLY)
        elsif !boundary_from_inner && !(at?(:RCURLY) && @lexer.bracket_depth < outer_depth)
          expect_close_token(:RCURLY, start_depth)
        elsif at?(:RCURLY) && @lexer.bracket_depth < outer_depth
          # Unclaimed closer from a dropped brace: leave it for the document
          # loop, which reports exactly one "unexpected closer" diagnostic.
        end
        @definition_boundary_reached = true if boundary_from_inner
        selections
      end

      SELECTION_START_TOKENS = [
        :IDENTIFIER, :ELLIPSIS,
        :QUERY, :MUTATION, :SUBSCRIPTION, :FRAGMENT, :SCHEMA, :SCALAR, :TYPE,
        :INTERFACE, :UNION, :ENUM, :INPUT, :DIRECTIVE, :EXTEND, :TRUE, :FALSE,
        :NULL, :ON, :REPEATABLE, :IMPLEMENTS,
      ].freeze

      # Skip the rest of a broken selection but stay inside the enclosing
      # selection set (brackets nested in arguments or sub-selections are
      # tracked, and strings are never inspected).
      def synchronize_within_selection(depth)
        advance_token while !@lexer.finished? &&
          (@lexer.bracket_depth > depth ||
            (@lexer.bracket_depth == depth &&
              !SELECTION_START_TOKENS.include?(token_name)))
      end

      # A definition keyword at the enclosing operation's depth means the
      # current (broken) selection set ended because a closer was dropped.
      def definition_boundary_at?(selection_depth)
        at_boundary = DEFINITION_KEYWORD_TOKENS.include?(token_name) &&
          @lexer.bracket_depth.between?(1, selection_depth)
        if at_boundary && !@definition_boundary_reported
          @definition_boundary_reported = true
          record_parse_error(build_parse_error("Expected RCURLY, actual: #{token_name} (#{debug_token_value.inspect})"))
          @definition_boundary_reached = true
          @lexer.reset_bracket_depth_for_recovery
        end
        at_boundary
      end

      def parse_selection
        if at?(:ELLIPSIS)
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
          start_depth = @diagnostic ? @lexer.bracket_depth : nil
          loop do
            break if at?(:RPAREN) || @lexer.finished? || (@diagnostic && @lexer.bracket_depth < start_depth)
            if @diagnostic
              begin
                loc = pos
                name = parse_name
                expect_token(:COLON)
                args << Nodes::Argument.new(pos: loc, name: name, value: value, filename: @filename, source: self)
              rescue GraphQL::ParseError => err
                break if recover_list_error(err, start_depth, :RPAREN, [:RPAREN, :IDENTIFIER]) == :stop
              end
            else
              loc = pos
              name = parse_name
              expect_token(:COLON)
              args << Nodes::Argument.new(pos: loc, name: name, value: value, filename: @filename, source: self)
            end
          end
          if args.empty?
            expect_token(:ARGUMENT_NAME) # At least one argument is required
          end
          expect_close_token(:RPAREN, start_depth)
          args
        else
          EMPTY_ARRAY
        end
      end

      # Skip a malformed list item while staying within the enclosing bracket.
      def synchronize_to_item(depth, resume_tokens)
        advance_token while !@lexer.finished? &&
          (@lexer.bracket_depth > depth ||
            (@lexer.bracket_depth == depth &&
              !resume_tokens.include?(token_name)))
      end

      CLOSE_TOKENS = [:RCURLY, :RPAREN, :RBRACKET].freeze

      VALUE_RESUME_TOKENS = [
        :RBRACKET, :INT, :FLOAT, :STRING, :TRUE, :FALSE, :NULL,
        :IDENTIFIER, :LBRACKET, :LCURLY, :VAR_SIGN,
      ].freeze

      # Consume a list's closing token in diagnostic mode, but leave it alone
      # if it belongs to an enclosing construct (depth already unwound).
      def expect_close_token(close_token, start_depth)
        return expect_token(close_token) unless @diagnostic
        closer_depth = start_depth - 1
        if @lexer.finished?
          build_and_record_close_error(close_token)
        elsif at?(close_token) && @lexer.bracket_depth == closer_depth
          advance_token
        elsif DEFINITION_KEYWORD_TOKENS.include?(token_name) && @lexer.bracket_depth < start_depth
          # A new definition starts where a closer was expected: the missing
          # closer was already reported when parsing stopped. Leave the keyword
          # for the document loop and unwind the nesting.
          @lexer.reset_bracket_depth_for_recovery
          @definition_boundary_reached = true
        else
          expect_token(close_token)
        end
      end

      def build_and_record_close_error(close_token)
        record_parse_error(build_parse_error("Expected #{close_token}, not end of file"))
      end

      # Shared recovery for a failed item in a bracket-delimited list.
      # Returns `:stop` when the caller must end its list (the list's own
      # closing token was consumed, or input ended); otherwise synchronizes to
      # the next parseable item and returns `:continue`.
      def recover_list_error(err, start_depth, close_token, resume_tokens)
        record_parse_error(err)
        if @lexer.finished?
          return :stop
        end
        synchronize_to_item(start_depth, resume_tokens)
        return :stop if @lexer.finished?
        if CLOSE_TOKENS.include?(token_name) && @lexer.bracket_depth <= start_depth
          return :stop
        end
        :continue
      end

      def string_value
        token_value = @diagnostic ? (@lexer.consume_malformed_string_value || @lexer.string_value) : @lexer.string_value
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
          start_depth = @diagnostic ? @lexer.bracket_depth : nil
          loop do
            break if at?(:RBRACKET) || @lexer.finished? || (@diagnostic && @lexer.bracket_depth < start_depth)
            if @diagnostic
              begin
                list << value
              rescue GraphQL::ParseError => err
                if recover_list_error(err, start_depth, :RBRACKET, VALUE_RESUME_TOKENS) == :stop
                  expect_close_token(:RBRACKET, start_depth)
                  return list
                end
              end
            else
              list << value
            end
          end
          expect_close_token(:RBRACKET, start_depth)
          list
        when :LCURLY
          start = pos
          advance_token
          args = []
          start_depth = @diagnostic ? @lexer.bracket_depth : nil
          loop do
            break if at?(:RCURLY) || @lexer.finished? || (@diagnostic && @lexer.bracket_depth < start_depth)
            if @diagnostic
              begin
                loc = pos
                n = parse_name
                expect_token(:COLON)
                args << Argument.new(pos: loc, name: n, value: value, filename: @filename, source: self)
              rescue GraphQL::ParseError => err
                if recover_list_error(err, start_depth, :RCURLY, [:RCURLY, :IDENTIFIER]) == :stop
                  expect_close_token(:RCURLY, start_depth)
                  return InputObject.new(pos: start, arguments: args, filename: @filename, source: self)
                end
              end
            else
              loc = pos
              n = parse_name
              expect_token(:COLON)
              args << Argument.new(pos: loc, name: n, value: value, filename: @filename, source: self)
            end
          end
          expect_close_token(:RCURLY, start_depth)
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
          if @diagnostic && @lexer.finished? && CLOSING_TOKENS.include?(expected_token_name)
            # At end of file while waiting for a closing bracket: report it as a
            # diagnostic but keep the structure parsed so far instead of failing
            # the whole enclosing definition.
            record_parse_error(build_parse_error("Expected #{expected_token_name}, not end of file"))
            return
          end
          raise_parse_error("Expected #{expected_token_name}, #{@token_name == false ? "not end of file" : "actual: #{@token_name} (#{debug_token_value.inspect})"}")
        end
        advance_token
      end

      CLOSING_TOKENS = [:RCURLY, :RPAREN, :RBRACKET].freeze

      def expect_one_of(token_names)
        raise_parse_error("Expected one of #{token_names.join(", ")}, actual: #{token_name || "NOTHING"} (#{debug_token_value.inspect})")
      end

      def build_parse_error(message)
        message += " at [#{@lexer.line_number}, #{@lexer.column_number}]"
        GraphQL::ParseError.new(
          message,
          @lexer.line_number,
          @lexer.column_number,
          @graphql_str,
          filename: @filename,
        )
      end

      def raise_parse_error(message)
        raise build_parse_error(message)
      end

      # Store an error encountered during diagnostic parsing. Several recovery
      # steps can notice the same underlying problem (e.g. one missing `{`
      # while unwinding nested selection sets), so duplicates at the same
      # location are reported only once.
      def record_parse_error(err)
        @parse_errors ||= []
        key = [err.line, err.col]
        return err if @error_positions.key?(key) ||
          @parse_errors.any? { |existing| existing.line == err.line && existing.col == err.col }
        @error_positions[key] = true
        @parse_errors << err
        err
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
    end
  end
end
