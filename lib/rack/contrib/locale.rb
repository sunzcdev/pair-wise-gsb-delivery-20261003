# frozen_string_literal: true

require 'i18n'

module Rack
  class Locale
    HEADERS_KLASS = Rack.release < "3" ? Utils::HeaderHash : Headers
    private_constant :HEADERS_KLASS

    # Pass +supported_locales:+ to enable full language tag negotiation.
    # Exact requested tags are tried first, then each shortened parent tag,
    # then wildcard ranges, and finally the configured default locale.
    # Supported tags are scoped to this middleware instance and do not change
    # I18n.available_locales.

    LanguageRange = Struct.new(:tag, :quality, :position) do
      def wildcard?
        tag == '*'
      end
    end

    class RequestConfig < I18n::Config
      attr_reader :available_locales, :available_locales_set,
        :default_locale, :enforce_available_locales

      def initialize(available_locales, default_locale)
        super()
        @available_locales = available_locales.map(&:to_sym)
        @available_locales_set = @available_locales.flat_map do |locale|
          [locale, locale.to_s]
        end
        @default_locale = default_locale.to_sym
        @enforce_available_locales = true
      end
    end
    private_constant :LanguageRange, :RequestConfig

    def initialize(app, supported_locales = nil, **options)
      @app = app

      if supported_locales.is_a?(Hash)
        options = supported_locales.merge(options)
        supported_locales = nil
      end

      configured_locales = supported_locales ||
        options[:locales] ||
        options[:available_locales] ||
        options[:languages] ||
        options[:supported_locales]

      if configured_locales
        @supported_locales = Array(configured_locales).map do |locale|
          locale.to_s.freeze
        end.uniq { |locale| locale.downcase }.freeze
      end

      return unless full_tag_negotiation?

      default_locale = options[:default_locale] ||
        options[:default_language] ||
        I18n.default_locale
      @default_locale = canonical_locale(default_locale.to_s)

      unless @default_locale
        raise ArgumentError, "default locale #{default_locale.inspect} is not supported"
      end
    end

    def call(env)
      locale_to_restore = I18n.locale
      return call_with_full_tag_negotiation(env) if full_tag_negotiation?

      locale = user_preferred_locale(env["HTTP_ACCEPT_LANGUAGE"])
      locale ||= I18n.default_locale

      env['rack.locale'] = I18n.locale = locale.to_s
      status, headers, body = @app.call(env)
      headers = HEADERS_KLASS.new.merge(headers)

      unless headers['Content-Language']
        headers['Content-Language'] = locale.to_s
      end

      [status, headers, body]
    ensure
      I18n.locale = locale_to_restore unless full_tag_negotiation?
    end

    private

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

    def call_with_full_tag_negotiation(env)
      locale = preferred_full_locale(env["HTTP_ACCEPT_LANGUAGE"])
      return not_acceptable unless locale

      config_to_restore = I18n.config
      I18n.config = RequestConfig.new(@supported_locales, @default_locale)
      I18n.locale = locale
      env['rack.locale'] = locale

      status, headers, body = @app.call(env)
      headers = HEADERS_KLASS.new.merge(headers)
      headers['Content-Language'] = locale unless headers['Content-Language']

      [status, headers, body]
    ensure
      I18n.config = config_to_restore if config_to_restore
    end

    def full_tag_negotiation?
      defined?(@supported_locales) && @supported_locales
    end

    def preferred_full_locale(header)
      ranges = parse_language_ranges(header)
      ranges.reject { |range| range.quality == 0 }.each do |range|
        candidate = candidate_for_range(range, ranges)
        return candidate if candidate
      end

      @default_locale unless rejected_locale?(@default_locale, ranges)
    end

    def candidate_for_range(range, ranges)
      if range.wildcard?
        return @supported_locales.find { |tag| !rejected_locale?(tag, ranges) }
      end

      candidate_groups_for_tag(range.tag).each do |tags|
        candidate = tags.find { |tag| !rejected_locale?(tag, ranges) }
        return candidate if candidate
      end

      nil
    end

    def candidate_groups_for_tag(tag)
      parts = tag.split('-')
      exact_groups = parts.length.downto(2).map do |length|
        variant = parts.first(length).join('-')
        @supported_locales.select { |locale| locale.casecmp(variant) == 0 }
      end.reject(&:empty?)

      primary_tag = parts.first
      primary_exact_group = @supported_locales.select { |locale| locale.casecmp(primary_tag) == 0 }
      primary_prefix_group = @supported_locales.select do |locale|
        locale.casecmp(primary_tag) != 0 && locale_has_prefix?(locale, primary_tag)
      end

      exact_groups + [primary_exact_group, primary_prefix_group].reject(&:empty?)
    end

    def rejected_locale?(locale, ranges)
      ranges.any? do |range|
        next false unless range.quality == 0

        if range.wildcard?
          !ranges.any? do |candidate|
            candidate.quality > 0 &&
              !candidate.wildcard? &&
              candidate.tag.split('-').first == locale.downcase.split('-').first
          end
        elsif range_covers?(range, locale)
          !ranges.any? do |candidate|
            candidate.quality > 0 &&
              !candidate.wildcard? &&
              range_covers?(candidate, locale) &&
              candidate.tag.split('-').length >= range.tag.split('-').length
          end
        end
      end
    end

    def range_covers?(range, locale)
      !range.wildcard? &&
        locale_has_prefix?(locale, range.tag)
    end

    def locale_has_prefix?(locale, prefix)
      locale_parts = locale.downcase.split('-')
      prefix_parts = prefix.downcase.split('-')
      locale_parts.first(prefix_parts.length) == prefix_parts
    end

    def parse_language_ranges(header)
      return [] if header.nil?

      header.split(',').each_with_index.map do |language_range, position|
        parts = language_range.split(';').map(&:strip)
        tag = parts.first.to_s
        next if tag.empty?

        quality = 1.0
        invalid_quality = parts[1..-1].any? do |parameter|
          name, value = parameter.split('=', 2).map { |part| part.to_s.strip }
          next false unless name && name.casecmp('q') == 0

          if value && value.match(/\A(?:0(?:\.\d{1,3})?|1(?:\.0{1,3})?)\z/)
            quality = value.to_f
            false
          else
            true
          end
        end
        next if invalid_quality

        LanguageRange.new(tag.downcase, quality, position)
      end.compact.sort_by do |range|
        [-range.quality, range.wildcard? ? 1 : 0, range.position]
      end
    end

    def canonical_locale(locale)
      @supported_locales.find { |supported_locale| supported_locale.casecmp(locale) == 0 }
    end

    def not_acceptable
      [406, { 'content-type' => 'text/plain' }, ['Not Acceptable']]
    end
  end
end
