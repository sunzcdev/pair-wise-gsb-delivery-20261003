require "multi_json/options"
require "multi_json/version"
require "multi_json/adapter_error"
require "multi_json/parse_error"
require "multi_json/options_cache"
require "multi_json/sequence"

module MultiJson
  include Options
  extend self

  def default_options=(value)
    Kernel.warn "MultiJson.default_options setter is deprecated\nUse MultiJson.load_options and MultiJson.dump_options instead"

    self.load_options = self.dump_options = value
  end

  def default_options
    Kernel.warn "MultiJson.default_options is deprecated\nUse MultiJson.load_options or MultiJson.dump_options instead"

    load_options
  end

  %w[cached_options reset_cached_options!].each do |method_name|
    define_method method_name do |*|
      Kernel.warn "MultiJson.#{method_name} method is deprecated and no longer used."
    end
  end

  ALIASES = {"jrjackson" => "jr_jackson"}.freeze

  REQUIREMENT_MAP = {
    oj: "oj",
    yajl: "yajl",
    jr_jackson: "jrjackson",
    json_gem: "json/ext",
    gson: "gson",
    json_pure: "json/pure"
  }

  # The default adapter based on what you currently
  # have loaded and installed.
  def default_adapter
    adapter = loaded_adapter || installable_adapter
    return adapter if adapter

    Kernel.warn "[WARNING] MultiJson is using the default adapter (ok_json). We recommend loading a different JSON library to improve performance."

    :ok_json
  end

  alias_method :default_engine, :default_adapter

  # Get the current adapter class.
  def adapter
    return @adapter if defined?(@adapter) && @adapter

    use nil # load default adapter

    @adapter
  end
  alias_method :engine, :adapter

  # Set the JSON parser utilizing a symbol, string, or class.
  # Supported by default are:
  #
  # * <tt>:oj</tt>
  # * <tt>:json_gem</tt>
  # * <tt>:json_pure</tt>
  # * <tt>:ok_json</tt>
  # * <tt>:yajl</tt>
  # * <tt>:gson</tt> (JRuby only)
  # * <tt>:jr_jackson</tt> (JRuby only)
  def use(new_adapter)
    @adapter = load_adapter(new_adapter)
  ensure
    OptionsCache.reset
  end
  alias_method :adapter=, :use
  alias_method :engine=, :use

  def load_adapter(new_adapter)
    case new_adapter
    when String, Symbol
      load_adapter_from_string_name new_adapter.to_s
    when NilClass, FalseClass
      load_adapter default_adapter
    when Class, Module
      new_adapter
    else
      raise ::LoadError, new_adapter
    end
  rescue ::LoadError => e
    raise AdapterError.build(e)
  end

  # Decode a JSON string into Ruby.
  #
  # <b>Options</b>
  #
  # <tt>:symbolize_keys</tt> :: If true, will use symbols instead of strings for the keys.
  # <tt>:adapter</tt> :: If set, the selected adapter will be used for this call.
  def load(string, options = {})
    adapter = current_adapter(options)
    begin
      adapter.load(string, options)
    rescue adapter::ParseError => e
      raise ParseError.build(e, string)
    end
  end
  alias_method :decode, :load

  def current_adapter(options = {})
    if (new_adapter = options[:adapter])
      load_adapter(new_adapter)
    else
      adapter
    end
  end

  # Encodes a Ruby object as JSON.
  def dump(object, options = {})
    current_adapter(options).dump(object, options)
  end
  alias_method :encode, :dump

  # Reads an RFC 7464 JSON Text Sequence from a string or byte-oriented IO.
  #
  # With a block each parsed record is yielded as soon as it is complete, so
  # sequences appended over time can be consumed without holding the whole
  # stream in memory. Without a block an Enumerator is returned.
  #
  # <b>Options</b>
  #
  # <tt>:on_bad_record</tt> :: +:raise+ (default), +:skip+, or a callable
  #   receiving a MultiJson::MalformedRecord describing the bad record and its
  #   raw byte range.
  # <tt>:max_record_bytes</tt> :: optional limit on a single record's payload
  #   size; larger records are reported as MultiJson::RecordTooLarge.
  def load_sequence(source, options = {})
    reader = SequenceReader.new(source, **options)
    return reader.each unless block_given?

    reader.each { |value| yield value }
  end
  alias_method :decode_sequence, :load_sequence

  # Dumps an enumerable of objects as an RFC 7464 JSON Text Sequence.
  #
  # Returns the framed sequence string. Use SequenceWriter with an IO to stream
  # records instead of buffering them.
  def dump_sequence(objects, options = {})
    writer = SequenceWriter.new(**options)
    objects.each { |object| writer.write(object) }
    writer.string
  end
  alias_method :encode_sequence, :dump_sequence

  # Streams an RFC 7464 JSON Text Sequence into the given IO.
  #
  # Pass an enumerable, or pass a block that receives the SequenceWriter for
  # incremental writes.
  def write_sequence(io, objects = nil, options = {})
    writer = SequenceWriter.new(io, **options)
    if block_given?
      yield writer
    else
      raise ArgumentError, "an enumerable or a block is required" if objects.nil?

      objects.each { |object| writer.write(object) }
    end
    writer.flush
    io
  end

  #  Executes passed block using specified adapter.
  def with_adapter(new_adapter)
    old_adapter = adapter
    self.adapter = new_adapter
    yield
  ensure
    self.adapter = old_adapter
  end
  alias_method :with_engine, :with_adapter

  private

  # Checks for already loaded adapters and returns the first match
  def loaded_adapter
    return :oj if defined?(::Oj)
    return :yajl if defined?(::Yajl)
    return :jr_jackson if defined?(::JrJackson)
    return :json_gem if defined?(::JSON::Ext::Parser)
    return :gson if defined?(::Gson)

    nil
  end

  # Attempts to load and return the first installable adapter
  def installable_adapter
    REQUIREMENT_MAP.each do |adapter, library|
      require library
      return adapter
    rescue ::LoadError
      next
    end
    nil
  end

  def load_adapter_from_string_name(name)
    name = ALIASES.fetch(name, name)
    require "multi_json/adapters/#{name.downcase}"
    klass_name = name.to_s.split("_").map(&:capitalize) * ""
    MultiJson::Adapters.const_get(klass_name)
  end
end
