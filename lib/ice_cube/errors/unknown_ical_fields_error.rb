module IceCube
  # Raised when an RRULE contains fields IceCube does not understand and the
  # caller has not explicitly chosen to :ignore or :preserve them.
  class UnknownIcalFieldsError < ArgumentError
    attr_reader :fields

    def initialize(fields)
      @fields = fields
      type = (fields.length == 1) ? "type" : "types"
      super("Invalid rule validation #{type}: #{fields.join(", ")}")
    end
  end
end
