# frozen_string_literal: true

require 'blueprinter/blueprinter_error'

module Blueprinter
  module Errors
    # Raised when a :root option is given to a streaming render, which only
    # emits a top-level JSON array.
    class StreamingRootUnsupported < BlueprinterError
      def initialize(message = 'the streaming JSON array entry does not support :root')
        super
      end
    end
  end
end
