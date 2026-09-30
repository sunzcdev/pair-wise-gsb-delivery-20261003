 # frozen_string_literal: true
module JMESPath
  # Returned by {JMESPath.search_with_sources}. The plain JMESPath result is
  # available as {#value} and the provenance of every position in it can be
  # looked up via {#locations}, {#location_at} or {#[]}.
  #
  #     result = JMESPath.search_with_sources('people[?age > `30`].name', data)
  #     result.value            #=> ["Jane"]
  #     result.locations        #=> {"" => ..., "/0" => ...}
  #     result.location_at(0).pointer #=> "/people/1/name"
  #
  # Keys for {#locations} and arguments to {#location_at} / {#[]} are
  # {https://www.rfc-editor.org/rfc/rfc6901 JSON Pointer} strings (the root is
  # `""`) or path components. Result-array indices are positions in the result
  # and never point back into the source array by themselves; provenance is
  # carried on the {SourceLocation}.
  class SourcedResult
    # @return [Object] The plain JMESPath result, identical in shape to
    #   what {JMESPath.search} returns.
    attr_reader :value

    # @return [Hash{String => SourceLocation}] Source information keyed by
    #   JSON Pointer into the *result*.
    attr_reader :locations

    # @api private
    def initialize(value, locations)
      @value = value
      @locations = locations
    end

    # @overload location_at(pointer)
    #   @param [String] pointer A JSON Pointer such as `"/items/0/name"`.
    # @overload location_at(*tokens)
    #   @param tokens [Array<String,Integer>] Path components, joined with
    #     JSON Pointer escaping.
    # @return [SourceLocation, nil]
    def location_at(*args)
      pointer =
        if args.size == 1 && args.first.is_a?(String)
          args.first
        else
          self.class.build_pointer(args)
        end
      @locations[pointer]
    end

    # Alias for {#location_at}.
    def [](*args)
      location_at(*args)
    end

    # @return [Boolean] `true` when every entry is a verbatim document value.
    def all_sourced?
      @locations.each_value.all?(&:sourced?)
    end

    # @api private
    def self.build_pointer(tokens)
      tokens.map do |token|
        "/#{token.to_s.gsub('~', '~0').gsub('/', '~1')}"
      end.join
    end
  end
end
