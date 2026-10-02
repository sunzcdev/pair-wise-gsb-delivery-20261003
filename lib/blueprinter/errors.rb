# frozen_string_literal: true

module Blueprinter
  module Errors
    autoload :InvalidBlueprint, 'blueprinter/errors/invalid_blueprint'
    autoload :InvalidRowPath, 'blueprinter/errors/invalid_row_path'
    autoload :UnknownView, 'blueprinter/errors/unknown_view'
  end
end
