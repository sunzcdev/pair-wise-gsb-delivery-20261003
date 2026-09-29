# frozen_string_literal: true

module I18n
  module Backend
    # Enables I18n::MessagePattern translations for a backend. This module is
    # strictly opt-in: plain String translations keep using the regular
    # "%{...}" interpolation and are never parsed as message patterns.
    #
    #   I18n::Backend::Simple.include(I18n::Backend::MessagePattern)
    #
    #   I18n.backend.store_translations(:en, :inbox => I18n::MessagePattern.new(
    #     '{count, plural, =0{No messages} one{One message} other{# messages}}'
    #   ))
    #   I18n.t(:inbox, :count => 2) # => "2 messages"
    #
    # When I18n::Backend::Pluralization is also included, the locale specific
    # pluralization rule (stored at :'i18n.plural.rule') is used for plural
    # branches; otherwise plural branches fall back to English rules.
    module MessagePattern
      def translate(locale, key, options = EMPTY_HASH)
        result = super
        # Reached when interpolation was skipped because no values were given.
        result.is_a?(I18n::MessagePattern) ? format_message_pattern(locale, result, options) : result
      end

      protected

      def interpolate(locale, subject, values = EMPTY_HASH)
        case subject
        when I18n::MessagePattern
          format_message_pattern(locale, subject, values)
        else
          super
        end
      end

      def format_message_pattern(locale, pattern, values)
        pattern.format(values, pluralizer: message_pattern_pluralizer(locale))
      end

      def message_pattern_pluralizer(locale)
        pluralizer = respond_to?(:pluralizer, true) ? pluralizer(locale) : nil
        pluralizer if pluralizer.respond_to?(:call)
      end
    end
  end
end
