# frozen_string_literal: true

require "addressable/uri"

module Addressable
  ##
  # A structured view of an RFC 6068 <code>mailto:</code> URI.
  #
  # Building a <code>mailto:</code> URI by concatenating the path and query
  # by hand is error-prone: characters inside a recipient (such as a comma)
  # can be mistaken for delimiters, and naive form-style decoding turns "+"
  # into a space. This class parses and generates <code>mailto:</code> URIs
  # structurally instead.
  #
  # Recipients keep their original order, and header fields keep both their
  # order and any duplicates (for example, repeated "cc" or custom fields).
  # Decoding never treats "+" as a space, and a percent-encoded comma
  # (%2C) inside a recipient does not split it into two recipients.
  #
  # @example
  #   mail = Addressable::MailTo.new(
  #     to: ["user+tag@example.com", "other@example.com"],
  #     headers: [["subject", "Hello"], ["body", "line1\nline2"]]
  #   )
  #   mail.to_s
  #   #=> "mailto:user+tag@example.com,other@example.com?subject=Hello&body=line1%0D%0Aline2"
  #
  #   mail = Addressable::MailTo.parse(
  #     "mailto:user+tag@example.com?subject=Hello+World"
  #   )
  #   mail.subject
  #   #=> "Hello+World"
  class MailTo
    ##
    # Raised when a string cannot be parsed as an RFC 6068 mailto URI, or
    # when a value cannot be represented in one.
    class InvalidMailToError < StandardError; end

    ##
    # Raised when a header field name is not a valid RFC 6068 hfname.
    class InvalidHeaderNameError < InvalidMailToError; end

    SCHEME = "mailto"

    # Header field names are restricted to printable ASCII token characters.
    # In particular they may not be empty and may not contain whitespace,
    # "%", "=", "?", "&", "#", or ":" — all of which would either be
    # impossible to distinguish from mailto syntax or require encoding that
    # RFC 6068 does not permit in an hfname.
    HEADER_NAME_PATTERN = /\A[A-Za-z0-9!#$&'*+\-.^_`{|}~]+\z/

    # Characters that may appear literally in a recipient. This covers the
    # addr-spec alphabet, including "+". Everything else — notably ",",
    # "%", "?", "#", whitespace, and non-ASCII characters — is percent
    # encoded so it cannot be mistaken for a delimiter.
    RECIPIENT_UNSAFE = /[^a-zA-Z0-9\-._~!$'*+@]/

    # Characters that may appear literally in a header value. "&" and "="
    # are excluded because they delimit hfields, and "+" is excluded so
    # that form-style decoders cannot mistake it for a space.
    HEADER_VALUE_UNSAFE = %r{[^a-zA-Z0-9\-._~!$'(*,;:@/?]}

    # Characters that must never appear unencoded anywhere in a mailto URI.
    INVALID_RAW_CHARACTERS = /[\x00-\x20<>"{}|\\^`]/

    ##
    # Parses an RFC 6068 <code>mailto:</code> URI into a structured
    # representation.
    #
    # @param [String, #to_str] uri The mailto URI to parse.
    #
    # @return [Addressable::MailTo] The parsed mailto URI.
    #
    # @raise [InvalidMailToError] If the string is not a valid mailto URI.
    # @raise [InvalidHeaderNameError] If a header field name is invalid.
    def self.parse(uri)
      begin
        uri = uri.to_str
      rescue NoMethodError, TypeError
        raise TypeError, "Can't convert #{uri.class} into String."
      end unless uri.is_a?(String)

      if uri =~ INVALID_RAW_CHARACTERS
        raise InvalidMailToError,
          "Invalid unencoded character in mailto URI: #{uri.inspect}"
      end

      match = /\Amailto:([^?#]*)(?:\?([^#]*))?\z/i.match(uri)
      unless match
        raise InvalidMailToError, "Not an RFC 6068 mailto URI: #{uri.inspect}"
      end

      path, query = match.captures
      recipients = []
      unless path.empty?
        path.split(",", -1).each do |recipient|
          if recipient.empty?
            raise InvalidMailToError,
              "Empty recipient in mailto URI: #{uri.inspect}"
          end
          recipients << decode_component(recipient, "recipient")
        end
      end

      headers = []
      if query
        query.split("&", -1).each do |field|
          name, separator, value = field.partition("=")
          if separator.empty?
            raise InvalidMailToError,
              "Header field missing '=' in mailto URI: #{field.inspect}"
          end
          headers << [
            validate_header_name!(decode_component(name, "header name")),
            decode_component(value, "header value")
          ]
        end
      end

      new(to: recipients, headers: headers)
    end

    ##
    # @param [Array<String>, String] to
    #   The recipients of the message, in order. A single String is treated
    #   as a one-element list.
    # @param [Array<Array(String, String)>, Hash] headers
    #   The header fields of the message as an ordered list of
    #   <code>[name, value]</code> pairs. A Hash may be used when duplicate
    #   field names are not needed; a Hash value that is an Array produces
    #   one field per element.
    def initialize(to: [], headers: [])
      @to = normalize_recipients(to)
      @headers = []
      normalize_header_pairs(headers).each do |name, value|
        add_header(name, value)
      end
    end

    ##
    # The recipients of the message, in order.
    #
    # @return [Array<String>] The recipient addresses.
    attr_reader :to

    ##
    # The header fields of the message as an ordered list of
    # <code>[name, value]</code> pairs. Duplicate field names are
    # preserved.
    #
    # @return [Array<Array(String, String)>] The header fields.
    attr_reader :headers

    ##
    # The first value of the named header field. Field names are
    # case-insensitive.
    #
    # @param [String] name The header field name.
    #
    # @return [String, nil] The field value, or nil if absent.
    def header(name)
      pair = @headers.find { |field_name, _| field_name.casecmp?(name) }
      pair && pair[1]
    end
    alias_method :[], :header

    ##
    # All values of the named header field, in order.
    #
    # @param [String] name The header field name.
    #
    # @return [Array<String>] The field values.
    def header_values(name)
      @headers.select { |field_name, _| field_name.casecmp?(name) }.
        map { |_, value| value }
    end

    ##
    # Replaces every field with the given name with a single field. The
    # other fields — including duplicates of other names — are left
    # untouched. Passing nil removes the field entirely.
    #
    # @param [String] name The header field name.
    # @param [String, nil] value The new field value.
    def set_header(name, value)
      validate_header_name!(name)
      @headers.delete_if { |field_name, _| field_name.casecmp?(name) }
      @headers << [name, validate_header_value!(name, value)] unless value.nil?
      value
    end
    alias_method :[]=, :set_header

    ##
    # Appends a header field without disturbing existing fields, allowing
    # duplicate field names.
    #
    # @param [String] name The header field name.
    # @param [String] value The field value.
    def add_header(name, value)
      validate_header_name!(name)
      @headers << [name, validate_header_value!(name, value)]
      value
    end

    ##
    # Removes every field with the given name.
    #
    # @param [String] name The header field name.
    def delete_header(name)
      @headers.delete_if { |field_name, _| field_name.casecmp?(name) }
    end

    ##
    # The "subject" header field.
    #
    # @return [String, nil] The subject.
    def subject
      header("subject")
    end

    ##
    # Sets the "subject" header field, replacing any existing subject.
    #
    # @param [String] value The new subject.
    def subject=(value)
      set_header("subject", value)
    end

    ##
    # The "body" header field.
    #
    # @return [String, nil] The body.
    def body
      header("body")
    end

    ##
    # Sets the "body" header field, replacing any existing body.
    #
    # @param [String] value The new body.
    def body=(value)
      set_header("body", value)
    end

    ##
    # The RFC 6068 mailto URI for this message.
    #
    # @return [String] The mailto URI.
    def to_s
      path = @to.map { |recipient| self.class.encode_recipient(recipient) }.
        join(",")
      return "#{SCHEME}:#{path}" if @headers.empty?

      query = @headers.map do |name, value|
        "#{name}=#{self.class.encode_header_value(value)}"
      end.join("&")
      "#{SCHEME}:#{path}?#{query}"
    end

    ##
    # The mailto URI as an Addressable::URI.
    #
    # @return [Addressable::URI] The mailto URI.
    def to_uri
      Addressable::URI.parse(to_s)
    end

    ##
    # @return [String] A human-readable representation.
    def inspect
      "#<#{self.class}:#{format('0x%x', object_id)} #{self}>"
    end

    def ==(other)
      other.is_a?(MailTo) && to == other.to && headers == other.headers
    end
    alias_method :eql?, :==

    def hash
      [self.class, @to, @headers].hash
    end

    ##
    # Percent-encodes a recipient address. A comma inside an address is
    # encoded as %2C so it cannot be mistaken for a recipient separator;
    # "+" is kept literally because it is a valid addr-spec character and
    # is never a space.
    #
    # @param [String] recipient The recipient address.
    #
    # @return [String] The encoded recipient.
    def self.encode_recipient(recipient)
      Addressable::URI.encode_component(recipient, RECIPIENT_UNSAFE)
    end

    ##
    # Percent-encodes a header field value. Line breaks are normalized to
    # CRLF and encoded as %0D%0A, as required by RFC 6068 for message
    # bodies.
    #
    # @param [String] value The header field value.
    #
    # @return [String] The encoded value.
    def self.encode_header_value(value)
      normalized = value.gsub(/\r\n|\r|\n/, "\r\n")
      Addressable::URI.encode_component(normalized, HEADER_VALUE_UNSAFE)
    end

    protected

    def initialize_copy(original)
      super
      @to = original.to.dup
      @headers = original.headers.map(&:dup)
    end

    private

    def normalize_recipients(to)
      recipients = to.is_a?(Array) ? to : [to]
      recipients.map do |recipient|
        begin
          recipient = recipient.to_str
        rescue NoMethodError, TypeError
          raise TypeError, "Can't convert #{recipient.class} into String."
        end unless recipient.is_a?(String)
        if recipient.empty?
          raise InvalidMailToError, "Recipient address must not be empty."
        end
        utf8_string(recipient, "recipient")
      end
    end

    def normalize_header_pairs(headers)
      if headers.is_a?(Hash)
        headers.flat_map do |name, value|
          value.is_a?(Array) ?
            value.map { |element| [name, element] } : [[name, value]]
        end
      else
        headers.map do |pair|
          unless pair.is_a?(Array) && pair.size == 2
            raise TypeError,
              "Expected [name, value] header pair, got #{pair.inspect}"
          end
          pair
        end
      end
    end

    def validate_header_name!(name)
      self.class.validate_header_name!(name)
    end

    def self.validate_header_name!(name)
      begin
        name = name.to_str
      rescue NoMethodError, TypeError
        raise TypeError, "Can't convert #{name.class} into String."
      end unless name.is_a?(String)
      unless name.match?(HEADER_NAME_PATTERN)
        raise InvalidHeaderNameError,
          "Invalid mailto header field name: #{name.inspect}"
      end
      name
    end

    def validate_header_value!(name, value)
      begin
        value = value.to_str
      rescue NoMethodError, TypeError
        raise TypeError,
          "Can't convert #{value.class} into String for header #{name.inspect}."
      end unless value.is_a?(String)
      utf8_string(value, "value of header #{name.inspect}")
    end

    # RFC 6068 requires percent-encoded octets to represent UTF-8, so
    # strings in other encodings are transcoded. Binary strings are
    # assumed to hold bytes that are already meaningful as-is.
    def utf8_string(string, description)
      return string.dup if string.encoding == Encoding::UTF_8 ||
        string.encoding == Encoding::ASCII_8BIT

      begin
        string.encode(Encoding::UTF_8)
      rescue EncodingError => error
        raise InvalidMailToError,
          "Cannot express #{description} as UTF-8: #{error.message}"
      end
    end

    def self.decode_component(component, description)
      if component =~ /%(?![0-9A-Fa-f]{2})/
        raise InvalidMailToError,
          "Invalid percent-encoding in #{description}: #{component.inspect}"
      end
      # Addressable::URI.unencode decodes percent-encoded sequences only;
      # unlike form decoding it never turns "+" into a space.
      Addressable::URI.unencode(component)
    end
  end
end
