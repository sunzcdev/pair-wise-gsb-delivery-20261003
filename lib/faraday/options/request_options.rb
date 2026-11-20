# frozen_string_literal: true

module Faraday
  # @!parse
  #   # RequestOptions contains the configurable properties for a Faraday request.
  #   #
  #   # @!attribute params_encoder
  #   #   @return [Object] the params encoder used to serialize the request params
  #   #
  #   # @!attribute proxy
  #   #   @return [ProxyOptions] proxy options for this request
  #   #
  #   # @!attribute bind
  #   #   @return [Hash] the local host and port to bind the connection to
  #   #
  #   # @!attribute timeout
  #   #   @return [Integer] time limit for the entire request (in seconds)
  #   #
  #   # @!attribute open_timeout
  #   #   @return [Integer] time limit for just the connection phase (in seconds)
  #   #
  #   # @!attribute read_timeout
  #   #   @return [Integer] time limit for the first response byte received from
  #   #           the server (in seconds)
  #   #
  #   # @!attribute write_timeout
  #   #   @return [Integer] time limit for the client to send the request to the
  #   #           server (in seconds)
  #   #
  #   # @!attribute boundary
  #   #   @return [String] the boundary used for multipart request bodies
  #   #
  #   # @!attribute oauth
  #   #   @return [Hash] OAuth options used by the OAuth middleware
  #   #
  #   # @!attribute context
  #   #   @return [Hash] free-form storage carried alongside the request. Each
  #   #           request built from the same options owns a deep copy of it.
  #   #
  #   # @!attribute on_data
  #   #   @return [Proc] callback invoked with each chunk when streaming the response
  #   class RequestOptions < Options; end
  RequestOptions = Options.new(:params_encoder, :proxy, :bind,
                               :timeout, :open_timeout, :read_timeout,
                               :write_timeout, :boundary, :oauth,
                               :context, :on_data) do
    # Members that are read-only collaborators, intentionally shared by every
    # copy instead of being duplicated. `on_data` is a callback and
    # `params_encoder` is a class or a stateless encoder.
    const_set(:SHARED_MEMBERS, %i[params_encoder on_data].freeze)

    # Build request options from +value+, taking ownership of caller-provided
    # nested structures (hashes and arrays) instead of aliasing them.
    #
    # @param value [nil, false, Hash, OptionsLike, #to_hash]
    # @return [RequestOptions]
    def self.from(value)
      return new unless value
      return value.deep_dup if value.is_a?(self)

      new.update(value.respond_to?(:to_hash) ? value.to_hash : value).deep_dup
    end

    # @param key [Object]
    # @return [Boolean] true if +key+ is declared as a shared member
    def self.shared_member?(key)
      const_get(:SHARED_MEMBERS).include?(key.to_sym)
    end

    def []=(key, value)
      if key && key.to_sym == :proxy
        super(key, value ? ProxyOptions.from(value) : nil)
      else
        super
      end
    end

    # Create a copy whose nested structures (hashes, arrays and nested options)
    # belong to the copy alone, so changing one copy never changes another.
    # Members listed in +SHARED_MEMBERS+ are kept by reference.
    #
    # This is the copy rule applied when a request is built from a connection's
    # options: middleware customising one request's `context` cannot change
    # another request's options, nor the connection defaults they came from.
    #
    # @return [RequestOptions]
    def deep_dup
      copy = self.class.new
      members.each do |key|
        value = send(key)
        next if value.nil?

        # `[]=` applies the proxy coercion declared on this class.
        copy[key] = self.class.shared_member?(key) ? value : Utils.deep_dup(value)
      end
      copy
    end

    def stream_response?
      on_data.is_a?(Proc)
    end
  end
end
