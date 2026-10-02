# frozen_string_literal: true

module Blueprinter
  module Errors
    autoload :InvalidBlueprint, 'blueprinter/errors/invalid_blueprint'
    autoload :UnknownView, 'blueprinter/errors/unknown_view'
    autoload :InvalidRoot, 'blueprinter/errors/invalid_root'
    autoload :MetaRequiresRoot, 'blueprinter/errors/meta_requires_root'
    autoload :StreamingRootUnsupported, 'blueprinter/errors/streaming_root_unsupported'
    autoload :StreamingMetaUnsupported, 'blueprinter/errors/streaming_meta_unsupported'
  end
end
