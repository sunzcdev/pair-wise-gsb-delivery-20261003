module Interactor
  # Public: Raised before an adapted Interactor is invoked when one or more
  # keys declared in the input mapping are absent from the parent Context.
  class MissingInput < ArgumentError
  end

  # Public: Raised after an adapted Interactor succeeds when one or more keys
  # declared in the output mapping are absent from its local Context.
  class MissingOutput < ArgumentError
  end

  # Internal: An Interactor that invokes another Interactor against an
  # isolated Context. Parent Context keys are translated into local keys
  # before invocation and local keys are translated back into the parent
  # Context only after every declared output has been verified.
  module Adapter
    # Internal: Build an Interactor class adapted to the given mappings.
    #
    # interactor - The Interactor class to invoke.
    # inputs     - A Hash mapping local input keys to parent Context keys.
    # outputs    - A Hash mapping local output keys to parent Context keys.
    #
    # Returns an Interactor class suitable for #organize.
    def self.build(interactor, inputs, outputs)
      input_mapping = normalize(inputs)
      output_mapping = normalize(outputs)

      Class.new do
        include Interactor
        include Interactor::Adapter

        class << self
          attr_reader :adapted_interactor, :input_mapping, :output_mapping
        end

        @adapted_interactor = interactor
        @input_mapping = input_mapping
        @output_mapping = output_mapping
      end
    end

    # Internal: Normalize mapping keys to Symbols.
    def self.normalize(mapping)
      mapping.each_with_object({}) { |(local_key, parent_key), normalized|
        normalized[local_key.to_sym] = parent_key.to_sym
      }
    end

    # Internal: Invoke the adapted Interactor against an isolated Context.
    #
    # Returns nothing.
    def call
      verify_output_targets!

      local_context = build_local_context

      begin
        self.class.adapted_interactor.call!(local_context)
        merge_outputs!(local_context)
        @local_context = local_context
      rescue Failure => e
        raise unless e.context.equal?(local_context)
        context.fail!
      rescue StandardError
        local_context.rollback!
        raise
      end
    end

    # Internal: Roll back the isolated Context so that the Interactor
    # instances that actually ran observe the same Context they ran against.
    #
    # Returns nothing.
    def rollback
      @local_context&.rollback!
    end

    private

    # Internal: Reject output mappings that target the same parent key before
    # the adapted Interactor is given a chance to run.
    #
    # Returns nothing.
    def verify_output_targets!
      parent_keys = self.class.output_mapping.values
      duplicates = parent_keys.select { |parent_key|
        parent_keys.count(parent_key) > 1
      }.uniq

      return if duplicates.empty?

      raise ArgumentError,
        "multiple outputs map to parent context " \
          "key#{"s" if duplicates.length > 1}: " \
          "#{duplicates.map(&:inspect).join(", ")}"
    end

    # Internal: Build the isolated Context from mapped parent keys. A key
    # explicitly set to nil is copied; a missing key raises MissingInput.
    #
    # Returns an Interactor::Context.
    def build_local_context
      mapping = self.class.input_mapping

      missing = mapping.reject { |_local_key, parent_key|
        context.to_h.key?(parent_key)
      }.map { |local_key, parent_key|
        "#{parent_key.inspect} (as #{local_key.inspect})"
      }

      unless missing.empty?
        raise MissingInput,
          "missing required input#{"s" if missing.length > 1} for " \
            "#{self.class.adapted_interactor}: #{missing.join(", ")}"
      end

      values = mapping.each_with_object({}) { |(local_key, parent_key), result|
        result[local_key] = context[parent_key]
      }
      Context.new(values)
    end

    # Internal: Verify every declared output exists before writing any of
    # them, then merge all outputs into the parent Context together.
    #
    # Returns nothing.
    def merge_outputs!(local_context)
      mapping = self.class.output_mapping

      missing = mapping.keys.reject { |local_key|
        local_context.to_h.key?(local_key)
      }.map(&:inspect)

      unless missing.empty?
        raise MissingOutput,
          "missing expected output#{"s" if missing.length > 1} from " \
            "#{self.class.adapted_interactor}: #{missing.join(", ")}"
      end

      mapping.each { |local_key, parent_key|
        context[parent_key] = local_context[local_key]
      }
    end
  end
end
