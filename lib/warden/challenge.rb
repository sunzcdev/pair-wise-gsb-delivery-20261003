# encoding: utf-8
# frozen_string_literal: true

module Warden
  # A structured representation of a single HTTP authentication challenge
  # as described by RFC 7235:
  #
  #   challenge   = auth-scheme [ 1*SP ( token68 / [ auth-param *( OWS "," OWS auth-param ) ] ) ]
  #   auth-param  = token BWS "=" BWS ( token / quoted-string )
  #
  # Strategies declare challenges (see Warden::Strategies::Base#challenge!)
  # and the failure handling renders them into a WWW-Authenticate header.
  #
  # A challenge carries either a token68 or a list of auth-params, never
  # both. All data is validated when the challenge is built, so invalid
  # input is rejected long before a response is generated.
  class Challenge
    # token = 1*tchar
    TOKEN = /\A[!#$%&'*+\-.^_`|~0-9A-Za-z]+\z/.freeze

    # token68 = 1*( ALPHA / DIGIT / "-" / "." / "_" / "~" / "+" / "/" ) *"="
    TOKEN68 = /\A[A-Za-z0-9\-._~+\/]+=*\z/.freeze

    # Characters allowed inside a quoted-string without breaking its
    # boundaries: HTAB / SP / VCHAR / obs-text. Control characters such as
    # CR and LF are rejected so a challenge can never inject extra headers.
    SAFE_QDTEXT = /\A[^\x00-\x08\x0A-\x1F\x7F]*\z/.freeze

    attr_reader :scheme, :params, :token68

    # Parameters:
    #   scheme  - the authentication scheme (e.g. "Basic", :Bearer)
    #   params  - a Hash of auth-param name => value pairs
    #   token68 - a token68 credentials string (mutually exclusive with params)
    #
    # Raises ArgumentError when any piece of data cannot be represented as
    # a well-formed challenge.
    def initialize(scheme, params = {}, token68 = nil)
      @scheme  = self.class.validate_scheme(scheme)
      @token68 = self.class.validate_token68(token68)
      @params  = self.class.validate_params(params)

      if @token68 && !@params.empty?
        raise ArgumentError, "a challenge cannot carry both token68 and auth-params"
      end
    end

    # Renders the challenge as it should appear inside a WWW-Authenticate
    # header. Param values that are not valid tokens are emitted as
    # quoted-strings with '"' and '\' escaped, so quotes, backslashes and
    # commas inside a value never break the challenge boundaries.
    def to_s
      str = @scheme.dup
      if @token68
        str << " " << @token68
      elsif !@params.empty?
        str << " " << @params.map { |k, v| "#{k}=#{self.class.render_value(v)}" }.join(", ")
      end
      str
    end

    # :api: private
    def self.validate_scheme(scheme)
      str = scheme.to_s
      unless str.match?(TOKEN)
        raise ArgumentError, "invalid authentication scheme: #{scheme.inspect}"
      end
      str
    end

    # :api: private
    def self.validate_token68(token68)
      return nil if token68.nil?
      str = token68.to_s
      unless str.match?(TOKEN68)
        raise ArgumentError, "invalid token68: #{token68.inspect}"
      end
      str
    end

    # :api: private
    def self.validate_params(params)
      validated = {}
      params.each do |key, value|
        name = key.to_s
        unless name.match?(TOKEN)
          raise ArgumentError, "invalid auth-param name: #{key.inspect}"
        end
        str = value.to_s
        unless str.match?(SAFE_QDTEXT)
          raise ArgumentError, "invalid auth-param value for #{name}: #{value.inspect}"
        end
        validated[name] = str
      end
      validated
    end

    # Renders a single auth-param value either as a bare token or as a
    # quoted-string with the required escaping.
    # :api: private
    def self.render_value(value)
      return value if value.match?(TOKEN)
      '"' + value.gsub(/[\\"]/) { |c| "\\#{c}" } + '"'
    end
  end # Challenge
end # Warden
