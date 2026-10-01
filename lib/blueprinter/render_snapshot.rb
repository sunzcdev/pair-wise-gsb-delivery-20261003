# frozen_string_literal: true

module Blueprinter
  # @api private
  #
  # RenderSnapshot pins the compiled view definitions (fields, exclusions,
  # included views and transformer order) of every Blueprint used during a
  # single top-level render, so that redefining a view mid-render does not
  # change the output of the render already in progress.
  #
  # The root Blueprint is pinned when the render starts. Nested or dynamically
  # selected Blueprints are pinned the first time they are encountered; later
  # encounters within the same render reuse the pinned definitions. Pins are
  # released when the render finishes (or raises), so definition updates become
  # visible to the next top-level render.
  #
  # Each top-level render owns its own RenderSnapshot, so concurrent renders
  # never overwrite each other's pinned versions. Only the Blueprints' compiled
  # view definitions are pinned (they are already frozen); the objects being
  # rendered are never frozen or otherwise modified.
  class RenderSnapshot
    def initialize
      @pins = {}
    end

    # Pins the Blueprint's current view definitions and returns the pinned
    # snapshot. Repeated calls for the same Blueprint return the first pin.
    def pin(blueprint)
      @pins[blueprint] ||= blueprint.view_collection.snapshot
    end

    def view?(blueprint, view_name)
      pin(blueprint)[:fields].key?(view_name)
    end

    def fields_for(blueprint, view_name)
      pin(blueprint)[:fields][view_name]
    end

    def transformers_for(blueprint, view_name)
      pin(blueprint)[:transformers][view_name]
    end

    # Releases all pinned view definitions.
    def release!
      @pins.clear
    end
  end
end
