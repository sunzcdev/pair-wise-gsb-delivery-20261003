# encoding: utf-8
# frozen_string_literal: true
require 'warden/session_serializer'

module Warden
  # This class is yielded inside Warden::Manager. If you have a plugin and want to
  # add more configuration to warden, you just need to extend this class.
  class Config < Hash
    # Creates an accessor that simply sets and reads a key in the hash:
    #
    #   class Config < Hash
    #     hash_accessor :failure_app
    #   end
    #
    #   config = Config.new
    #   config.failure_app = Foo
    #   config[:failure_app] #=> Foo
    #
    #   config[:failure_app] = Bar
    #   config.failure_app #=> Bar
    #
    def self.hash_accessor(*names) #:nodoc:
      names.each do |name|
        class_eval <<-METHOD, __FILE__, __LINE__ + 1
          def #{name}
            self[:#{name}]
          end

          def #{name}=(value)
            self[:#{name}] = value
          end
        METHOD
      end
    end

    hash_accessor :failure_app, :default_scope, :intercept_401

    def initialize(other={})
      merge!(other)
      self[:default_scope]      ||= :default
      self[:scope_defaults]     ||= {}
      self[:default_strategies] ||= {}
      self[:intercept_401] = true unless key?(:intercept_401)
      self[:session_serializer_class] ||= Class.new(Warden::SessionSerializer)
    end

    def initialize_copy(other)
      super
      deep_dup(:scope_defaults, other)
      deep_dup(:default_strategies, other)

      # Give every Proxy its own serializer subclass. It is created by
      # subclassing the Manager's serializer instead of reusing it, so the
      # blocks captured when the request starts cannot be replaced by
      # configuring another (or the same) Manager afterwards.
      if (source = other[:session_serializer_class])
        serializer_class = Class.new(source)
        # Pin every method defined directly on the Manager's serializer so
        # later calls to serialize_into_session/serialize_from_session cannot
        # change methods this Proxy already captured.
        source.instance_methods(false).each do |name|
          serializer_class.send(:define_method, name, source.instance_method(name))
        end
        self[:session_serializer_class] = serializer_class
      end
    end

    # Do not raise an error if a missing strategy is given.
    # :api: plugin
    def silence_missing_strategies!
      self[:silence_missing_strategies] = true
    end

    def silence_missing_strategies? #:nodoc:
      !!self[:silence_missing_strategies]
    end

    # Set the default strategies to use.
    # :api: public
    def default_strategies(*strategies)
      opts  = Hash === strategies.last ? strategies.pop : {}
      hash  = self[:default_strategies]
      scope = opts[:scope] || :_all

      hash[scope] = strategies.flatten unless strategies.empty?
      hash[scope] || hash[:_all] || []
    end

    # A short hand way to set up a particular scope
    # :api: public
    def scope_defaults(scope, opts = {})
      if strategies = opts.delete(:strategies)
        default_strategies(strategies, :scope => scope)
      end

      if opts.empty?
        self[:scope_defaults][scope] || {}
      else
        self[:scope_defaults][scope] ||= {}
        self[:scope_defaults][scope].merge!(opts)
      end
    end

    # Quick accessor to strategies from manager
    # :api: public
    def strategies
      Warden::Strategies
    end

    # Prepares the user to serialize into the session for this Manager only.
    #
    # Unlike Warden::Manager.serialize_into_session, which defines the method
    # globally on Warden::SessionSerializer, the block given here is defined on
    # the Manager's private serializer subclass. Other Managers keep their own
    # behavior and already running proxies are unaffected.
    #
    # Pass a scope symbol to handle a single scope. Lookup order when storing:
    #   1. this Manager's scope serializer
    #   2. this Manager's default serializer
    #   3. the class level Warden::Manager scope serializer
    #   4. the class level Warden::Manager default serializer
    #   5. the user itself
    #
    # :api: public
    def serialize_into_session(scope = nil, &block)
      method_name = scope.nil? ? :serialize : "#{scope}_serialize"
      session_serializer_class.send(:define_method, method_name, &block)
    end

    # Reconstitutes the user from the session for this Manager only.
    #
    # The block is defined on the Manager's private serializer subclass instead
    # of globally, so Managers never redefine a method another application is
    # already using. Pass a scope symbol to handle a single scope. The lookup
    # order when fetching mirrors serialize_into_session:
    #   1. this Manager's scope deserializer
    #   2. this Manager's default deserializer
    #   3. the class level Warden::Manager scope deserializer
    #   4. the class level Warden::Manager default deserializer
    #   5. the stored key itself
    #
    # :api: public
    def serialize_from_session(scope = nil, &block)
      method_name = scope.nil? ? :deserialize : "#{scope}_deserialize"
      if session_serializer_class.instance_methods(false).include?(method_name)
        session_serializer_class.send(:remove_method, method_name)
      end
      session_serializer_class.send(:define_method, method_name, &block)
    end

    # The private serializer subclass owned by this configuration.
    # :api: private
    def session_serializer_class
      self[:session_serializer_class]
    end

  protected

    def deep_dup(key, other)
      self[key] = hash = other[key].dup
      hash.each { |k, v| hash[k] = v.dup }
    end
  end
end
