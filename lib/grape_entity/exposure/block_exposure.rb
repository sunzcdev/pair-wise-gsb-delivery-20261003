# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class BlockExposure < Base
        attr_reader :block

        def value(entity, options)
          entity.exec_exposure_value_block(options, @block)
        end

        def dup
          super(&@block)
        end

        def ==(other)
          super && @block == other.block
        end

        def valid?(_entity)
          true
        end

        def setup(&block)
          @block = block
        end
      end
    end
  end
end
