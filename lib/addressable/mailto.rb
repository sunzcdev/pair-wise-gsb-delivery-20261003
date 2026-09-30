# frozen_string_literal: true

require "addressable/uri"

module Addressable
  ##
  # This is an implementation of the `mailto` URI scheme based on
  # {RFC 6068}[https://www.rfc-editor.org/rfc/rfc6068].
  #
  # Unlike a generic parsing of `mailto:` as an opaque path plus query,
  # this class provides a structured view of the comma-separated recipients
  # and the header fields. Recipient order and repeated header fields are
  # preserved, `+` characters are never treated as spaces, percent-encoded
  # commas do not delimit a second recipient, and newlines in field values
  # (such as `body`) are serialized as CRLF sequences.
  #
  # @example
  #   mailto = Addressable::Mailto.new(
  #     :to => ["user+tag@example.com"],
  #     :headers => [["subject", "hello"], ["body", "line1\nline2"]]
  #   )
  #   mailto.to_s
  #   #=> "mailto:user+tag@example.com?subject=hello&body=line1%0D%0Aline2"
  class Mailto
    ##
    # Raised when a value cannot be represented as a `mailto` URI, for
    # example an invalid field name, a stray percent sign, or bytes that
    # cannot be interpreted as UTF-8.
    class InvalidMailtoError < StandardError
    end

    ##
    # Characters that may appear unencoded within an `addr-spec`.
    # @see CharacterClasses
    ADDR_SAFE =
      Addressable::URI::CharacterClasses::UNRESERVED +
      "\\!\\$\\'\\(\\)\\*\\+\\;\\=\\:\\@"

    ##
    # Characters that may appear unencoded within a header field value.
    # The `&` and `=` delimiters are intentionally excluded so that they
    # cannot be confused with the query structure.
    # @see CharacterClasses
    FIELD_VALUE_SAFE =
      Addressable::URI::CharacterClasses::UNRESERVED +
      "\\!\\$\\'\\(\\)\\*\\+\\,\\;\\=\\:\\@\\/\\?"

    ##
    # Characters that may appear unencoded within a header field name.
    # The `&` and `=` delimiters are excluded so a field name can never
    # be confused with the query structure, even when it contains other
    # characters permitted by RFC 5322 `ftext`.
    FIELD_NAME_SAFE =
      Addressable::URI::CharacterClasses::UNRESERVED +
      "\\!\\$\\'\\(\\)\\*\\+\\,\\;\\:\\@\\/\\?"

    ADDR_REGEXP = /[^#{ADDR_SAFE}]/.freeze
    FIELD_VALUE_REGEXP = /[^#{FIELD_VALUE_SAFE}]/.freeze
    FIELD_NAME_REGEXP_ENCODE = /[^#{FIELD_NAME_SAFE}]/.freeze

    ##
    # A field name as defined by RFC 5322: printable ASCII characters
    # excluding the space and the colon.
    FIELD_NAME_REGEXP = /\A[\x21-\x39\x3b-\x7e]+\z/.freeze

    ##
    # A single percent-encoded octet or a literal percent sign that does
    # not begin a valid escape sequence.
    PERCENT_ESCAPE = /%[0-9a-fA-F]{2}|%/

    ##
    # Creates a new `mailto` URI from structured components.
    #
    # @option [String, Array<String>] :to
    #   One or more recipients. The order is preserved.
    # @option [Array<Array(String, String)>, Hash] :headers
    #   Header field name/value pairs. An array of pairs preserves order
    #   and repeated field names. A `to` field is treated as additional
    #   recipients.
    #
    # @return [Addressable::Mailto] The constructed object.
    def initialize(options = {})
      @to = []
      @headers = []

      add_recipients(options[:to]) if options.key?(:to)

      headers = options[:headers]
      if headers
        if headers.respond_to?(:to_hash)
          headers.to_hash.each do |name, value|
            add_header(name, value)
          end
        elsif headers.respond_to?(:to_ary)
          headers.to_ary.each do |pair|
            name, value = pair.to_ary
            add_header(name, value)
          end
        else
          raise TypeError,
            "Can't convert #{headers.class} into header fields."
        end
      end
    end

    ##
    # Constructs a `mailto` URI from structured components.
    #
    # @param (see #initialize)
    # @return [Addressable::Mailto] The constructed object.
    def self.build(options = {})
      new(options)
    end

    ##
    # Parses a `mailto` URI into a structured representation.
    #
    # @param [String, Addressable::URI, #to_str] uri
    #   The URI string to parse. An existing {Addressable::Mailto} is
    #   duplicated.
    #
    # @return [Addressable::Mailto] The parsed object.
    def self.parse(uri)
      return uri.dup if uri.kind_of?(self)

      begin
        uri = uri.to_str
      rescue NoMethodError, TypeError
        raise TypeError, "Can't convert #{uri.class} into String."
      end unless uri.is_a?(String)

      parsed = Addressable::URI.parse(uri)
      unless parsed.scheme && parsed.scheme.downcase == "mailto"
        raise InvalidMailtoError,
          "Not a mailto URI: '#{uri}'"
      end
      if parsed.fragment
        raise InvalidMailtoError,
          "mailto URIs must not contain a fragment: '#{uri}'"
      end

      mailto = new

      if parsed.path && !parsed.path.empty?
        split_recipients(parsed.path).each do |address|
          mailto.add_recipient(decode_component(address))
        end
      end

      if parsed.query
        parsed.query.split("&").each do |pair|
          next if pair.empty?
          name, value = pair.split("=", 2)
          raw_name = name.to_s
          name = decode_component(raw_name)
          validate_field_name!(name)
          if name.downcase == "to"
            raw_value = value.nil? ? "" : value
            split_recipients(raw_value).each do |address|
              mailto.add_recipient(decode_component(address))
            end
          else
            value = value.nil? ? "" : decode_component(value)
            mailto.add_header(name, value)
          end
        end
      end

      mailto
    end

    ##
    # @return [Array<String>] The recipients in order.
    attr_reader :to

    ##
    # @return [Array<Array(String, String)>]
    #   The header fields as ordered name/value pairs. Repeated fields are
    #   preserved.
    attr_reader :headers

    ##
    # @return [Array<String>] The recipients in order.
    alias_method :recipients, :to

    ##
    # Appends one or more recipients.
    #
    # @param [String, Array<String>] recipients
    # @return [void]
    def add_recipients(recipients)
      if recipients.kind_of?(Array)
        recipients.each { |address| add_recipient(address) }
      else
        add_recipient(recipients)
      end
    end

    ##
    # Appends a single recipient.
    #
    # @param [#to_str] address The recipient address.
    # @return [void]
    def add_recipient(address)
      @to << stringify(address, "recipient")
    end

    ##
    # Appends a header field. A `to` field is treated as additional
    # recipients.
    #
    # @param [#to_str] name The field name.
    # @param [#to_str] value The field value.
    # @return [void]
    def add_header(name, value)
      name = stringify(name, "field name")
      value = value.nil? ? "" : stringify(value, "field value")
      validate_field_name!(name)
      if name.downcase == "to"
        add_recipients(value)
      else
        @headers << [name, value]
      end
    end

    ##
    # Returns the value of the first header field with the given name.
    # Field names are matched case-insensitively.
    #
    # @param [#to_str] name The field name.
    # @return [String, nil] The field value, or nil if absent.
    def header(name)
      name = stringify(name, "field name")
      pair = @headers.find { |field_name, _| field_name.downcase == name.downcase }
      pair && pair[1]
    end

    ##
    # @return [String, nil] The value of the `subject` field.
    def subject
      header("subject")
    end

    ##
    # Replaces the `subject` field, preserving the position of an existing
    # field. No other field is rewritten.
    #
    # @param [#to_str, nil] value The new subject, or nil to remove it.
    # @return [void]
    def subject=(value)
      replace_header("subject", value)
    end

    ##
    # @return [String, nil] The value of the `body` field.
    def body
      header("body")
    end

    ##
    # Replaces the `body` field.
    #
    # @param [#to_str, nil] value The new body, or nil to remove it.
    # @return [void]
    def body=(value)
      replace_header("body", value)
    end

    ##
    # @return [Addressable::URI] The structured data as a generic URI.
    def to_uri
      path = @to.map do |address|
        Addressable::URI.encode_component(address, ADDR_REGEXP)
      end.join(",")

      query = @headers.map do |name, value|
        encoded_name = Addressable::URI.encode_component(
          name, FIELD_NAME_REGEXP_ENCODE
        )
        encoded_value = Addressable::URI.encode_component(
          canonicalize_newlines(value), FIELD_VALUE_REGEXP
        )
        "#{encoded_name}=#{encoded_value}"
      end.join("&")

      Addressable::URI.new(
        :scheme => "mailto",
        :path => path,
        :query => query.empty? ? nil : query
      )
    end

    ##
    # @return [String] The serialized `mailto` URI.
    def to_s
      to_uri.to_s
    end
    alias_method :to_str, :to_s

    ##
    # @param [Object] other The object to compare.
    # @return [Boolean]
    #   Whether the other object is an equivalent `mailto` URI.
    def ==(other)
      other.kind_of?(self.class) && to_s == other.to_s
    end
    alias_method :eql?, :==

    ##
    # @return [Integer] A hash value consistent with {#eql?}.
    def hash
      self.class.hash ^ to_s.hash
    end

    ##
    # @return [Addressable::Mailto] A duplicate of the object.
    def dup
      copy = self.class.new
      @to.each { |address| copy.add_recipient(address) }
      @headers.each { |name, value| copy.add_header(name, value) }
      copy
    end

    ##
    # Freezes the object and its internal recipient and header arrays.
    #
    # @return [Addressable::Mailto] The frozen object.
    def freeze
      @to.freeze
      @headers.freeze
      super
    end

    ##
    # @return [String] A developer-friendly representation.
    def inspect
      "#<#{self.class} #{to_s}>"
    end

    private

    ##
    # Validates a decoded header field name.
    #
    # @param [String] name The field name.
    # @return [void]
    def validate_field_name!(name)
      self.class.send(:validate_field_name!, name)
    end

    ##
    # Replaces the first field matching `name`, appending a new field if
    # none exists. A nil value removes all matching fields.
    #
    # @param [String] name The field name.
    # @param [#to_str, nil] value The new value.
    # @return [void]
    def replace_header(name, value)
      @headers.reject! { |field_name, _| field_name.downcase == name }
      add_header(name, value) unless value.nil?
    end

    ##
    # Converts newlines into the CRLF sequences required by RFC 6068.
    #
    # @param [String] value The field value.
    # @return [String] The value with canonical line endings.
    def canonicalize_newlines(value)
      value.gsub(/\r\n|\r|\n/, "\r\n")
    end

    ##
    # Converts an object to a frozen-independent String.
    #
    # @param [Object] value The value to convert.
    # @param [String] label A label for error messages.
    # @return [String] The converted value.
    def stringify(value, label)
      value = value.to_str
      unless value.encoding == Encoding::UTF_8 || value.ascii_only?
        value = value.encode(Encoding::UTF_8)
      end
      value
    rescue NoMethodError, TypeError
      raise TypeError, "Can't convert #{value.class} into #{label}."
    rescue Encoding::UndefinedConversionError => error
      raise InvalidMailtoError,
        "Unrepresentable mailto #{label}: #{error.message}"
    end

    class << self
      private

      ##
      # Validates a decoded header field name.
      #
      # @param [String] name The field name.
      # @return [void]
      def validate_field_name!(name)
        unless name.ascii_only? && name =~ FIELD_NAME_REGEXP
          raise InvalidMailtoError,
            "Invalid mailto header field name: #{name.inspect}"
        end
      end

      ##
      # Splits a percent-encoded recipient list on unencoded commas.
      # Encoded commas (`%2C`) remain part of the address.
      #
      # @param [String] encoded The encoded list.
      # @return [Array<String>] The encoded recipient parts.
      def split_recipients(encoded)
        # `%2C` contains no literal comma, so a plain split is sufficient.
        encoded.split(",")
      end

      ##
      # Percent-decodes a component without treating `+` as a space.
      #
      # @param [String] component The encoded component.
      # @return [String] The decoded component.
      def decode_component(component)
        decoded = component.gsub(PERCENT_ESCAPE) do |match|
          if match == "%"
            raise InvalidMailtoError,
              "Invalid percent encoding in mailto URI: #{component.inspect}"
          end
          match[1..2].to_i(16).chr
        end
        decoded.force_encoding(Encoding::UTF_8)
        unless decoded.valid_encoding?
          raise InvalidMailtoError,
            "mailto component is not valid UTF-8: #{component.inspect}"
        end
        decoded
      end
    end
  end
end
