# frozen_string_literal: true

require "dry/types/coercions/lossless"

module Dry
  module Types
    # Built-in lossless numeric types.
    #
    # These types never trade away the mathematical value of the input:
    #
    #   Dry::Types["lossless.integer"].("3.7") # raises CoercionError
    #   Dry::Types["lossless.float"].("3.7")   # raises CoercionError
    #   Dry::Types["lossless.float"].("3.5")   # => 3.5
    #   Dry::Types["lossless.decimal"].("3.7") # => 0.37e1
    #
    # See {Dry::Types::Coercions::Lossless} for the full set of rules.
    #
    # @api public

    options = {namespace: "lossless"}

    {
      "integer" => :to_int,
      "float" => :to_float,
      "decimal" => :to_decimal
    }.each do |name, method|
      register("lossless.#{name}") do
        self["nominal.#{name}"].with(**options).constructor(
          Coercions::Lossless.method(method)
        )
      end
    end

    %w[integer float decimal].each do |name|
      register("optional.lossless.#{name}", self["lossless.#{name}"].optional)
    end
  end
end
