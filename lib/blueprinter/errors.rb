# frozen_string_literal: true

module Blueprinter
  module Errors
    autoload :InvalidBlueprint, 'blueprinter/errors/invalid_blueprint'
    autoload :InvalidProjection, 'blueprinter/errors/invalid_projection'
    autoload :UnknownView, 'blueprinter/errors/unknown_view'
  end
end
