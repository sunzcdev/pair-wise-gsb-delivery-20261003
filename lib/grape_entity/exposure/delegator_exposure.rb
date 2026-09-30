# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class DelegatorExposure < Base
        def value(entity, options)
          entity.consistency_read(attribute, reread?, options)
        end
      end
    end
  end
end
