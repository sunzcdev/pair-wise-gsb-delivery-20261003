# frozen_string_literal: true

require 'blueprinter/errors/invalid_projection'

module Blueprinter
  # @api private
  #
  # FieldProjection is a render-time, read-only subset of the fields a View would otherwise render.
  #
  # It is expressed using output field names:
  #
  #   fields: [:id, :name]
  #   fields: {id: true, name: true}
  #   fields: {id: false, owner: [:name]}
  #
  # A nested projection may be given for an association, e.g. `fields: {owner: [:name]}`.
  # A value of +false+ explicitly excludes a field (including identifiers). An
  # +:identifier+ key toggles identifier inclusion for that projection level.
  class FieldProjection
    IDENTIFIER_KEY = :identifier
    # Key under which a nested projection is passed to the association extractor.
    OPTION_KEY = :__blueprinter_nested_projection__
    # Key under which the dotted projection path is passed to the association extractor.
    PATH_OPTION_KEY = :__blueprinter_projection_path__

    # @return [FieldProjection, nil]
    def self.build(spec)
      return nil if spec.nil?
      return spec if spec.is_a?(FieldProjection)

      new(spec)
    end

    # @return [Boolean] whether identifiers are included at this level
    attr_accessor :include_identifiers

    def initialize(spec = nil)
      @include_identifiers = true
      @selected = {}

      merge!(spec) unless spec.nil?
    end

    # Returns true when this projection restricts the rendered output. A projection that was
    # constructed without any explicit selections does nothing.
    def active?
      !@selected.empty? || !@include_identifiers
    end

    # @param name [Symbol, String] an output field name
    # @return [Boolean] whether the field was explicitly excluded
    def excluded?(name)
      @selected[name.to_sym] == false
    end

    # @param name [Symbol, String] an output field name
    # @return [Boolean] whether the field was explicitly selected
    def includes?(name)
      value = @selected[name.to_sym]
      !value.nil? && value != false
    end

    # @param name [Symbol, String] an output field name
    # @return [FieldProjection, nil] the nested projection for an association, or nil
    def nested(name)
      value = @selected[name.to_sym]
      value.is_a?(FieldProjection) ? value : nil
    end

    # Validates this projection against the fields a View resolves to. Returns true when valid and
    # raises Blueprinter::Errors::InvalidProjection otherwise.
    #
    # @param fields [Array<Field>] the fields the selected view renders
    # @param path_prefix [String] the dotted path prefix used in error messages
    # @return [Boolean]
    def validate!(fields, path_prefix = nil)
      fields_by_name = {}
      fields.each { |field| fields_by_name[field.name] = field }

      @selected.each do |name, selection|
        path = join_path(path_prefix, name)
        field = fields_by_name[name]

        if field.nil?
          raise Errors::InvalidProjection, "Unknown field '#{path}'"
        end

        next unless selection.is_a?(FieldProjection)

        unless field.is_a?(Association)
          raise Errors::InvalidProjection,
                "Cannot project nested fields onto '#{path}': '#{name}' is not an association"
        end

        next if selection.empty_selection?

        associated_blueprint = field.options[:blueprint]

        # Dynamic blueprint Procs are resolved per object, so their nested
        # projections are validated at render time instead.
        next unless associated_blueprint.is_a?(Class)

        nested_fields = associated_blueprint.view_collection.fields_for(field.options[:view] || :default)
        selection.validate!(nested_fields, path)
      end

      true
    end

    # @return [Boolean] true when no explicit field selections were made (identifier toggle only)
    def empty_selection?
      @selected.empty?
    end

    private

    def merge!(spec)
      case spec
      when self.class
        @include_identifiers = spec.include_identifiers
        @selected = spec.instance_variable_get(:@selected).dup
      when Hash
        merge_hash!(spec.transform_keys(&:to_sym))
      when Array
        spec.each { |entry| merge_entry!(entry) }
      when Symbol, String
        merge_scalar!(spec, true)
      else
        raise ArgumentError,
              "Invalid field projection: expected a Symbol, Array, or Hash, got #{spec.class}"
      end
    end

    def merge_hash!(hash)
      if hash.key?(IDENTIFIER_KEY)
        @include_identifiers = hash.fetch(IDENTIFIER_KEY)
      end

      hash.each do |key, value|
        next if key == IDENTIFIER_KEY

        merge_entry!(key => value)
      end
    end

    def merge_entry!(entry)
      case entry
      when Symbol, String
        merge_scalar!(entry, true)
      when Hash
        entry.each { |name, spec| merge_pair!(name, spec) }
      else
        raise ArgumentError, "Invalid field projection entry: #{entry.inspect}"
      end
    end

    def merge_pair!(name, spec)
      name = name.to_sym

      case spec
      when FieldProjection
        merge_nested!(name, spec)
      when Hash, Array
        merge_nested!(name, self.class.new(spec))
      when false
        merge_scalar!(name, false)
      when nil, true
        merge_scalar!(name, true)
      else
        raise ArgumentError,
              "Invalid field projection value for '#{name}': #{spec.inspect}"
      end
    end

    def merge_scalar!(name, included)
      name = name.to_sym
      previous = @selected[name]

      if previous.is_a?(FieldProjection) && included
        return
      end

      @selected[name] = included
    end

    def merge_nested!(name, projection)
      previous = @selected[name]

      if previous.is_a?(FieldProjection)
        merge_projections!(previous, projection)
      else
        @selected[name] = projection
      end
    end

    def merge_projections!(target, source)
      target.send(:merge!, source) unless target.equal?(source)
    end

    def join_path(prefix, name)
      prefix ? "#{prefix}.#{name}" : name.to_s
    end
  end
end
