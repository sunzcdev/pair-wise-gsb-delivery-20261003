# frozen_string_literal: true

require 'blueprinter/blueprinter_error'

module Blueprinter
  module Errors
    class UnsupportedStreamOption < BlueprinterError
      def initialize(message = 'render_stream does not support the :root or :meta options')
        super
      end
    end
  end
end
