# frozen_string_literal: true

module Liquid
  # @liquid_public_docs
  # @liquid_type tag
  # @liquid_category error handling
  # @liquid_name protect
  # @liquid_summary
  #   Renders a protected block, or a fallback block when the protected block raises a Liquid template error.
  # @liquid_description
  #   Output written by the protected block is committed only when the entire block succeeds. Assignments made
  #   inside either block are local to that block. Resource usage consumed by a failed protected block is not
  #   refunded.
  #
  #   The fallback block can access the handled error through the `error` object. The object only exposes
  #   `message`, `template_name`, `line_number`, and `type`.
  # @liquid_syntax
  #   {% protect %}
  #     expression
  #   {% fallback %}
  #     fallback_expression
  #   {% endprotect %}
  class Protect < Block
    FALLBACK_TAG = 'fallback'
    ERROR_TYPES = {
      'ArgumentError' => 'argument',
      'ContextError' => 'context',
      'FileSystemError' => 'file_system',
      'StandardError' => 'standard_error',
      'SyntaxError' => 'syntax',
      'StackLevelError' => 'stack_level',
      'ZeroDivisionError' => 'zero_division',
      'FloatDomainError' => 'float_domain',
      'UndefinedVariable' => 'undefined_variable',
      'UndefinedDropMethod' => 'undefined_drop_method',
      'UndefinedFilter' => 'undefined_filter',
      'MethodOverrideError' => 'method_override',
      'DisabledError' => 'disabled',
      'TemplateEncodingError' => 'template_encoding',
    }.freeze
    private_constant :ERROR_TYPES

    def initialize(tag_name, markup, options)
      super
      unless markup.strip.empty?
        raise SyntaxError, options[:locale].t('errors.syntax.block_tag_unexpected_args', tag: tag_name)
      end
    end

    def parse(tokens)
      @body = new_body
      @fallback = nil

      while parse_body(current_body, tokens)
      end

      unless @fallback
        raise SyntaxError, parse_context.locale.t('errors.syntax.protect_missing_fallback')
      end

      @blank = @body.blank? && @fallback.blank?
      @body.freeze
      @fallback.freeze
    end

    def blank?
      @blank
    end

    def nodelist
      [@body, @fallback]
    end

    def render_to_output_buffer(context, output)
      protected_output = output.dup
      caught_error = nil

      context.with_local_scope do
        with_template_error_handling(context) do
          context.with_isolated_errors do
            @body.render_to_output_buffer(context, protected_output)
          end
        end
      rescue Liquid::MemoryError, Liquid::InternalError
        raise
      rescue Liquid::Error => error
        caught_error = error
      end

      if caught_error
        error_diagnostic = {
          'message' => safe_error_message(caught_error),
          'template_name' => caught_error.template_name,
          'line_number' => caught_error.line_number,
          'type' => error_type(caught_error),
        }.freeze

        context.with_local_scope do
          context['error'] = error_diagnostic
          @fallback.render_to_output_buffer(context, output)
        end
      else
        output.replace(protected_output)
      end

      output
    end

    private

    def current_body
      @fallback || @body
    end

    def unknown_tag(tag_name, markup, _tokens)
      return start_fallback(markup) if tag_name == FALLBACK_TAG

      super
    end

    def start_fallback(markup)
      unless markup.strip.empty?
        raise SyntaxError, parse_context.locale.t('errors.syntax.tag_unexpected_args', tag: FALLBACK_TAG)
      end
      if @fallback
        raise SyntaxError, parse_context.locale.t('errors.syntax.protect_multiple_fallback')
      end

      @fallback = new_body
    end

    def error_type(error)
      ERROR_TYPES.fetch(error.class.name&.sub(/\ALiquid::/, ''), 'liquid')
    end

    def safe_error_message(error)
      message = error.to_s(false)
      message.delete_suffix!(" #{error.markup_context}") if error.markup_context
      message
    end

    def with_template_error_handling(context)
      previous_exception_renderer = context.exception_renderer
      context.exception_renderer = Liquid::RAISE_EXCEPTION_LAMBDA
      yield
    ensure
      context.exception_renderer = previous_exception_renderer
    end
  end
end
