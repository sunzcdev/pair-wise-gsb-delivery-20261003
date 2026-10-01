# frozen_string_literal: true

module Blueprinter
  # @api private
  #
  # A RenderSnapshot pins the compiled view structure of every Blueprint used by a single top-level
  # render. A Blueprint is pinned the first time it is encountered (the root Blueprint is pinned when
  # the render starts) and the same pinned structure is reused for every later object and encounter.
  #
  # The pinned structure includes fields, field exclusions, included views, and transformer order,
  # so redefining a Blueprint while a collection is being rendered cannot cause earlier and later
  # objects to be serialized against different field sets. Definition changes made during the render
  # remain visible to the next top-level render.
  #
  # Snapshots are stored in thread-local storage, meaning concurrent renders never overwrite one
  # another's version selections, and an abnormal exit restores the previously active snapshot.
  class RenderSnapshot
    THREAD_KEY = :_blueprinter_render_snapshot

    # @return [RenderSnapshot, nil] the snapshot active for the current thread, if any
    def self.current
      Thread.current[THREAD_KEY]
    end

    # Activates a fresh snapshot for the duration of the block, restoring the previously active
    # snapshot (if any) when the block returns or raises.
    def self.wrap
      previous = current
      snapshot = new
      Thread.current[THREAD_KEY] = snapshot

      begin
        yield snapshot
      ensure
        Thread.current[THREAD_KEY] = previous
      end
    end

    # @param view_collection [Blueprinter::ViewCollection]
    # @return [Hash] the frozen compiled structure (fields and transformers for every view)
    def pin(view_collection)
      @pins ||= {}
      @pins[view_collection] ||= view_collection.compile_structure
    end
  end
end
