require 'flipper/ui/action'
require 'flipper/ui/decorators/feature'
require 'flipper/ui/util'
require 'flipper/adapters/actor_limit'

module Flipper
  module UI
    module Actions
      class ActorsGate < UI::Action
        include FeatureNameFromRoute

        route %r{\A/features/(?<feature_name>.*)/actors/?\Z}

        def get
          feature = flipper[feature_name]
          @feature = Decorators::Feature.new(feature)
          view_response :add_actor
        end

        def post
          render_read_only if read_only?

          feature = flipper[feature_name]
          value = params['value'].to_s.strip
          values = value.split(UI.configuration.actors_separator).map(&:strip).uniq

          if values.empty?
            redirect_to_form(feature, "#{value.inspect} is not a valid actor value.")
          end

          case params['operation']
          when 'replace'
            replace_actors(feature, values)
          else
            values.each do |value|
              actor = Flipper::Actor.new(value)

              case params['operation']
              when 'enable'
                feature.enable_actor actor
              when 'disable'
                feature.disable_actor actor
              end
            end
          end

          redirect_to("/features/#{Flipper::UI::Util.escape feature.key}")
        rescue Flipper::Adapters::ActorLimit::LimitExceeded => exception
          redirect_to_form(feature, exception.message)
        rescue Flipper::ActorSetConflict => exception
          redirect_to_form(feature, "#{exception.message} Nothing was changed; refresh and try again.")
        end

        private

        # Internal: Replaces the actors in the "from" field with the actors in
        # the "value" field as single set changes, one pair at a time.
        #
        # The members the page was rendered with come back in the "expected"
        # field and are handed to the adapter, so a stale page gets a conflict
        # telling the operator to refresh and retry instead of silently
        # overwriting the other admin's change. After each pair the expected
        # set is advanced by hand so the next pair is checked against what the
        # previous pair left behind.
        def replace_actors(feature, new_values)
          old_values = split_actors(params['from'])

          if old_values.empty? || old_values.size != new_values.size
            return redirect_to_form(feature, "Replacing actors needs the same number of actors to remove and to add.")
          end

          expected = if params.key?('expected')
                       split_actors(params['expected'])
                     else
                       feature.actors_value.to_a
                     end
          current = expected.to_set

          old_values.zip(new_values).each do |old_value, new_value|
            feature.replace_actor(
              Flipper::Actor.new(old_value),
              Flipper::Actor.new(new_value),
              expected: current,
            )
            current.delete(old_value)
            current.add(new_value)
          end
        end

        def split_actors(value)
          value.to_s.split(UI.configuration.actors_separator).map(&:strip).reject(&:empty?)
        end

        def redirect_to_form(feature, error)
          redirect_to("/features/#{Flipper::UI::Util.escape feature.key}/actors?error=#{Flipper::UI::Util.escape error}")
        end
      end
    end
  end
end
