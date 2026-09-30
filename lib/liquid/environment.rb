# frozen_string_literal: true

module Liquid
  # The Environment is the container for all configuration options of Liquid, such as
  # the registered tags, filters, and the default error mode.
  class Environment
    # The default error mode for all templates. This can be overridden on a
    # per-template basis.
    attr_accessor :error_mode

    # The tags that are available to use in the template.
    attr_accessor :tags

    # The strainer template which is used to store filters that are available to
    # use in templates.
    attr_accessor :strainer_template

    # The exception renderer that is used to render exceptions that are raised
    # when rendering a template
    attr_accessor :exception_renderer

    # The default file system that is used to load templates from.
    attr_accessor :file_system

    # The default resource limits that are used to limit the resources that a
    # template can consume.
    attr_accessor :default_resource_limits

    class << self
      # Creates a new environment instance.
      #
      # @param tags [Hash] The tags that are available to use in
      #  the template.
      # @param file_system The default file system that is used
      #  to load templates from.
      # @param error_mode [Symbol] The default error mode for all templates
      #  (either :strict2, :strict, :warn, or :lax).
      # @param exception_renderer [Proc] The exception renderer that is used to
      #   render exceptions.
      # @yieldparam environment [Environment] The environment instance that is being built.
      # @return [Environment] The new environment instance.
      def build(tags: nil, file_system: nil, error_mode: nil, exception_renderer: nil)
        ret = new
        ret.tags = tags if tags
        ret.file_system = file_system if file_system
        ret.error_mode = error_mode if error_mode
        ret.exception_renderer = exception_renderer if exception_renderer
        yield ret if block_given?
        ret.freeze
      end

      # Returns the default environment instance.
      #
      # @return [Environment] The default environment instance.
      def default
        @default ||= new
      end

      # Sets the default environment instance for the duration of the block
      #
      # @param environment [Environment] The environment instance to use as the default for the
      #   duration of the block.
      # @yield
      # @return [Object] The return value of the block.
      def dangerously_override(environment)
        original_default = @default
        @default = environment
        yield
      ensure
        @default = original_default
      end
    end

    # Initializes a new environment instance.
    # @api private
    def initialize
      @tags = Tags::STANDARD_TAGS.dup
      @error_mode = :lax
      @strainer_template = Class.new(StrainerTemplate).tap do |klass|
        klass.add_filter(StandardFilters)
      end
      @exception_renderer = ->(exception) { exception }
      @file_system = BlankFileSystem.new
      @default_resource_limits = Const::EMPTY_HASH
      @strainer_template_class_cache = {}
      @filter_signatures = {}
      @filter_signature_aliases = {}
    end

    # Registers a new tag with the environment.
    #
    # @param name [String] The name of the tag.
    # @param klass [Liquid::Tag] The class that implements the tag.
    # @return [void]
    def register_tag(name, klass)
      @tags[name] = klass
    end

    # Registers a new filter with the environment.
    #
    # @param filter [Module] The module that contains the filter methods.
    # @return [void]
    def register_filter(filter)
      @strainer_template_class_cache.clear
      @strainer_template.add_filter(filter)
    end

    # Registers multiple filters with this environment.
    #
    # @param filters [Array<Module>] The modules that contain the filter methods.
    # @return [self]
    def register_filters(filters)
      @strainer_template_class_cache.clear
      filters.each { |f| @strainer_template.add_filter(f) }
      self
    end

    # Registers a signature for a filter, describing the arguments it accepts.
    #
    # Templates parsed within this environment report signature mismatches at
    # parse time, and renders within this environment apply registered keyword
    # defaults. Filters without a registered signature keep their historical
    # behavior.
    #
    # @param name [String, Symbol] The name of the filter.
    # @param positional [Integer, Range] The allowed number of positional
    #   arguments (the filtered input itself is not counted). An Integer means
    #   an exact count; an endless range such as `1..` means "at least".
    # @param keywords [Hash] The allowed keyword arguments and their default
    #   values. Defaults are applied at render time when the template does not
    #   provide the keyword.
    # @param aliases [Array<String, Symbol>] Alternative filter names that
    #   resolve to this signature.
    # @return [Liquid::FilterSignature] The registered signature.
    def register_filter_signature(name, positional: 0..0, keywords: {}, aliases: [])
      signature = FilterSignature.new(name, positional: positional, keywords: keywords, aliases: aliases)
      @filter_signatures[signature.name] = signature
      signature.aliases.each do |filter_alias|
        @filter_signature_aliases[filter_alias] = signature.name
      end
      signature
    end

    # Returns the signature registered for the given filter name, resolving
    # aliases, or nil when the filter has no signature. A signature registered
    # under the exact name takes precedence over an alias.
    #
    # @param name [String, Symbol] The name of the filter as written in the
    #   template.
    # @return [Liquid::FilterSignature, nil] The signature, if any.
    def filter_signature_for(name)
      name = name.to_s
      @filter_signatures[name] || @filter_signatures[@filter_signature_aliases[name]]
    end

    # Creates a new strainer instance with the given filters, caching the result
    # for faster lookup.
    #
    # @param context [Liquid::Context] The context that the strainer will be
    #   used in.
    # @param filters [Array<Module>] The filters that the strainer will have
    #   access to.
    # @return [Liquid::Strainer] The new strainer instance.
    def create_strainer(context, filters = Const::EMPTY_ARRAY)
      return @strainer_template.new(context) if filters.empty?

      strainer_template = @strainer_template_class_cache[filters] ||= begin
        klass = Class.new(@strainer_template)
        filters.each { |f| klass.add_filter(f) }
        klass
      end

      strainer_template.new(context)
    end

    # Returns the names of all the filter methods that are available to use in
    # the strainer template.
    #
    # @return [Array<String>] The names of all the filter methods.
    def filter_method_names
      @strainer_template.filter_method_names
    end

    # Returns the tag class for the given tag name.
    #
    # @param name [String] The name of the tag.
    # @return [Liquid::Tag] The tag class.
    def tag_for_name(name)
      @tags[name]
    end

    # @api private
    def initialize_copy(other)
      super
      @filter_signatures = @filter_signatures.dup
      @filter_signature_aliases = @filter_signature_aliases.dup
    end

    def freeze
      @tags.freeze
      @filter_signatures.freeze
      @filter_signature_aliases.freeze
      # TODO: freeze the tags, currently this is not possible because of liquid-c
      # @strainer_template.freeze
      super
    end
  end
end
