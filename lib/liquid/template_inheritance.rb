# frozen_string_literal: true

module Liquid
  # TemplateInheritance is an optional extension that adds template
  # inheritance to Liquid. The common skeleton of a family of templates
  # (e.g. notification emails) lives in a parent template, while children
  # only override named blocks such as the body or the footer:
  #
  #   # layout (loaded through the environment's file system)
  #   Subject: update
  #   {% block body %}generic body{% endblock %}
  #   {% block footer %}sent by Acme{% endblock %}
  #
  #   # child template
  #   {% extends 'notification' %}
  #   {% block body %}Your order shipped{% endblock %}
  #
  # Inside an overriding block, {% super %} renders the content of the
  # nearest ancestor block with the same name.
  #
  # The extension is opt-in. Register the tags on an environment to use it:
  #
  #   environment = Liquid::Environment.build do |env|
  #     Liquid::TemplateInheritance.register!(env)
  #   end
  #
  # Semantics:
  #
  # * +extends+ must be a top-level tag, must appear before any
  #   non-whitespace output, and may appear at most once per template.
  #   Violations raise a Liquid::SyntaxError.
  # * +block+ must be a top-level tag and block names must be unique per
  #   template. Violations raise a Liquid::SyntaxError at parse time.
  # * +extends+ and +block+ cannot be used inside partials (templates
  #   loaded through +render+/+include+); +super+ renders nothing when
  #   there is no parent block content.
  # * In a multi-level chain (child -> mid -> base) the most derived
  #   override wins; +super+ walks one level up the chain at a time.
  # * Parent templates are loaded through the environment's controlled
  #   file system and are cached per render context. Overrides are applied
  #   through the render context only, so a cached parent template is never
  #   mutated by a child.
  # * A cycle in the inheritance chain raises Liquid::StackLevelError with
  #   the offending chain (e.g. "alpha -> beta -> alpha").
  module TemplateInheritance
    class << self
      # Registers the +extends+, +block+ and +super+ tags on the given
      # environment (defaults to the global environment).
      def register!(environment = Environment.default)
        environment.register_tag('extends', Extends)
        environment.register_tag('block',   Block)
        environment.register_tag('super',   Super)
        environment
      end

      # Per-template parse-time state. A fresh ParseContext is created for
      # every top-level template parse, so keying the state off the parse
      # context keeps templates isolated from each other.
      #
      # @api private
      def template_state(parse_context)
        parse_context.instance_variable_get(:@template_inheritance_state) ||
          parse_context.instance_variable_set(:@template_inheritance_state, { extends: nil, blocks: {} })
      end

      # Runs +block+ with +chain+ (a list of Block tags, most derived first)
      # as the content {% super %} resolves to.
      #
      # @api private
      def with_super_chain(context, chain)
        stack = (context.registers[:inheritance_super] ||= [])
        stack << chain
        begin
          yield
        ensure
          stack.pop
        end
      end
    end

    # {% extends 'parent' %} - renders the parent template, replacing its
    # named blocks with the overrides defined in the current template.
    class Extends < Tag
      SYNTAX = /\A\s*(#{QuotedString})\s*\z/o

      attr_reader :parent_name

      def initialize(tag_name, markup, parse_context)
        super
        TemplateInheritance.check_not_partial!(tag_name, parse_context)
        if parse_context.depth > 0
          raise SyntaxError, parse_context.locale.t("errors.syntax.extends_nested")
        end
        unless (matches = SYNTAX.match(markup))
          raise SyntaxError, parse_context.locale.t("errors.syntax.extends")
        end
        @parent_name = matches[1][1...-1]

        state = TemplateInheritance.template_state(parse_context)
        if state[:extends]
          raise SyntaxError, parse_context.locale.t("errors.syntax.extends_multiple")
        end
        state[:extends] = self
      end

      def render_to_output_buffer(context, output)
        if output.match?(/\S/)
          raise SyntaxError, parse_context.locale.t("errors.syntax.extends_position")
        end

        chain = (context.registers[:inheritance_chain] ||= [])
        if chain.include?(@parent_name)
          cycle = chain + [@parent_name]
          raise StackLevelError, parse_context.locale.t("errors.syntax.inheritance_cycle", cycle: cycle.join(" -> "))
        end

        parent = load_parent(context)

        overrides = (context.registers[:inheritance_blocks] ||= {})
        own_blocks = TemplateInheritance.template_state(parse_context)[:blocks]
        own_blocks.each do |name, block|
          (overrides[name] ||= []) << block
        end

        chain << @parent_name
        begin
          parent.render_to_output_buffer(context, output)
        ensure
          chain.pop
          own_blocks.each_key { |name| overrides[name].pop }
        end

        output
      end

      private

      # Parent templates are loaded through the controlled file system and
      # cached in the render context, mirroring PartialCache. The cached
      # template is only ever read; overrides are applied through the
      # context, so a child can never permanently modify it.
      def load_parent(context)
        cache = context.registers[:cached_partials]
        cache_key = "extends:#{@parent_name}:#{parse_context.error_mode}"
        cached = cache[cache_key]
        return cached if cached

        file_system = context.registers[:file_system]
        source = file_system.read_template_file(@parent_name)

        template_factory = context.registers[:template_factory]
        template = template_factory.for(@parent_name)

        begin
          template.parse(source, environment: parse_context.environment, error_mode: parse_context.error_mode)
        rescue Liquid::Error => e
          e.template_name = template&.name || @parent_name
          raise e
        end

        template.name ||= @parent_name

        cache[cache_key] = template
      end
    end

    # {% block name %}...{% endblock %} - a named, overridable section.
    #
    # In a template without +extends+, a block dispatches to the most
    # derived override registered by an extending template, falling back
    # to its own body. In a template with +extends+, top-level blocks only
    # provide overrides and never render inline.
    class Block < Liquid::Block
      SYNTAX = /\A[a-zA-Z_]\w*\z/

      attr_reader :block_id

      def initialize(tag_name, markup, parse_context)
        super
        TemplateInheritance.check_not_partial!(tag_name, parse_context)
        if parse_context.depth > 0
          raise SyntaxError, parse_context.locale.t("errors.syntax.block_nested")
        end

        @block_id = markup.strip
        unless SYNTAX.match?(@block_id)
          raise SyntaxError, parse_context.locale.t("errors.syntax.block")
        end

        state = TemplateInheritance.template_state(parse_context)
        if state[:blocks].key?(@block_id)
          raise SyntaxError, parse_context.locale.t("errors.syntax.block_duplicate", name: @block_id)
        end
        state[:blocks][@block_id] = self
      end

      def render_to_output_buffer(context, output)
        if TemplateInheritance.template_state(parse_context)[:extends]
          # This template extends a parent: its blocks only provide
          # overrides and must not render inline.
          return output
        end

        chain = override_chain(context)
        if chain
          render_override(context, output, chain)
        else
          render_body(context, output)
        end
      end

      # Renders this block's own body, without consulting overrides.
      def render_body(context, output)
        @body.render_to_output_buffer(context, output)
      end

      private

      def override_chain(context)
        overrides = context.registers[:inheritance_blocks]
        return nil unless overrides

        chain = overrides[@block_id]
        return nil if chain.nil? || chain.empty?

        # The dispatching block is the innermost content for {% super %}.
        chain + [self]
      end

      def render_override(context, output, chain)
        block = chain.first
        TemplateInheritance.with_super_chain(context, chain[1..]) do
          block.render_body(context, output)
        end
        output
      end
    end

    # {% super %} - renders the content of the nearest ancestor block with
    # the same name. Renders nothing when there is no parent block content.
    class Super < Tag
      def initialize(tag_name, markup, parse_context)
        super
        unless markup.strip.empty?
          raise SyntaxError, parse_context.locale.t("errors.syntax.super")
        end
      end

      def render_to_output_buffer(context, output)
        stack = context.registers[:inheritance_super]
        chain = stack&.last
        return output if chain.nil? || chain.empty?

        block = chain.first
        TemplateInheritance.with_super_chain(context, chain[1..]) do
          block.render_body(context, output)
        end
        output
      end
    end

    # @api private
    def self.check_not_partial!(tag_name, parse_context)
      if parse_context.partial
        raise SyntaxError, parse_context.locale.t("errors.syntax.inheritance_partial", tag: tag_name)
      end
    end
  end
end
