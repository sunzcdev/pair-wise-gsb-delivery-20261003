require "singleton"
require "multi_json/options"

module MultiJson
  class Adapter
    extend Options
    include Singleton

    class << self
      def defaults(action, value)
        metaclass = class << self; self; end

        metaclass.instance_eval do
          define_method(:"default_#{action}_options") { value }
        end
      end

      def load(string, options = {})
        string = string.read if string.respond_to?(:read)
        raise self::ParseError if blank?(string)

        merged = cached_load_options(options)
        if merged[:preserve_numbers] && !supports?(:preserve_numbers)
          raise UnsupportedOptionError.build(self, :preserve_numbers, unsupported_numbers_message)
        end

        instance.load(string, merged)
      end

      def dump(object, options = {})
        instance.dump(object, cached_dump_options(options))
      end

      # Whether this backend can implement an option without breaking its
      # contract. Subclasses override this for options they actually support.
      def supports?(option)
        case option
        when :preserve_numbers then false
        else true
        end
      end

      private

      def unsupported_numbers_message
        "Use the :json_gem, :json_pure or :ok_json adapter, or parse without :preserve_numbers"
      end

      def blank?(input)
        input.nil? || /\A\s*\z/.match?(input)
      rescue ArgumentError # invalid byte sequence in UTF-8
        false
      end

      def cached_dump_options(options)
        OptionsCache.fetch(:dump, options) do
          dump_options(options).merge(MultiJson.dump_options(options)).merge!(options)
        end
      end

      def cached_load_options(options)
        OptionsCache.fetch(:load, options) do
          load_options(options).merge(MultiJson.load_options(options)).merge!(options)
        end
      end
    end
  end
end
