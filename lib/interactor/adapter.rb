module Interactor
  # Public: Adapts an Interactor for use within an Interactor::Organizer when
  # the interactor's context keys do not match the keys used by the parent
  # organizer. The adapter copies declared inputs from the parent context into
  # a local context, invokes the interactor against that local context and,
  # on success, copies declared outputs back into the parent context. Keys
  # that are not declared in either mapping never cross the boundary in either
  # direction.
  #
  # Adapters are usually declared with Interactor::Organizer's "adapt" class
  # method rather than being instantiated directly.
  #
  # Examples
  #
  #   class MyOrganizer
  #     include Interactor::Organizer
  #
  #     adapt ReceivePayment,
  #       inputs: {payer: :account},
  #       outputs: {receipt: :payment_receipt}
  #   end
  class Adapter
    # Internal: Gets the Interactor class invoked by the Interactor::Adapter.
    attr_reader :interactor_class

    # Internal: Gets the Hash mapping the interactor's local input keys to the
    # parent context keys they are read from.
    attr_reader :inputs

    # Internal: Gets the Hash mapping the interactor's local output keys to
    # the parent context keys they are written to.
    attr_reader :outputs

    # Internal: Initialize an Interactor::Adapter.
    #
    # interactor_class - An Interactor class to be invoked against a local
    #                    context.
    # inputs           - A Hash mapping each local context key expected by the
    #                    interactor to the parent context key providing its
    #                    value. (default: {})
    # outputs          - A Hash mapping each local context key produced by the
    #                    interactor to the parent context key receiving its
    #                    value. (default: {})
    #
    # Raises Interactor::DuplicateOutputError if more than one output maps to
    #   the same parent context key.
    def initialize(interactor_class, inputs: {}, outputs: {})
      @interactor_class = interactor_class
      @inputs = normalize(inputs)
      @outputs = normalize(outputs)
      validate_outputs!
    end

    # Internal: Invoke the adapted Interactor against a local context derived
    # from the given parent context. This method mirrors the "call!" interface
    # of an Interactor class so that adapters can be organized alongside
    # ordinary interactors.
    #
    # parent_context - The Interactor::Context of the organizing interactor.
    #
    # Raises Interactor::MissingInputError if a declared input key is absent
    #   from the parent context.
    # Raises Interactor::MissingOutputError if a declared output key is absent
    #   from the interactor's local context after invocation. All outputs are
    #   verified before any of them is merged into the parent context.
    # Raises Interactor::Failure if the interactor fails its local context.
    #   The parent context is failed in turn, preserving the organizer's
    #   failure and rollback semantics.
    #
    # Returns nothing.
    def call!(parent_context)
      local_context = build_local_context(parent_context)
      instance = interactor_class.new(local_context)

      begin
        instance.run!
      rescue Failure
        parent_context.fail!
      end

      parent_context.called!(instance)
      merge_outputs!(local_context, parent_context)
    end

    private

    # Internal: Normalize a mapping's keys and values to Symbols.
    #
    # Returns a Hash.
    def normalize(mapping)
      mapping.each_with_object({}) do |(local_key, parent_key), normalized|
        normalized[local_key.to_sym] = parent_key.to_sym
      end
    end

    # Internal: Ensure that no two declared outputs write to the same parent
    # context key.
    #
    # Raises Interactor::DuplicateOutputError if a parent context key is the
    #   target of more than one output mapping.
    def validate_outputs!
      duplicates = outputs.values.tally.select { |_, count| count > 1 }.keys
      return if duplicates.empty?

      raise DuplicateOutputError,
        "Duplicate output mapping to #{duplicates.map { |key| "`#{key}`" }.join(", ")} " \
        "for #{interactor_class}"
    end

    # Internal: Build a local context for the interactor by copying declared
    # inputs from the parent context.
    #
    # Raises Interactor::MissingInputError if a declared input key is absent
    #   from the parent context. A key present with a nil value is copied as
    #   nil and does not raise.
    #
    # Returns an Interactor::Context.
    def build_local_context(parent_context)
      local_context = Context.new
      inputs.each do |local_key, parent_key|
        unless parent_context.to_h.key?(parent_key)
          raise MissingInputError,
            "Missing input `#{parent_key}` for #{interactor_class} (mapped to `#{local_key}`)"
        end
        local_context[local_key] = parent_context[parent_key]
      end
      local_context
    end

    # Internal: Copy declared outputs from the interactor's local context back
    # into the parent context. Every declared output is verified before any
    # value is merged, so a missing output never leaves the parent context
    # partially written.
    #
    # Raises Interactor::MissingOutputError if a declared output key is absent
    #   from the local context. A key present with a nil value is merged as
    #   nil and does not raise.
    #
    # Returns nothing.
    def merge_outputs!(local_context, parent_context)
      outputs.each do |local_key, parent_key|
        unless local_context.to_h.key?(local_key)
          raise MissingOutputError,
            "Missing output `#{local_key}` from #{interactor_class} (mapped to `#{parent_key}`)"
        end
      end
      outputs.each do |local_key, parent_key|
        parent_context[parent_key] = local_context[local_key]
      end
    end
  end
end
