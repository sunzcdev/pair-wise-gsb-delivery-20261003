require "flipper/errors"

module Flipper
  module Expressions
    class FeatureEnabled
      EVALUATING_KEY = :flipper_evaluating_features

      CircularDependency = Flipper::CircularDependencyError
      CircularDependencyError = Flipper::CircularDependencyError
      CyclicDependency = Flipper::CircularDependencyError
      CyclicDependencyError = Flipper::CircularDependencyError
      DependencyCycle = Flipper::CircularDependencyError
      DependencyCycleError = Flipper::CircularDependencyError
      CycleError = Flipper::CircularDependencyError
      CycleDetected = Flipper::CircularDependencyError
      CycleDetectedError = Flipper::CircularDependencyError

      def self.call(feature_name, context:)
        feature_name = feature_name.to_s

        # Gates own the active-expression path. Checking it here gives the
        # reference expression itself a precise error while remaining lazy:
        # this code only runs for an operand All/Any actually evaluates.
        path = Thread.current[EVALUATING_KEY]
        if path&.include?(feature_name)
          raise Flipper::CircularDependencyError.new(Array(path) + [feature_name])
        end

        actor = context[:actor]
        if actor
          Flipper.enabled?(feature_name, actor)
        else
          Flipper.enabled?(feature_name)
        end
      end

      # Tracks only feature expressions that are actually being evaluated.
      # This keeps All/Any short-circuiting and earlier gates authoritative:
      # a reference in an unvisited branch never enters this path, while a
      # reference that returns to an active expression reports the real back edge.
      def self.with_feature(feature_name)
        feature_name = feature_name.to_s
        path = Thread.current[EVALUATING_KEY] ||= []

        if path.include?(feature_name)
          raise Flipper::CircularDependencyError.new(path + [feature_name])
        end

        path << feature_name
        begin
          yield
        ensure
          path.pop if path.last == feature_name
          Thread.current[EVALUATING_KEY] = nil if path.empty?
        end
      end
    end

    [
      :CircularDependency,
      :CircularDependencyError,
      :CyclicDependency,
      :CyclicDependencyError,
      :DependencyCycle,
      :DependencyCycleError,
      :CycleError
    ].each do |error_name|
      const_set(error_name, Flipper::CircularDependencyError) unless
        const_defined?(error_name, false)
    end
  end
end

if defined?(Flipper::Expression)
  %i[
    CircularDependency
    CircularDependencyError
    CyclicDependency
    CyclicDependencyError
    DependencyCycle
    DependencyCycleError
    CycleError
  ].each do |name|
    Flipper::Expression.const_set(name, Flipper::CircularDependencyError) unless
      Flipper::Expression.const_defined?(name, false)
  end
end
