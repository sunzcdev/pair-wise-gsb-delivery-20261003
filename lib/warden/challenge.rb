# encoding: utf-8
# frozen_string_literal: true

module Warden
  # A structured representation of a single WWW-Authenticate challenge
  # as described by RFC 7235.
  #
  # A challenge is composed of an authentication scheme, optionally
  # followed by either a token68 or a list of auth-params:
  #
  #   Warden::Challenge.new('Basic', :realm => 'Restricted Area').to_s
  #   # => 'Basic realm="Restricted Area"'
  #
  #   Warden::Challenge.new('Bearer', 'abcDEF-._~+/==').to_s
  #   # => 'Bearer abcDEF-._~+/=='
  #
  #   Warden::Challenge.new('Bearer').to_s
  #   # => 'Bearer'
  #
  # Invalid schemes, token68 values or parameter names are rejected with
  # an ArgumentError as soon as the challenge is declared, i.e. before
  # any response is generated.
  class Challenge
    # token, as defined by RFC 7230 section 3.2.6
    TOKEN = /\A[!#$%&'*+\-.^_`|~0-9A-Za-z]+\z/.freeze

    # token68, as defined by RFC 7235 section 2.1
    TOKEN68 = /\A[A-Za-z0-9\-._~+\/]+=*\z/.freeze

    # Characters that may never appear in a header value
    INVALID_VALUE_CHARS = /[\x00-\x1f\x7f]/.freeze

    attr_reader :scheme, :token68, :params

    # Parameters:
    #   scheme <String, Symbol> - the authentication scheme (a token)
    #   params <nil, Hash, String> - either nothing, a hash of
    #     auth-params, or a token68 string
    #
    # :api: public
    def initialize(scheme, params = nil)
      @scheme = validate_token(scheme, 'scheme')

      case params
      when nil
        @params = {}
      when Hash
        @params = {}
        params.each do |name, value|
          @params[validate_token(name, 'parameter name')] = validate_value(value)
        end
      when String, Symbol
        @token68 = validate_token68(params)
        @params = {}
      else
        raise ArgumentError, "Invalid challenge parameters: #{params.inspect}"
      end
    end

    # Serializes the challenge into its HTTP header representation.
    # Parameter values that are not valid tokens are emitted as
    # quoted-strings with double quotes and backslashes escaped, so
    # commas inside a value cannot break the challenge boundaries.
    #
    # :api: public
    def to_s
      parts = [scheme]
      if token68
        parts << token68
      elsif !params.empty?
        parts << params.map { |name, value| "#{name}=#{serialize_value(value)}" }.join(', ')
      end
      parts.join(' ')
    end

    def ==(other)
      other.is_a?(Challenge) && other.to_s == to_s
    end

    def inspect
      "#<#{self.class.name} #{to_s.inspect}>"
    end

  private

    def validate_token(value, description)
      string = value.to_s
      unless string =~ TOKEN
        raise ArgumentError, "Invalid challenge #{description}: #{value.inspect}"
      end
      string
    end

    def validate_token68(value)
      string = value.to_s
      unless string =~ TOKEN68
        raise ArgumentError, "Invalid challenge token68: #{value.inspect}"
      end
      string
    end

    def validate_value(value)
      string = value.to_s
      if string =~ INVALID_VALUE_CHARS
        raise ArgumentError, "Invalid character in challenge parameter value: #{value.inspect}"
      end
      string
    end

    def serialize_value(value)
      return value if value =~ TOKEN
      %("#{value.gsub(/[\\"]/) { |char| "\\#{char}" }}")
    end
  end # Challenge
end # Warden
