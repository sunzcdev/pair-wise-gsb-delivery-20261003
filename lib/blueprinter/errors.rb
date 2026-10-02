# frozen_string_literal: true

module Blueprinter
  module Errors
    autoload :InvalidBlueprint, 'blueprinter/errors/invalid_blueprint'
    autoload :MissingPreloadResult, 'blueprinter/errors/missing_preload_result'
    autoload :UnknownView, 'blueprinter/errors/unknown_view'
  end
end
