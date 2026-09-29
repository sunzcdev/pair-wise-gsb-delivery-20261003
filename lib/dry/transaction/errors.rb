# frozen_string_literal: true

module Dry
  module Transaction
    class InvalidStepError < ArgumentError
      def initialize(step_name)
        super("step +`#{step_name}`+ must respond to `#call`")
      end
    end

    class MissingStepError < ArgumentError
      def initialize(step_name)
        super("Definition for step +`#{step_name}`+ is missing")
      end
    end

    class InvalidResultError < ArgumentError
      def initialize(step_name)
        super("step +#{step_name}+ must return a Result object")
      end
    end

    class MissingCatchListError < ArgumentError
      def initialize(step_name)
        super("step +#{step_name}+ requires one or more exception classes provided via +catch:+")
      end
    end

    class MissingCheckListError < ArgumentError
      def initialize(step_name)
        super("validation group +#{step_name}+ requires one or more checks provided via +checks:+")
      end
    end

    class InvalidCheckOutputError < ArgumentError
      def initialize(step_name, check_name)
        super(
          "check +#{check_name}+ in validation group +#{step_name}+ must return a Success " \
          "wrapping a hash of named outputs"
        )
      end
    end

    class DuplicateCheckOutputError < ArgumentError
      def initialize(step_name, check_name, key)
        super(
          "check +#{check_name}+ in validation group +#{step_name}+ declares output key " \
          "+#{key}+, which is already declared by another check in the group"
        )
      end
    end
  end
end
