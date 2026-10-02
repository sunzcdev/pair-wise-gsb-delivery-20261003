# frozen_string_literal: true

require 'i18n'
require 'rack'

module Rack
  # Selects the I18n locale from the Accept-Language request header.
  #
  # Without explicit locale options, the historical two-letter fallback
  # behavior remains enabled. Pass a list of configured locales to negotiate
  # complete language tags instead:
  #
  #   use Rack::Locale, %w(en zh-Hans zh-Hant), default_locale: 'en'
  #
  # Ranges are selected by quality, then by header order. A range is matched
  # exactly before its subtag prefixes. Wildcards and the default locale are
  # only considered afterward, and q=0 ranges cannot be selected by fallback.
  # When even the default locale is rejected, the middleware responds 406.
  #
  class Locale
    HEADERS_KLASS = Rack.release < "3" ? Utils::HeaderHash : Headers
    private_constant :HEADERS_KLASS

    LanguageRange = Struct.new(:tag, :quality, :position)
    private_constant :LanguageRange

    def initialize(app, locales = nil, default_locale: nil)
      @app = app
      @locales =
        if locales.is_a?(Array)
          locales.map(&:to_s)
        elsif locales
          [locales.to_s]
        end
      @default_locale = default_locale && default_locale.to_s

      if @locales
        @locale_map = {}
        @locales.each do |locale|
          @locale_map[language_parts(locale)] ||= locale
        end

        default = @default_locale || I18n.default_locale.to_s
        @default_locale = @locale_map.fetch(language_parts(default)) do
          raise ArgumentError, "default locale #{default.inspect} is not available"
        end
      end
    end

    def call(env)
      locale_to_restore = I18n.locale
      enforce_to_restore = I18n.enforce_available_locales
      locale = preferred_locale(env["HTTP_ACCEPT_LANGUAGE"])
      return not_acceptable unless locale

      I18n.enforce_available_locales = false if @locales
      env['rack.locale'] = I18n.locale = locale.to_s
      status, headers, body = @app.call(env)
      headers = HEADERS_KLASS.new.merge(headers)

      unless headers['Content-Language']
        headers['Content-Language'] = locale.to_s
      end

      [status, headers, body]
    ensure
      I18n.locale = locale_to_restore
      I18n.enforce_available_locales = enforce_to_restore if @locales
    end

    private

    def preferred_locale(header)
      if @locales
        FullLanguageNegotiation.new(@locale_map, @default_locale, header).locale
      else
        user_preferred_locale(header) || I18n.default_locale.to_s
      end
    end

    def not_acceptable
      body = "Not Acceptable\n"
      [
        406,
        {
          'content-type' => 'text/plain',
          'content-length' => body.bytesize.to_s,
        },
        [body]
      ]
    end

    def language_parts(locale)
      locale.to_s.tr('_', '-').downcase.split('-')
    end

    # Accept-Language header is covered mainly by RFC 7231
    # https://tools.ietf.org/html/rfc7231
    #
    # Related sections:
    #
    # * https://tools.ietf.org/html/rfc7231#section-5.3.1
    # * https://tools.ietf.org/html/rfc7231#section-5.3.5
    # * https://tools.ietf.org/html/rfc4647#section-3.4
    #
    # There is an obsolete RFC 2616 (https://tools.ietf.org/html/rfc2616)
    #
    # Edge cases:
    #
    # * Value can be a comma separated list with optional whitespaces:
    #   Accept-Language: da, en-gb;q=0.8, en;q=0.7
    #
    # * Quality value can contain optional whitespaces as well:
    #   Accept-Language: ru-UA, ru; q=0.8, uk; q=0.6, en-US; q=0.4, en; q=0.2
    #
    # * Quality prefix 'q=' can be in upper case (Q=)
    #
    # * Ignore case when match locale with I18n available locales
    #
    def user_preferred_locale(header)
      return if header.nil?

      locales = header.gsub(/\s+/, '').split(",").map do |language_tag|
        locale, quality = language_tag.split(/;q=/i)
        quality = quality ? quality.to_f : 1.0
        [locale, quality]
      end.reject do |(locale, quality)|
        locale == '*' || quality == 0
      end.sort_by do |(_, quality)|
        quality
      end.map(&:first)

      return if locales.empty?

      if I18n.enforce_available_locales
        locale = locales.reverse.find { |locale| I18n.available_locales.any? { |al| match?(al, locale) } }
        matched_locale = I18n.available_locales.find { |al| match?(al, locale) } if locale
        if !locale && !matched_locale
          matched_locale = locales.reverse.find { |locale| I18n.available_locales.any? { |al| variant_match?(al, locale) } }
          matched_locale = matched_locale[0,2].downcase if matched_locale
        end
        matched_locale
      else
        locales.last
      end
    end

    def match?(s1, s2)
      s1.to_s.casecmp(s2.to_s) == 0
    end

    def variant_match?(s1, s2)
      s1.to_s.casecmp(s2[0,2].to_s) == 0
    end

    class FullLanguageNegotiation
      QVALUE = /\Aq=([01](?:\.\d{1,3})?)\z/i
      private_constant :QVALUE

      def initialize(locale_map, default_locale, header)
        @locale_map = locale_map
        @default_locale = default_locale
        @ranges = parse(header)
      end

      def locale
        concrete_locale || wildcard_locale || default_locale
      end

      private

      def parse(header)
        return [] if header.nil?

        header.gsub(/\s+/, '').split(',').each_with_index.map do |language_range, position|
          tag, *parameters = language_range.split(';')
          next if tag.nil? || tag.empty?

          quality_parameter = parameters.find { |parameter| parameter =~ /\Aq=/i }
          quality = 1.0
          if quality_parameter
            match = quality_parameter.match(QVALUE)
            next unless match
            quality = match[1].to_f
            next unless quality.between?(0.0, 1.0)
          end

          LanguageRange.new(tag, quality, position)
        end.compact.sort_by { |range| [-range.quality, range.position] }
      end

      def accepted_ranges
        @ranges.reject { |range| range.quality == 0 }
      end

      def rejected_ranges
        @ranges.select { |range| range.quality == 0 }
      end

      def concrete_locale
        accepted_ranges.each do |range|
          next if range.tag == '*'

          parts = parts(range.tag)
          exact_locale = @locale_map[parts]
          if exact_locale && exact_match_allowed?(exact_locale, range)
            return exact_locale
          end

          (parts.length - 1).downto(1) do |length|
            locale = @locale_map[parts.first(length)]
            return locale if locale && fallback_allowed?(locale)
          end
        end
        nil
      end

      def wildcard_locale
        return unless accepted_ranges.any? { |range| range.tag == '*' }

        @locale_map.values.find { |locale| fallback_allowed?(locale) }
      end

      def default_locale
        @default_locale if fallback_allowed?(@default_locale)
      end

      def exact_match_allowed?(locale, range)
        locale_parts = parts(locale)
        range_parts = parts(range.tag)
        rejected_ranges.none? do |rejected_range|
          next false if rejected_range.tag == '*'

          rejected_parts = parts(rejected_range.tag)
          prefix?(locale_parts, rejected_parts) &&
            rejected_parts.length > range_parts.length
        end
      end

      def fallback_allowed?(locale)
        locale_parts = parts(locale)
        rejected_ranges.none? do |range|
          range.tag == '*' || prefix?(locale_parts, parts(range.tag))
        end
      end

      def prefix?(locale_parts, range_parts)
        range_parts.length <= locale_parts.length &&
          locale_parts.first(range_parts.length) == range_parts
      end

      def parts(tag)
        tag.to_s.tr('_', '-').downcase.split('-')
      end
    end
  end
end
