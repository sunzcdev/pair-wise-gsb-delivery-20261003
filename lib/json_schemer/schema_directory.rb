# frozen_string_literal: true
module JSONSchemer
  # An in-memory directory of JSON Schema documents, usable as a
  # `ref_resolver`. Documents are registered in atomic batches: root and
  # nested `$id`, `$anchor` and `$dynamicAnchor` identifiers are indexed and
  # every static `$ref` is checked against the batch plus the already
  # registered documents before anything becomes visible. `$dynamicRef` is
  # never resolved eagerly, so it keeps its runtime dynamic-scope semantics.
  # Registration, lookup and schema compilation through the directory are
  # entirely in-memory and never perform network access.
  class SchemaDirectory
    class Error < StandardError; end
    class DuplicateResourceError < Error; end
    class DuplicateAnchorError < Error; end
    class UnresolvableRefError < Error; end

    # Keywords whose values are instance data or annotations rather than
    # subschemas; they are neither indexed nor checked for references.
    NON_SCHEMA_KEYWORDS = %w[const default enum examples].freeze

    ARRAY_INDEX_REGEX = /\A(0|[1-9]\d*)\z/.freeze

    Entry = Struct.new(:document, :pointer, :source)

    def initialize(schemas = nil)
      @documents = {}
      @resources = {}
      @anchors = {}
      register(schemas) if schemas
    end

    # Registers a batch of `{ uri => schema }` documents. The whole batch
    # is validated before anything is stored: if any document is rejected,
    # the directory is left untouched. Registered documents are deep-copied
    # and frozen, so later changes to the passed-in hashes have no effect.
    def register(schemas)
      documents = {}
      resources = {}
      anchors = {}
      refs = []

      schemas.each do |uri, schema|
        base_uri = normalize_uri(uri)
        document_key = base_uri.to_s
        raise Error, "schema document for `#{document_key}` must be an object" unless schema.is_a?(Hash)
        document = deep_copy(schema)
        add_resource(resources, document_key, [], base_uri)
        documents[document_key] = document
        index_node(document, document_key, base_uri, [], resources, anchors, refs)
      end

      validate_refs(refs, documents, resources, anchors)

      @documents.merge!(documents)
      @resources.merge!(resources)
      @anchors.merge!(anchors)
      self
    end

    # Returns the registered (sub)schema for a resource URI, an anchor URI
    # (`...#name`) or a JSON pointer URI (`...#/a/b`). Raises `UnknownRef`
    # when the URI cannot be resolved.
    def fetch(uri)
      uri = normalize_uri(uri)
      fragment = uri.fragment
      resource_uri = uri.dup
      resource_uri.fragment = nil
      resource_key = resource_uri.to_s

      if fragment.nil?
        entry = @resources[resource_key]
        raise UnknownRef, uri.to_s unless entry
        navigate(@documents.fetch(entry.document), entry.pointer, uri)
      elsif fragment.start_with?('/')
        entry = @resources[resource_key]
        raise UnknownRef, uri.to_s unless entry
        node = navigate(@documents.fetch(entry.document), entry.pointer, uri)
        navigate(node, pointer_tokens(fragment), uri)
      else
        entry = @anchors["#{resource_key}##{fragment}"]
        raise UnknownRef, uri.to_s unless entry
        navigate(@documents.fetch(entry.document), entry.pointer, uri)
      end
    end

    # `ref_resolver` interface: returns the schema for `uri` or nil.
    def call(uri)
      fetch(uri)
    rescue UnknownRef
      nil
    end

    def key?(uri)
      fetch(uri)
      true
    rescue UnknownRef
      false
    end

    def to_proc
      method(:call).to_proc
    end

  private

    def index_node(node, document_key, base_uri, pointer, resources, anchors, refs)
      case node
      when Hash
        id = node['$id']
        if id.is_a?(String) && !id.empty?
          id_uri = normalize_uri(URI.join(base_uri, id))
          add_resource(resources, document_key, pointer, id_uri) unless pointer.empty? && id_uri.to_s == document_key
          base_uri = id_uri
        end

        anchor = node['$anchor']
        add_anchor(anchors, document_key, pointer, base_uri, anchor) if anchor.is_a?(String)

        dynamic_anchor = node['$dynamicAnchor']
        add_anchor(anchors, document_key, pointer, base_uri, dynamic_anchor) if dynamic_anchor.is_a?(String)

        ref = node['$ref']
        refs << [normalize_uri(URI.join(base_uri, ref)), source_for(document_key, pointer)] if ref.is_a?(String)

        # `$dynamicRef` is intentionally neither indexed nor checked: it is
        # resolved against the runtime dynamic scope, not a fixed target.
        node.each do |keyword, value|
          index_node(value, document_key, base_uri, pointer + [keyword], resources, anchors, refs) unless NON_SCHEMA_KEYWORDS.include?(keyword)
        end
      when Array
        node.each_with_index do |item, index|
          index_node(item, document_key, base_uri, pointer + [index.to_s], resources, anchors, refs)
        end
      end
    end

    def add_resource(resources, document_key, pointer, uri)
      key = uri.to_s
      source = source_for(document_key, pointer)
      if (existing = resources[key] || @resources[key])
        raise DuplicateResourceError, "duplicate schema resource URI `#{key}` (registered by #{existing.source} and #{source})"
      end
      resources[key] = Entry.new(document_key, pointer, source)
    end

    def add_anchor(anchors, document_key, pointer, base_uri, anchor)
      key = "#{base_uri}##{anchor}"
      source = source_for(document_key, pointer)
      if (existing = anchors[key] || @anchors[key])
        raise DuplicateAnchorError, "duplicate anchor `#{key}` (registered by #{existing.source} and #{source})"
      end
      anchors[key] = Entry.new(document_key, pointer, source)
    end

    def validate_refs(refs, documents, resources, anchors)
      refs.each do |uri, source|
        fragment = uri.fragment
        resource_uri = uri.dup
        resource_uri.fragment = nil
        resource_key = resource_uri.to_s

        if fragment.nil?
          next if resources.key?(resource_key) || @resources.key?(resource_key)
          raise UnresolvableRefError, "unresolvable $ref `#{uri}` at #{source}: no schema resource registered for `#{resource_key}`"
        end

        if fragment.start_with?('/')
          entry = resources[resource_key] || @resources[resource_key]
          raise UnresolvableRefError, "unresolvable $ref `#{uri}` at #{source}: no schema resource registered for `#{resource_key}`" unless entry
          document = documents[entry.document] || @documents.fetch(entry.document)
          begin
            navigate(document, entry.pointer + pointer_tokens(fragment), uri)
          rescue UnknownRef
            raise UnresolvableRefError, "unresolvable $ref `#{uri}` at #{source}: invalid JSON pointer `#{fragment}`"
          end
        else
          anchor_key = "#{resource_key}##{fragment}"
          next if anchors.key?(anchor_key) || @anchors.key?(anchor_key)
          raise UnresolvableRefError, "unresolvable $ref `#{uri}` at #{source}: no anchor registered for `#{anchor_key}`"
        end
      end
    end

    def navigate(node, tokens, uri)
      tokens.reduce(node) do |current, token|
        case current
        when Hash
          raise UnknownRef, uri.to_s unless current.key?(token)
          current[token]
        when Array
          raise UnknownRef, uri.to_s unless token.match?(ARRAY_INDEX_REGEX) && token.to_i < current.size
          current[token.to_i]
        else
          raise UnknownRef, uri.to_s
        end
      end
    end

    def pointer_tokens(fragment)
      Hana::Pointer.parse(URI.decode_www_form_component(fragment))
    end

    def normalize_uri(uri)
      normalized = uri.is_a?(URI) ? uri.dup : URI.parse(uri.to_s)
      normalized.normalize!
      normalized.fragment = nil if normalized.fragment.to_s.empty?
      normalized
    end

    def source_for(document_key, pointer)
      "#{document_key}#/#{pointer.join('/')}"
    end

    def deep_copy(value)
      case value
      when Hash
        value.each_with_object({}) do |(key, item), copy|
          copied_key = case key
          when String
            key.dup.freeze
          when Symbol
            key.to_s.freeze
          else
            key
          end
          copy[copied_key] = deep_copy(item)
        end.freeze
      when Array
        value.map { |item| deep_copy(item) }.freeze
      when String
        value.dup.freeze
      else
        value
      end
    end
  end
end
