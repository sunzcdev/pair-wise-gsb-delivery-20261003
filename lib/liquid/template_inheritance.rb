# frozen_string_literal: true

module Liquid
  # Optional template inheritance extension.
  #
  # This extension is not loaded by default. To enable it, require this file
  # and register its tags on an environment:
  #
  #   require 'liquid/template_inheritance'
  #
  #   environment = Liquid::Environment.build(file_system: my_file_system) do |env|
  #     Liquid::TemplateInheritance.register!(env)
  #   end
  #
  # It provides three tags:
  #
  #   {% extends 'parent' %}  - the template inherits the skeleton of 'parent'.
  #                             Must be the very first tag of the template (only
  #                             whitespace may precede it) and must appear at the
  #                             top level. Anything after it, except {% block %}
  #                             definitions, is ignored.
  #   {% block name %}...{% endblock %}
  #                           - defines a named, overridable region. In a child
  #                             template a top-level block replaces the parent's
  #                             block of the same name. Block names must be
  #                             unique within a template.
  #   {% super %}             - inside an overriding block, renders the content
  #                             of the next block with the same name up the
  #                             inheritance chain. Renders nothing when there
  #                             is no parent block.
  #
  # Parent templates are loaded through the environment's file system (via
  # PartialCache), exactly like the render tag, so the usual file system
  # restrictions apply. Overrides are tracked per render in the render context,
  # so parent templates kept in the partial cache are never mutated by the
  # children that extend them.
  module TemplateInheritance
    # Raised for inheritance-specific runtime failures, most notably circular
    # extends chains. The message contains the diagnostic chain of template
    # names that form the cycle.
    InheritanceError = Class.new(Liquid::Error)

    REGISTERS_KEY = :template_inheritance

    def self.register!(environment = Liquid::Environment.default)
      environment.register_tag('extends', Extends)
      environment.register_tag('block', Block)
      environment.register_tag('super', Super)
    end

    # Per-render mutable state, stored in the context registers. Because it
    # lives in the registers' per-context changes, it is rebuilt for every
    # render and never leaks into cached templates.
    class State
      attr_reader :override_stack, :super_stack, :chain

      def initialize
        # Array of {String => Block}, most-derived template first.
        @override_stack = []
        # Array of SuperFrame describing the active {% block %} overrides.
        @super_stack = []
        # Names of the templates in the current extends chain, used for
        # cycle detection.
        @chain = []
      end
    end

    SuperFrame = Struct.new(:block_name, :next_index, :fallback)

    def self.state(context)
      context.registers[REGISTERS_KEY] ||= State.new
    end

    def self.find_override(state, name, from_index)
      stack = state.override_stack
      index = from_index
      while index < stack.length
        tag = stack[index][name]
        return [tag, index] if tag

        index += 1
      end
      nil
    end

    # Renders the content for block +name+, looking for an override starting
    # at override stack index +from_index+. When no override exists, the
    # +fallback+ tag's own body is rendered (if any).
    def self.render_block_content(context, output, name, from_index, fallback)
      state = state(context)
      if (found = find_override(state, name, from_index))
        tag, index = found
        state.super_stack.push(SuperFrame.new(name, index + 1, fallback))
        begin
          tag.render_own_body(context, output)
        ensure
          state.super_stack.pop
        end
      elsif fallback
        state.super_stack.push(SuperFrame.new(name, from_index, nil))
        begin
          fallback.render_own_body(context, output)
        ensure
          state.super_stack.pop
        end
      end
      output
    end

    # {% extends 'parent' %}
    #
    # Slurps the remainder of the template at parse time: only top-level
    # {% block %} tags collected from its body are used as overrides,
    # everything else is ignored at render time.
    class Extends < Tag
      SYNTAX = /\A#{QuotedString}\z/o

      def initialize(tag_name, markup, options)
        super
        parse_with_selected_parser(markup)
      end

      def parse(tokenizer)
        unless parse_context.depth.zero?
          raise SyntaxError, "Liquid syntax error: 'extends' must be a top-level tag"
        end

        @body = parse_context.new_block_body
        @body.parse(tokenizer, parse_context) do |unknown_tag_name, _unknown_tag_markup|
          case unknown_tag_name
          when nil
            # end of the template
          when 'else', 'end'
            raise SyntaxError, parse_context.locale.t("errors.syntax.unexpected_outer_tag", tag: unknown_tag_name)
          else
            raise SyntaxError, parse_context.locale.t("errors.syntax.unknown_tag", tag: unknown_tag_name)
          end
        end
        @body.freeze
      end

      def render_to_output_buffer(context, output)
        if output.match?(/\S/)
          raise SyntaxError, "Liquid syntax error: 'extends' must be the first tag in the template"
        end

        template_name = @template_name
        state = TemplateInheritance.state(context)

        if (cycle_start = state.chain.index(template_name))
          cycle = (state.chain[cycle_start..] + [template_name]).join(' -> ')
          raise InheritanceError, "Liquid error: circular template inheritance detected (#{cycle})"
        end

        overrides = {}
        @body.nodelist.each do |node|
          next unless node.is_a?(Block)

          if overrides.key?(node.block_name)
            raise SyntaxError, "Liquid syntax error: duplicate block '#{node.block_name}' in template"
          end
          overrides[node.block_name] = node
        end

        partial = PartialCache.load(
          template_name,
          context: context,
          parse_context: parse_context,
        )

        state.override_stack.push(overrides)
        state.chain.push(template_name)
        begin
          partial.render_to_output_buffer(context, output)
        ensure
          state.override_stack.pop
          state.chain.pop
        end

        output
      end

      def strict2_parse(markup)
        p = parse_context.new_parser(markup)
        @template_name = parse_expression(p.consume(:string), safe: true)
        p.consume(:end_of_string)
      end

      def strict_parse(markup)
        lax_parse(markup)
      end

      def lax_parse(markup)
        unless SYNTAX.match?(markup.strip)
          raise SyntaxError, "Liquid syntax error: syntax error in 'extends' - Valid syntax: extends 'template'"
        end

        @template_name = parse_expression(markup.strip)
      end
    end

    # {% block name %}...{% endblock %}
    class Block < Liquid::Block
      SYNTAX = /\A(?<block_name>[a-zA-Z_]\w*)\z/

      attr_reader :block_name

      def initialize(tag_name, markup, options)
        super
        unless (match = SYNTAX.match(markup.strip))
          raise SyntaxError, "Liquid syntax error: syntax error in 'block' - Valid syntax: block name"
        end

        @block_name = match[:block_name]
      end

      # The region name (block_name) must not leak into the delimiter:
      # every block tag is closed by {% endblock %}.
      def block_delimiter
        'endblock'
      end

      def render_to_output_buffer(context, output)
        TemplateInheritance.render_block_content(context, output, block_name, 0, self)
      end

      # Renders this block's own body, bypassing override resolution.
      def render_own_body(context, output)
        @body.render_to_output_buffer(context, output)
      end
    end

    # {% super %}
    class Super < Tag
      def render_to_output_buffer(context, output)
        frame = TemplateInheritance.state(context).super_stack.last
        return output unless frame

        TemplateInheritance.render_block_content(
          context, output, frame.block_name, frame.next_index, frame.fallback
        )
      end
    end
  end
end
