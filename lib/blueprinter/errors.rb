# frozen_string_literal: true

module Blueprinter
  module Errors
    autoload :InvalidProjection, 'blueprinter/errors/invalid_projection'
    autoload :InvalidBlueprint, 'blueprinter/errors/invalid_blueprint'
    autoload :UnknownView, 'blueprinter/errors/unknown_view'
  end
end
