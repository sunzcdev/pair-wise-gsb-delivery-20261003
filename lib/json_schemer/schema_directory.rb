# frozen_string_literal: true
module JSONSchemer
  # In-memory directory of JSON Schema documents, usable as a `ref_resolver`.
  #
  # Documents are registered in batches. Each batch is indexed (root and nested
  # `$id` resources plus `$anchor`/`$dynamicAnchor` anchors) and every static
  # `$ref` in the batch is checked against the combined index (batch + already
  # registered documents). A batch only takes effect if all of its documents
  # pass validation, so a failed registration leaves the directory untouched.
  #
  # `$dynamicRef` values are intentionally not resolved at registration time;
  # they keep their runtime dynamic-scope semantics.
  #
  # The directory never performs network access: lookups are served from the
  # registered documents and unknown URIs raise `UnknownRef`.
  #
  #   directory = JSONSchemer::SchemaDirectory.new
  #   directory.register(
  #     'http://example.com/root.json' => { '$ref' => 'other.json' },
  #     'http://example.com/other.json' => { 'type' => 'string' }
  #   )
  #   schemer = JSONSchemer.schema({ '$ref' => 'http://example.com/root.json' }, :ref_resolver => directory)
  class SchemaDirectory
    class Error < StandardError; end
    class DuplicateResourceError < Error; end
    class DuplicateAnchorError < Error; end
    class UnresolvableRefError < Error; end

    # keywords whose values are data, not subschemas
    NON_SCHEMA_KEYWORDS = %w[const default enum examples].freeze

    ANCHOR_NAMESPACES = %i[static dynamic].freeze

    def initialize(documents = nil)
      @resources = {}
      @anchors = { :static => {}, :dynamic => {} }
      register(documents) if documents
    end

    # Registers a batch of documents atomically. Accepts a hash of
    # retrieval URI => schema document, or an array of documents that each
    # carry a root `$id`. Returns self so calls can be chained.
    def register(documents)
      entries = coerce_entries(documents)

      resources = @resources.dup
      anchors = { :static => @anchors[:static].dup, :dynamic => @anchors[:dynamic].dup }
      refs = []

      entries.each do |source, document|
        document = deep_dup(document)
        unless document.is_a?(Hash) || document == true || document == false
          raise Error, "schema document for `#{source}` must be an object or boolean"
        end
        index_document(document, source, resources, anchors, refs)
      end

      refs.each { |ref| validate_ref(ref, resources, anchors) }

      @resources = resources
      @anchors = anchors
      self
    end

    # `ref_resolver` protocol: returns the registered schema document for the
    # given (optionally fragmented) URI, or raises `UnknownRef`.
    def call(uri)
      resource = @resources[resource_key(uri)]
      raise UnknownRef, uri.to_s unless resource
      resource[:node]
    end
    alias fetch call

    def key?(uri)
      @resources.key?(resource_key(uri))
    end

    def resource_uris
      @resources.keys
    end

    def size
      @resources.size
    end

  private

    def coerce_entries(documents)
      case documents
      when Hash
        documents.map { |uri, document| [normalize_uri(uri).to_s, document] }
      else
        Array(documents).map do |document|
          id = document['$id'] if document.is_a?(Hash)
          raise Error, 'documents registered without an explicit URI must define a root `$id`' unless id.is_a?(String)
          [normalize_uri(id).to_s, document]
        end
      end
    end

    def normalize_uri(uri)
      uri = URI.parse(uri.to_s) unless uri.is_a?(URI::Generic)
      normalized = uri.normalize
      normalized.fragment = nil if normalized.fragment&.empty?
      normalized
    end

    def resource_key(uri)
      uri = normalize_uri(uri)
      uri.fragment = nil
      uri.to_s
    end

    def index_document(document, source, resources, anchors, refs)
      add_resource(resources, source, document, source, '')
      walk(document, source, source, source, '', resources, anchors, refs) if document.is_a?(Hash)
    end

    def walk(node, base_uri, resource_uri, source, path, resources, anchors, refs)
      case node
      when Hash
        if node['$id'].is_a?(String)
          id_uri = URI.join(base_uri, node['$id']).normalize
          id_fragment = id_uri.fragment
          id_uri.fragment = nil
          if id_fragment && !id_fragment.empty?
            # draft-4 style `id` with a plain-name fragment acts as an anchor
            add_anchor(anchors, :static, id_uri.to_s, id_fragment, source, path, '$id') unless id_fragment.start_with?('/')
          else
            resource_uri = id_uri.to_s
            add_resource(resources, resource_uri, node, source, path)
            base_uri = resource_uri
          end
        end

        if node['$anchor'].is_a?(String)
          add_anchor(anchors, :static, resource_uri, node['$anchor'], source, path, '$anchor')
        end

        if node['$dynamicAnchor'].is_a?(String)
          add_anchor(anchors, :dynamic, resource_uri, node['$dynamicAnchor'], source, path, '$dynamicAnchor')
        end

        if node['$ref'].is_a?(String)
          refs << {
            :uri => URI.join(base_uri, node['$ref']).normalize,
            :source => source,
            :path => path.empty? ? '/$ref' : "#{path}/$ref"
          }
        end

        node.each do |keyword, value|
          next if NON_SCHEMA_KEYWORDS.include?(keyword)
          walk(value, base_uri, resource_uri, source, "#{path}/#{Location.escape_json_pointer_token(keyword)}", resources, anchors, refs)
        end
      when Array
        node.each_with_index do |value, index|
          walk(value, base_uri, resource_uri, source, "#{path}/#{index}", resources, anchors, refs)
        end
      end
    end

    def add_resource(resources, uri, node, source, path)
      existing = resources[uri]
      if existing && !existing[:node].equal?(node)
        raise DuplicateResourceError,
          "schema resource `#{uri}` from `#{source}` (#{display_path(path)}) conflicts with the resource already provided by `#{existing[:source]}` (#{display_path(existing[:path])})"
      end
      resources[uri] ||= { :node => node, :source => source, :path => path }
    end

    def add_anchor(anchors, namespace, resource_uri, name, source, path, keyword)
      key = "#{resource_uri}##{name}"
      existing = anchors.fetch(namespace)[key]
      if existing
        raise DuplicateAnchorError,
          "duplicate anchor `#{name}` in resource `#{resource_uri}`: defined by `#{keyword}` at #{display_path(path)} in `#{source}` and by `#{existing[:keyword]}` at #{display_path(existing[:path])} in `#{existing[:source]}`"
      end
      anchors[namespace][key] = { :source => source, :path => path, :keyword => keyword }
    end

    def validate_ref(ref, resources, anchors)
      uri = ref[:uri].dup
      fragment = uri.fragment
      uri.fragment = nil
      resource_uri = uri.to_s
      resource = resources[resource_uri]

      unless resource
        raise UnresolvableRefError,
          "cannot resolve $ref `#{ref[:uri]}` at #{display_path(ref[:path])} in `#{ref[:source]}`: no schema resource registered for `#{resource_uri}`"
      end

      return if fragment.nil? || fragment.empty?

      if fragment.start_with?('/')
        validate_pointer(resource, fragment, ref)
      else
        key = "#{resource_uri}##{fragment}"
        unless ANCHOR_NAMESPACES.any? { |namespace| anchors[namespace].key?(key) }
          raise UnresolvableRefError,
            "cannot resolve $ref `#{ref[:uri]}` at #{display_path(ref[:path])} in `#{ref[:source]}`: no anchor `#{fragment}` in resource `#{resource_uri}`"
        end
      end
    end

    def validate_pointer(resource, fragment, ref)
      pointer = URI.decode_www_form_component(fragment)
      node = resource[:node]
      Hana::Pointer.parse(pointer).each do |token|
        node =
          if node.is_a?(Hash) && node.key?(token)
            node[token]
          elsif node.is_a?(Array) && token.match?(/\A(0|[1-9]\d*)\z/) && token.to_i < node.size
            node[token.to_i]
          else
            raise UnresolvableRefError,
              "cannot resolve $ref `#{ref[:uri]}` at #{display_path(ref[:path])} in `#{ref[:source]}`: no value at pointer `#{pointer}` in resource `#{resource[:source]}`"
          end
      end
    end

    def display_path(path)
      path.empty? ? '/' : path
    end

    def deep_dup(value)
      case value
      when Hash
        value.each_with_object({}) do |(key, item), out|
          out[key.is_a?(Symbol) ? key.to_s : key] = deep_dup(item)
        end
      when Array
        value.map { |item| deep_dup(item) }
      when String
        value.dup
      else
        value
      end
    end
  end
end
