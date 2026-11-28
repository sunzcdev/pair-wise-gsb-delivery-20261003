require 'flipper/api/action'
require 'flipper/api/v1/decorators/feature'
require 'flipper/adapters/actor_limit'

module Flipper
  module Api
    module V1
      module Actions
        class ActorsGate < Api::Action
          include FeatureNameFromRoute

          route %r{\A/features/(?<feature_name>.*)/actors/?\Z}

          def post
            ensure_valid_params
            feature = flipper[feature_name]
            actor = Actor.new(flipper_id)
            feature.enable_actor(actor)
            decorated_feature = Decorators::Feature.new(feature)
            json_response(decorated_feature.as_json, 200)
          end

          def delete
            ensure_valid_params
            feature = flipper[feature_name]
            actor = Actor.new(flipper_id)
            feature.disable_actor(actor)
            decorated_feature = Decorators::Feature.new(feature)
            json_response(decorated_feature.as_json, 200)
          end

          # Public: Replaces one actor with another in a single set change.
          #
          # PUT with flipper_id (the actor to enable) and replace_flipper_id
          # (the actor to disable in its place). Succeeds even when the actor
          # set is at its limit, because the size does not grow. The optional
          # expected member list, sent as expected[] entries or a
          # separator-joined expected string, makes the change a
          # compare-and-set: when the members have changed since the caller
          # read them, nothing is written and the request is rejected so the
          # caller can re-read and retry.
          #
          # Returns 200 with the feature, 409 when the members changed since the
          # caller read them (another admin won; re-read and retry), or 422
          # when the change would exceed the actor limit.
          def put
            ensure_valid_params
            ensure_valid_replace_params
            feature = flipper[feature_name]

            begin
              feature.replace_actor(
                Actor.new(replace_flipper_id),
                Actor.new(flipper_id),
                expected: expected_members,
              )
            rescue Flipper::ActorSetConflict
              json_error_response(:actor_set_conflict)
            rescue Flipper::Adapters::ActorLimit::LimitExceeded
              json_error_response(:actor_limit_exceeded)
            end

            decorated_feature = Decorators::Feature.new(feature)
            json_response(decorated_feature.as_json, 200)
          end

          private

          def ensure_valid_params
            json_error_response(:flipper_id_invalid) if flipper_id.nil?
          end

          def ensure_valid_replace_params
            json_error_response(:replace_flipper_id_invalid) if replace_flipper_id.nil?
          end

          def flipper_id
            @flipper_id ||= string_param('flipper_id')
          end

          def replace_flipper_id
            @replace_flipper_id ||= string_param('replace_flipper_id')
          end

          # Private: The optional member list the caller observed, as
          # expected[] entries or a separator-joined expected string.
          #
          # Returns an Array of String members, or nil when the parameter is
          # absent (no compare-and-set).
          def expected_members
            return @expected_members if defined?(@expected_members)

            raw = safe_params['expected']
            @expected_members =
              case raw
              when nil
                nil
              when Array
                json_error_response(:request_invalid) unless raw.all? { |value| valid_param_string?(value) }
                raw.map(&:to_s).reject(&:empty?)
              when String
                raw.split(',').map(&:strip).reject(&:empty?)
              else
                json_error_response(:request_invalid)
              end
          end
        end
      end
    end
  end
end
