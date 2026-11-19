# frozen_string_literal: true

require "json"

module AMQ
  module Protocol
    module Spec
      # Builds Description objects from protocol description documents
      # (the JSON format used by rabbitmq-codegen) and merges several
      # documents into one version.
      #
      # Only file paths are read here; everything downstream works on plain,
      # IO-free data.
      module Loader
        module_function

        # Reads and parses a description document from a file path.
        def load_file(path, label: nil)
          from_document(JSON.parse(File.read(path)), label: label || path.to_s)
        end

        # Parses one description document (a Hash, typically JSON-parsed).
        #
        # Keys follow rabbitmq-codegen: "name", "major-version",
        # "minor-version", "revision", "port", "domains", "constants",
        # "classes". Field order is preserved exactly as declared because
        # it is part of the wire format.
        def from_document(document, label: nil, port: nil, preamble: nil)
          doc = stringify(document)
          version = version_string(doc)
          domains = build_domains(doc["domains"] || [])

          Description.new(
            version: version,
            name: doc["name"] || "AMQP",
            port: port || doc["port"] || 5672,
            preamble: preamble || doc["preamble"],
            domains: domains,
            constants: build_constants(doc["constants"] || []),
            classes: build_classes(doc["classes"] || []),
            label: label || version
          )
        end

        # Merges several documents of the same protocol version into one
        # Description. Raises ConflictError when the documents disagree about
        # a declared domain, constant, class, method or property instead of
        # silently letting one overwrite another.
        def merge(documents, label: nil)
          raise ArgumentError, "nothing to merge" if documents.nil? || documents.empty?

          parsed = documents.map { |d| stringify(d) }
          versions = parsed.map { |d| version_string(d) }.uniq
          if versions.size > 1
            raise ConflictError.new(["documents declare versions #{versions.join(', ')}"], label)
          end

          ports = parsed.map { |d| d["port"] }.compact.uniq
          base = {
            "name" => parsed.map { |d| d["name"] }.compact.first,
            "major-version" => parsed.first["major-version"],
            "minor-version" => parsed.first["minor-version"],
            "revision" => parsed.first["revision"],
            "port" => ports.size == 1 ? ports.first : nil,
            "domains" => parsed.flat_map { |d| d["domains"] || [] },
            "constants" => parsed.flat_map { |d| d["constants"] || [] },
            "classes" => parsed.flat_map { |d| d["classes"] || [] }
          }

          conflicts = collect_conflicts(base)
          raise ConflictError.new(conflicts, label || versions.first) unless conflicts.empty?

          from_document(base, label: label || parsed.first["name"])
        end

        # Deep-copies a document with String keys so JSON and Ruby callers
        # are handled the same way.
        def stringify(value)
          case value
          when Hash
            value.each_with_object({}) { |(k, v), acc| acc[k.to_s] = stringify(v) }
          when Array
            value.map { |v| stringify(v) }
          else
            value
          end
        end

        # Deep-freezes a document in place. Used by generated sources so their
        # embedded DOCUMENT constant cannot be mutated at load time.
        def freeze_document(value)
          case value
          when Hash
            value.each { |key, item| freeze_document(key); freeze_document(item) }
          when Array
            value.each { |item| freeze_document(item) }
          end
          value.freeze
        end

        def version_string(doc)
          %w[major-version minor-version].each do |key|
            raise ArgumentError, "description is missing #{key}" if doc[key].nil?
          end

          "#{doc['major-version']}.#{doc['minor-version']}.#{Integer(doc['revision'] || 0)}"
        end

        def build_domains(entries)
          entries.map do |entry|
            name, type_name =
              case entry
              when Hash then [entry["name"], entry["type"] || entry["domain"]]
              when Array then [entry[0], entry[1]]
              else raise ArgumentError, "unsupported domain declaration #{entry.inspect}"
              end
            Domain.new(name: name.to_s, type: type_name.to_s)
          end
        end

        def build_constants(entries)
          entries.map do |entry|
            Constant.new(name: entry["name"].to_s, value: entry["value"], class_name: entry["class"])
          end
        end

        def build_classes(entries)
          entries.map do |entry|
            class_name = entry["name"].to_s
            class_id = Integer(entry["id"])
            methods = (entry["methods"] || []).map do |m|
              Method.new(
                class_name: class_name,
                class_id: class_id,
                name: m["name"].to_s,
                id: Integer(m["id"]),
                arguments: (m["arguments"] || []).map { |a| build_field(a) },
                content: m["content"],
                synchronous: m["synchronous"]
              )
            end

            Class.new(
              name: class_name,
              id: class_id,
              methods: methods,
              properties: (entry["properties"] || []).map { |p| build_field(p) }
            )
          end
        end

        # Builds a field from its declaration. Domain names are kept as
        # written; Description resolves them against its own domain table so
        # the same description can never see another version's domains.
        def build_field(entry)
          attrs = { name: entry["name"].to_s }
          if entry.key?("domain")
            attrs[:domain] = entry["domain"].to_s
          else
            attrs[:type] = (entry["type"] || entry["domain"]).to_s
          end
          # A JSON null under "default-value" is a declared default.
          attrs[:default] = entry["default-value"] if entry.key?("default-value")
          Field.new(**attrs)
        end

        def collect_conflicts(doc)
          conflicts = []

          domain_seen = {}
          (doc["domains"] || []).each do |entry|
            name, type_name =
              case entry
              when Hash then [entry["name"], entry["type"] || entry["domain"]]
              when Array then [entry[0], entry[1]]
              end
            existing = domain_seen[name]
            if existing && existing != type_name
              conflicts << "domain #{name.inspect} is declared as both #{existing.inspect} and #{type_name.inspect}"
            end
            domain_seen[name] = type_name
          end

          constant_seen = {}
          (doc["constants"] || []).each do |entry|
            name = entry["name"]
            existing = constant_seen[name]
            if existing && existing != entry
              conflicts << "constant #{name.inspect} has conflicting value or error class"
            end
            constant_seen[name] = entry
          end

          class_ids = {}
          method_declarations = {}
          property_declarations = {}
          (doc["classes"] || []).each do |entry|
            class_name = entry["name"]
            id = entry["id"]
            if (existing = class_ids[class_name]) && existing != id
              conflicts << "class #{class_name.inspect} is declared with ids #{existing.inspect} and #{id.inspect}"
            end
            class_ids[class_name] = id

            (entry["methods"] || []).each do |m|
              key = [class_name, m["name"]]
              if (existing = method_declarations[key])
                conflicts << "method #{class_name}.#{m['name']} has conflicting declarations" if existing != m
              end
              method_declarations[key] = m
            end

            (entry["properties"] || []).each do |p|
              key = [class_name, p["name"]]
              if (existing = property_declarations[key])
                conflicts << "property #{class_name}.#{p['name']} has conflicting declarations" if existing != p
              end
              property_declarations[key] = p
            end
          end

          conflicts
        end
      end
    end
  end
end
