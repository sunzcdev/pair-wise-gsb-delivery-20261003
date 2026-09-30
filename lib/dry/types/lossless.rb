# frozen_string_literal: true

require "dry/types/coercions/lossless"

module Dry
  module Types
    # Lossless numeric coercions
    #
    # These types accept the same common inputs as `coercible.*`, `params.*`,
    # and `json.*` (Integer, Float, BigDecimal, and decimal strings) but they
    # only perform coercions that preserve the mathematical value. See
    # {Coercions::Lossless} for the precise rules.
    options = {namespace: "lossless"}

    {
      "integer" => :to_integer,
      "float" => :to_float,
      "decimal" => :to_decimal
    }.each do |name, method|
      register("lossless.#{name}") do
        Constructor::Lossless.new(
          self["nominal.#{name}"].with(**options),
          fn: Coercions::Lossless.method(method)
        )
      end
    end

    register("optional.lossless.integer") { self["nil"] | self["lossless.integer"] }
    register("optional.lossless.float") { self["nil"] | self["lossless.float"] }
    register("optional.lossless.decimal") { self["nil"] | self["lossless.decimal"] }
  end
end
