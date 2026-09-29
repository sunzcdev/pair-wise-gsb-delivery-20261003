# frozen_string_literal: true

module I18n
  module Backend
    # Opt-in support for I18n::MessagePattern translations.
    #
    # Include this module into the Simple backend (or any backend that
    # includes I18n::Backend::Base):
    #
    #   I18n::Backend::Simple.include(I18n::Backend::MessagePattern)
    #
    # Plain translation strings keep being interpolated with the regular
    # "%{name}" syntax. Message pattern syntax only applies when explicitly
    # requested, in one of two ways:
    #
    # 1. Store an I18n::MessagePattern instance as translation data:
    #
    #      store_translations(:en, :inbox => I18n::MessagePattern.parse(
    #        "{count, plural, one{one message} other{# messages}}"))
    #      I18n.t(:inbox, :count => 2) # => "2 messages"
    #
    # 2. Pass +message_pattern: true+ to treat the looked-up String as a
    #    message pattern (parsed once, then cached):
    #
    #      store_translations(:en, :inbox => "{count, plural, one{one message} other{# messages}}")
    #      I18n.t(:inbox, :count => 2, :message_pattern => true) # => "2 messages"
    module MessagePattern
      def translate(locale, key, options = EMPTY_HASH)
        if options[:message_pattern]
          options = Utils.except(options, :message_pattern).merge(:skip_interpolation => true)
          entry = super(locale, key, options)
          values = Utils.except(options, *RESERVED_KEYS)
          case entry
          when ::String then I18n::MessagePattern.parse(entry).render(values)
          when I18n::MessagePattern then entry.render(values)
          else entry
          end
        else
          entry = super
          if entry.is_a?(I18n::MessagePattern)
            entry.render(options.empty? ? EMPTY_HASH : Utils.except(options, *RESERVED_KEYS))
          else
            entry
          end
        end
      end

      def interpolate(locale, subject, values = EMPTY_HASH)
        case subject
        when I18n::MessagePattern
          subject.render(values)
        when ::Array
          subject.map { |element| interpolate(locale, element, values) }
        else
          super
        end
      end

      def deep_interpolate(locale, data, values = EMPTY_HASH)
        case data
        when I18n::MessagePattern
          data.render(values)
        else
          super
        end
      end
    end
  end
end
