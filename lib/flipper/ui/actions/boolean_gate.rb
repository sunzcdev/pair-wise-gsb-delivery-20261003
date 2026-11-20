require 'flipper/ui/action'
require 'flipper/ui/decorators/feature'

module Flipper
  module UI
    module Actions
      class BooleanGate < UI::Action
        include FeatureNameFromRoute

        route %r{\A/features/(?<feature_name>.*)/boolean/?\Z}

        def post
          render_read_only if read_only?

          feature = flipper[feature_name]
          @feature = Decorators::Feature.new(feature)

          if params['action'] == 'Enable'
            if Flipper::UI.configuration.disable_fully_enable
              status 403
              halt view_response(:disable_fully_enable)
            end
            feature.enable
          else
            feature.disable
          end

          redirect_to "/features/#{Flipper::UI::Util.escape @feature.key}"
        end
      end
    end
  end
end
