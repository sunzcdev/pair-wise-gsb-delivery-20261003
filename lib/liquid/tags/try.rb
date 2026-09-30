# frozen_string_literal: true

require 'liquid/drop'

module Liquid
  # @liquid_public_docs
  # @liquid_type tag
  # @liquid_category syntax
  # @liquid_name try
  # @liquid_summary
  #   Renders a protected block, falling back to alternative content when a
  #   template error occurs inside the block.
  # @liquid_description
  #   The `try` tag renders its body into a temporary buffer. If the body
  #   renders without a template error, the buffered output is committed to
  #   the template output. If a `Liquid::Error` (for example an undefined
  #   variable in strict mode, a missing partial, or a filter error) is raised
  #   while rendering the body, everything the body has written so far is
  #   discarded and the `fallback` branch is rendered instead.
  #
  #   Only documented template exceptions (`Liquid::Error` and its subclasses)
  #   are handled. Resource limit errors (`Liquid::MemoryError`) and any other
  #   exception are not caught and keep propagating.
  #
  #   Variables assigned inside the `try` block (with `assign` or `capture`)
  #   do not leak to the outer scope, whether the block succeeds or fails.
  #   Resource limits consumed by the discarded output are not rolled back.
  #
  #   Inside the `fallback` branch, an `error` object is available with safe
  #   diagnostic information about the caught error: `message`, `line_number`,
  #   `template_name`, and `type`.
  #
  #   The `fallback` branch is optional. Without it, a failed block renders
  #   nothing.
  # @liquid_syntax
  #   {% try %}
  #     expression
  #   {% fallback %}
  #     fallback expression
  #   {% endtry %}
  # @liquid_syntax_keyword expression The content to render when no error occurs.
  # @liquid_syntax_keyword fallback expression The content to render when the protected block fails.
  class Try < Block
    # Safe diagnostic view of a caught template error, exposed to the
    # fallback branch as `error`. Being a Drop, only the explicitly defined
    # methods below are accessible from templates.
    class ErrorDrop < Liquid::Drop
      def initialize(error)
        super()
        @error = error
      end

      def message
        @error.message
      end

      def line_number
        @error.line_number
      end

      def template_name
        @error.template_name
      end

      def type
        @error.class.name
      end
    end

    def initialize(tag_name, markup, options)
      super
      @body     = new_body
      @fallback = nil
    end

    def parse(tokens)
      while parse_body(active_body, tokens)
      end
      @body.freeze
      @fallback&.freeze
    end

    def nodelist
      @fallback ? @body.nodelist + @fallback.nodelist : @body.nodelist
    end

    def unknown_tag(tag, markup, tokens)
      if tag == 'fallback'
        if @fallback
          raise SyntaxError, parse_context.locale.t("errors.syntax.try_multiple_fallback")
        end

        @fallback = new_body
      else
        super
      end
    end

    def render_to_output_buffer(context, output)
      # Assignments (assign/capture) write to the outermost scope, so snapshot
      # and restore it to keep the block's local assignments from leaking.
      saved_outer_scope = context.scopes.last.dup
      begin
        error = render_body(context, output)
        render_fallback(context, output, error) if error
      ensure
        context.scopes.last.replace(saved_outer_scope)
      end
      output
    end

    private

    def active_body
      @fallback || @body
    end

    # Renders the protected body into a temporary buffer, committing it to
    # the output on success. Returns the caught Liquid::Error on failure.
    def render_body(context, output)
      buffer = +''
      previous_exception_renderer = context.exception_renderer
      # Force template errors to re-raise so they reach this tag instead of
      # being rendered as error text by the surrounding block body.
      context.exception_renderer = Liquid::RAISE_EXCEPTION_LAMBDA
      begin
        context.stack do
          @body.render_to_output_buffer(context, buffer)
        end
      rescue Liquid::MemoryError
        # Resource limits are process-level protection: never caught, and the
        # budget consumed by the discarded buffer is not rolled back.
        raise
      rescue Liquid::Error => e
        return e
      ensure
        context.exception_renderer = previous_exception_renderer
      end
      output << buffer
      nil
    end

    def render_fallback(context, output, error)
      return output unless @fallback

      context.stack('error' => ErrorDrop.new(error)) do
        @fallback.render_to_output_buffer(context, output)
      end
      output
    end
  end
end
