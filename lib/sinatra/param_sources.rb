# frozen_string_literal: true

module Sinatra
  # Tracks where each request parameter came from. Sinatra merges route
  # captures, the query string and the request body into a single +params+
  # hash, which makes it impossible for application code to tell whether
  # +params[:id]+ came from the path (/users/7) or from the query string
  # (?id=9). ParamSources keeps the three original trees apart, remembers
  # the original (bracketed) key paths and detects repeated query keys.
  #
  # The view is exposed to application code through the +param_sources+
  # helper and is enforced through the +param_policy+ class DSL.
  class ParamSources
    SOURCES = %i[route query body].freeze

    # Sentinel returned by #value_at when a path is absent from a tree.
    NOT_FOUND = Object.new

    # Raw query keys (after unescaping) that appeared more than once in the
    # query string, mapped to the list of values seen, in order. Keys ending
    # in "[]" are regular array usage and are not considered duplicates.
    attr_reader :duplicates

    def initialize(request = nil)
      @trees = {
        route: IndifferentHash.new,
        query: IndifferentHash.new,
        body: IndifferentHash.new
      }
      @duplicates = {}
      return unless request

      @trees[:query] = wrap { request.GET }
      @trees[:body]  = wrap { request.POST }
      @duplicates = self.class.find_duplicates(request.query_string)
    end

    # The parameter tree for a single source (:route, :query or :body).
    def [](source)
      @trees.fetch(source.to_sym)
    end

    # All three trees, keyed by source name.
    def to_h
      { route: @trees[:route], query: @trees[:query], body: @trees[:body] }
    end

    # Original key paths present in a source tree, using Rack's bracket
    # notation, e.g. "user[name]" or "ids[]".
    def paths(source)
      result = []
      self.class.flatten_paths(@trees.fetch(source.to_sym), nil, result)
      result
    end

    # The sources that provide a value for the given key path.
    def sources_for(path)
      SOURCES.select { |source| !value_at(source, path).equal?(NOT_FOUND) }
    end

    # The value stored under a bracketed key path ("user[name]") in one of
    # the source trees, or NOT_FOUND when the path is absent.
    def value_at(source, path)
      current = @trees.fetch(source.to_sym)
      self.class.path_segments(path).each do |segment|
        case current
        when Hash
          return NOT_FOUND unless current.key?(segment)

          current = current[segment]
        when Array
          values = current.filter_map { |el| el[segment] if el.is_a?(Hash) && el.key?(segment) }
          return NOT_FOUND if values.empty?

          current = values
        else
          return NOT_FOUND
        end
      end
      current
    end

    # A conflict is a key path provided by more than one source (or repeated
    # in the query string) with disagreeing values. Values of different
    # kinds (scalar vs array/hash) always conflict, even when they would
    # compare equal.
    Conflict = Struct.new(:path, :values, :kind)

    def conflicts
      result = []
      all_paths = SOURCES.flat_map { |source| paths(source).map { |p| p.sub(/\[\]\z/, '') } }.uniq
      all_paths.each do |path|
        found = SOURCES.filter_map do |source|
          value = value_at(source, path)
          [source, value] unless value.equal?(NOT_FOUND)
        end
        next if found.size < 2

        kind = self.class.conflict_kind(found.map(&:last))
        result << Conflict.new(path, found.to_h, kind) if kind
      end
      duplicates.each do |key, values|
        next unless values.uniq.size > 1

        result << Conflict.new(key, { query: values }, :duplicate)
      end
      result
    end

    # :type when scalars are mixed with arrays/hashes, :value when values
    # of the same kind disagree, nil when there is no conflict.
    def self.conflict_kind(values)
      return nil if values.size < 2

      collections = values.map { |v| v.is_a?(Array) || v.is_a?(Hash) }
      return :type if collections.uniq.size > 1

      values.uniq.size > 1 ? :value : nil
    end

    # Merge route captures into the route tree. Called for every matching
    # route candidate; paired with #clear_route when the candidate is done.
    def track_route(params)
      @trees[:route].merge!(params)
    end

    # Forget the route tree of the current candidate. After a route passes,
    # the next candidate must not inherit its captures.
    def clear_route
      @trees[:route] = IndifferentHash.new
    end

    # Split a bracketed key path into segments: "user[name]" => ["user",
    # "name"], "ids[]" => ["ids"].
    def self.path_segments(path)
      path.to_s.split(/\[|\]/).reject(&:empty?)
    end

    # Collect the original key paths of a (possibly nested) parameter tree.
    def self.flatten_paths(node, prefix, acc)
      node.each_pair do |key, value|
        path = prefix ? "#{prefix}[#{key}]" : key.to_s
        case value
        when Hash
          flatten_paths(value, path, acc)
        when Array
          acc << "#{path}[]"
          value.each { |el| flatten_paths(el, "#{path}[]", acc) if el.is_a?(Hash) }
        else
          acc << path
        end
      end
      acc
    end

    # Raw query keys occurring more than once, mapped to their values.
    def self.find_duplicates(query_string)
      seen = Hash.new { |hash, key| hash[key] = [] }
      query_string.to_s.split('&').each do |pair|
        key, _, value = pair.partition('=')
        next if key.empty?

        key = unescape(key)
        next if key.end_with?('[]')

        seen[key] << unescape(value)
      end
      seen.select { |_, values| values.size > 1 }
    end

    def self.unescape(component)
      Rack::Utils.unescape(component.to_s.tr('+', ' '))
    rescue ArgumentError
      component.to_s
    end

    private

    def wrap
      IndifferentHash.new.merge!(yield)
    rescue StandardError
      # Malformed query strings or bodies are reported the usual way when
      # Sinatra merges @request.params; the source view stays empty.
      IndifferentHash.new
    end
  end
end
