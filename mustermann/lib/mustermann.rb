# frozen_string_literal: true
require 'mustermann/pattern'
require 'mustermann/composite'
require 'mustermann/concat'
require 'thread'

# Namespace and main entry point for the Mustermann library.
#
# Under normal circumstances the only external API entry point you should be using is {Mustermann.new}.
module Mustermann
  # Type to use if no type is given.
  # @api private
  DEFAULT_TYPE = :sinatra

  # Creates a new pattern based on input.
  #
  # * From {Mustermann::Pattern}: returns given pattern.
  # * From String: creates a pattern from the string, depending on type option (defaults to {Mustermann::Sinatra})
  # * From Regexp: creates a {Mustermann::Regular} pattern.
  # * From Symbol: creates a {Mustermann::Sinatra} pattern with a single named capture named after the input.
  # * From an Array or multiple inputs: creates a new pattern from each element, combines them to a {Mustermann::Composite}.
  # * From anything else: Will try to call to_pattern on it or raise a TypeError.
  #
  # Note that if the input is a {Mustermann::Pattern}, Regexp or Symbol, the type option is ignored and if to_pattern is
  # called on the object, the type will be handed on but might be ignored by the input object.
  #
  # If you want to enforce the pattern type, you should create them via their expected class.
  #
  # @example creating patterns
  #   require 'mustermann'
  #
  #   Mustermann.new("/:name")                    # => #<Mustermann::Sinatra:"/example">
  #   Mustermann.new("/{name}", type: :template)  # => #<Mustermann::Template:"/{name}">
  #   Mustermann.new(/.*/)                        # => #<Mustermann::Regular:".*">
  #   Mustermann.new(:name, capture: :word)       # => #<Mustermann::Sinatra:":name">
  #   Mustermann.new("/", "/*.jpg", type: :shell) # => #<Mustermann::Composite:(shell:"/" | shell:"/*.jpg")>
  #
  # @example using custom #to_pattern
  #   require 'mustermann'
  #
  #   class MyObject
  #     def to_pattern(**options)
  #       Mustermann.new("/:name", **options)
  #     end
  #   end
  #
  #   Mustermann.new(MyObject.new, type: :rails) # => #<Mustermann::Rails:"/:name">
  #
  # @example enforcing type
  #   require 'mustermann/sinatra'
  #
  #   Mustermann::Sinatra.new("/:name")
  #
  # @param [String, Pattern, Regexp, Symbol, #to_pattern, Array<String, Pattern, Regexp, Symbol, #to_pattern>]
  #   input The representation of the pattern
  # @param [Hash] options The options hash
  # @return [Mustermann::Pattern] pattern corresponding to string.
  # @raise (see [])
  # @raise (see Mustermann::Pattern.new)
  # @raise [TypeError] if the passed object cannot be converted to a pattern
  # @see file:README.md#Types_and_Options "Types and Options" in the README
  def self.new(*input, type: DEFAULT_TYPE, operator: :|, **options)
    type ||= DEFAULT_TYPE
    input  = input.first if input.size < 2
    case input
    when Pattern then input
    when Regexp  then self[:regexp].new(input, **options)
    when String  then self[type].new(input, **options)
    when Symbol  then self[:sinatra].new(input.inspect, **options)
    when Array   then input.map { |i| new(i, type: type, **options) }.inject(operator)
    else
      pattern = input.to_pattern(type: type, **options) if input.respond_to? :to_pattern
      raise TypeError, "#{input.class} can't be coerced into Mustermann::Pattern" if pattern.nil?
      pattern
    end
  end

  @mutex = Mutex.new
  @types = {}

  # Maps a type to its factory.
  #
  # @example
  #   Mustermann[:sinatra] # => Mustermann::Sinatra
  #
  # @param [Symbol] name a pattern type identifier
  # @raise [ArgumentError] if the type is not supported
  # @return [Class, #new] pattern factory
  def self.[](name)
    return name if name.respond_to? :new
    @types.fetch(normalized = normalized_type(name)) do
      @mutex.synchronize do
        error = try_require "mustermann/#{normalized}"
        @types.fetch(normalized) { raise ArgumentError, "unsupported type %p#{" (#{error.message})" if error}" % name }
      end
    end
  end

  # @return [LoadError, nil]
  # @!visibility private
  def self.try_require(path)
    require(path)
    nil
  rescue LoadError => error
    raise(error) unless error.path == path
    error
  end

  # @!visibility private
  def self.register(name, type)
    @types[normalized_type(name)] = type
  end

  # @!visibility private
  def self.normalized_type(type)
    @normalized_types       ||= {}
    @normalized_types[type] ||= type.to_s.gsub('-', '_').downcase
  end

  # Statically analyzes the relationship between two path patterns.
  #
  # Decides, from the patterns themselves rather than a finite list of sample URLs,
  # whether their languages intersect and whether the first is contained in the
  # second. When the conclusion needs a concrete path (an intersection witness or a
  # containment counterexample), the result contains one, and every path is checked
  # against the patterns' real matching interface.
  #
  # Literal segments, ordinary captures, splats, optional segments and unions of all
  # of these are supported, as are built-in typed capture converters. Custom
  # constraints that cannot be analyzed reliably (arbitrary Regexps, unknown POSIX
  # capture classes, shell/regular patterns, etc.) produce an unknown answer together
  # with the reason instead of a potentially wrong conclusion.
  #
  # Patterns are compared by what they match, never by their source strings, so
  # patterns written with different options or encodings are still compared
  # correctly.
  #
  # @example
  #   relation = Mustermann.relate('/users/:id', '/users/:name')
  #   relation.intersect? # => true
  #   relation.witness    # => "/users/a"
  #   relation.subset?    # => false
  #
  # @param first [Mustermann::Pattern, String, Symbol, Regexp, #to_pattern]
  # @param second [Mustermann::Pattern, String, Symbol, Regexp, #to_pattern]
  # @param options [Hash] options passed to {Mustermann.new} for string inputs
  # @return [Mustermann::Relation]
  # @see Mustermann::Pattern#relation_to
  def self.relate(first, second, **options)
    require 'mustermann/relation'
    first  = new(first, **options)  unless first.is_a?(Pattern)
    second = new(second, **options) unless second.is_a?(Pattern)
    Relation.new(first, second)
  end

  # @!visibility private
  def self.dedup(object)
    return object.freeze unless @dedup
    # need to check this first because JRuby raises on @dedup.key?(nil)
    case object
    when nil, true, false, Integer, Symbol, Module
      return object
    end

    canonical = @dedup.getkey(object)
    return canonical if canonical

    case object
    when Array
      object.map! { |o| dedup(o) }
      @dedup[object.freeze] = true
    when Hash
      object.transform_values! { |v| dedup(v) }
      object.transform_keys! { |k| dedup(k) }
      @dedup[object.freeze] = true
    when String
      @dedup[-object] = true
    else
      @dedup[object.freeze] = true
    end
    object
  end

  @dedup = defined?(ObjectSpace::WeakKeyMap) ? ObjectSpace::WeakKeyMap.new : false

  # @!visibility private
  EMPTY_STRING = dedup(-'')

  # @!visibility private
  EMPTY_ARRAY = dedup([])

  # @!visibility private
  EMPTY_HASH = dedup({})
end
