# encoding: binary
# frozen_string_literal: true

require "amq/protocol/spec"
require "amq/protocol/spec/loader"
require "amq/protocol/versioned"

module AMQ
  module Protocol
    # Versioned loading and generation entry point.
    #
    # Each description is exposed as its own namespace (a Module) containing
    # generated protocol classes, so two versions can be loaded and used in
    # the same process without redefining AMQ::Protocol constants.
    #
    #   v1 = AMQ::Protocol::Registry.load("0.9.1", paths: ["amqp-0.9.1.json"])
    #   v2 = AMQ::Protocol::Registry.load("0.9.2", paths: ["amqp-0.9.2.json"])
    #   v1.method(60, 40)   # basic.publish of 0.9.1
    #   v2.method(60, 40)   # basic.publish of 0.9.2, if declared differently
    #
    # Registries hold no IO handles anywhere: files are read once while
    # building a description, and namespaces only keep data and classes.
    class Registry
      # A loaded protocol version: its description plus the generated
      # namespace built from it.
      class Version
        attr_reader :description, :namespace

        def initialize(description, namespace)
          @description = description
          @namespace = namespace
        end

        def version
          description.version
        end

        def label
          description.label
        end

        def fingerprint
          @fingerprint ||= Registry.fingerprint(description)
        end

        # Metadata lookup: returns a Spec::Class for a class ID.
        def protocol_class(class_id)
          description.class_with_id(class_id)
        end

        # Metadata lookup: returns a Spec::Method for class/method IDs.
        def method(class_id, method_id)
          description.method_with_id(class_id, method_id)
        end

        # Metadata lookup by packed 32 bit index.
        def method_at_index(index)
          description.method_at_index(index)
        end

        # Generated method class for class/method IDs.
        def method_class(class_id, method_id)
          namespace.method(class_id, method_id)
        end

        # Generated protocol class constant, e.g. #class_constant("basic").
        def class_constant(name)
          spec = description.class_named(name)
          spec && namespace.const_get(spec.constant_name)
        end

        def classes
          description.classes
        end

        def methods
          description.methods
        end

        def preamble
          description.preamble
        end

        def port
          description.port
        end

        # Encoded description document, deterministic for equal descriptions.
        def to_h
          description.to_h
        end

        def inspect
          "#<#{self.class.name} #{version} methods=#{methods.size}>"
        end
      end

      # Raised when one label is asked to hold two different descriptions. A
      # description is therefore never silently replaced by a later load.
      class ConflictError < Spec::ConflictError; end

      class << self
        def versions
          @versions ||= {}
        end

        def clear!
          @versions = {}
        end

        # Loads one or more description documents for `version`.
        #
        # paths     - array of JSON file paths.
        # documents - array of ready-made documents (Hashes) or file paths.
        #
        # When several documents are given they are merged; conflicting
        # declarations raise ConflictError before anything is generated.
        def load(version, paths: nil, documents: nil, label: nil)
          loaded = Array(documents).map do |document|
            document.is_a?(String) ? Spec::Loader.load_file(document) : Spec::Loader.from_document(document)
          end
          Array(paths).each { |path| loaded << Spec::Loader.load_file(path) }

          raise ArgumentError, "no protocol description given for #{version}" if loaded.empty?

          description = loaded.size == 1 ? loaded.first : Spec::Loader.merge(loaded.map(&:to_h))
          unless description.version == version.to_s
            raise ArgumentError, "description is version #{description.version}, not #{version}"
          end

          register(description, label: label)
        end

        # Builds (or returns) the namespace for a Description.
        def register(description, label: nil)
          key = label || description.label
          existing = versions[key]
          if existing
            return existing if existing.description.digest == description.digest

            raise ConflictError.new(
              ["#{key.inspect} is already loaded with a different description"],
              key
            )
          end

          namespace = build_namespace(description)
          version = Version.new(description, namespace)
          versions[key] = version
          versions[description.version] ||= version
          version
        end

        def fetch(version)
          versions[version.to_s] || raise(KeyError, "protocol version #{version.inspect} is not loaded")
        end

        # Stable structural fingerprint: two descriptions with the same
        # declarations (including field order) have the same fingerprint.
        def fingerprint(description)
          require "digest"
          Digest::SHA256.hexdigest(description.digest)
        end

        private

        # Generates the version namespace. Every generated class is bound to
        # this description, so its lookups and codecs belong to this version.
        def build_namespace(description)
          namespace = Module.new
          namespace.define_singleton_method(:description) { description }
          namespace.define_singleton_method(:version) { description.version }
          namespace.define_singleton_method(:preamble) { description.preamble }
          namespace.define_singleton_method(:port) { description.port }

          # Constants are emitted in declaration order, first declaration wins,
          # so a merged description is deterministic.
          declared_constants = {}
          description.constants.each { |constant| declared_constants[constant.ruby_name] ||= constant }
          declared_constants.each do |name, constant|
            namespace.const_set(name, constant.value)
          end

          method_index = {}
          description.classes.each do |spec|
            klass = ::Class.new(Versioned::ProtocolClass)
            klass.configure(description: description, spec: spec)

            spec.methods.each do |method_spec|
              method_class = ::Class.new(Versioned::Method)
              method_class.configure(description: description, spec: method_spec)
              klass.register_method(method_class)
              method_index[method_spec.index] = method_class
            end

            namespace.const_set(spec.constant_name, klass)
          end
          method_index.freeze

          namespace.define_singleton_method(:classes) do
            description.classes.map { |c| const_get(c.constant_name) }
          end
          namespace.define_singleton_method(:method_classes) { method_index.values.freeze }
          namespace.define_singleton_method(:method_at_index) { |index| method_index[index] }

          namespace.define_singleton_method(:method) do |class_id, method_id|
            method_index[(class_id << 16) | method_id]
          end

          namespace.define_singleton_method(:protocol_class) do |class_id|
            spec = description.class_with_id(class_id)
            spec && const_get(spec.constant_name)
          end

          namespace
        end
      end
    end
  end
end
