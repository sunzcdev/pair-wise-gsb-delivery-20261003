module MultiJson
  # Raised when a Ruby object cannot be serialized according to
  # RFC 8785 (JSON Canonicalization Scheme) without silently losing
  # information or changing the represented value.
  class CanonicalizationError < StandardError
  end
end
