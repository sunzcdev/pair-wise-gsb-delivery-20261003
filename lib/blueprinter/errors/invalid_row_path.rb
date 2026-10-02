# frozen_string_literal: true

require 'blueprinter/blueprinter_error'

module Blueprinter
  module Errors
    # Raised when a column or collection path passed to {Blueprinter::Base.render_rows}
    # cannot be resolved against the rendered Blueprint.
    class InvalidRowPath < BlueprinterError; end
  end
end
