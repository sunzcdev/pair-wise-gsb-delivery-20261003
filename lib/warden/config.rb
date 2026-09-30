# encoding: utf-8
# frozen_string_literal: true

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
      self[:serializers]        ||= { :store => {}, :fetch => {} }
      self[:intercept_401] = true unless key?(:intercept_401)
    end

    def initialize_copy(other)
      super
      deep_dup(:scope_defaults, other)
      deep_dup(:default_strategies, other)
      deep_dup(:serializers, other)
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
    # Unlike Warden::Manager.serialize_into_session, the given block is stored on
    # this configuration and does not modify any other Manager mounted in the
    # same process.
    #
    # Pass a scope symbol to register a scope specific serializer. A scope
    # specific block registered on this Manager takes precedence over the
    # default block of this Manager, which in turn takes precedence over the
    # class level Warden::Manager serializers.
    #
    # Example:
    #   Warden::Manager.new(app) do |config|
    #     config.serialize_into_session { |user| user.id }
    #     config.serialize_into_session(:admin) { |user| user.admin_id }
    #   end
    #
    # :api: public
    def serialize_into_session(scope = nil, &block)
      self[:serializers][:store][scope] = block
    end

    # Reconstitutes the user from the session for this Manager only.
    # Unlike Warden::Manager.serialize_from_session, the given block is stored
    # on this configuration and does not modify any other Manager mounted in
    # the same process.
    #
    # Pass a scope symbol to register a scope specific deserializer. A scope
    # specific block registered on this Manager takes precedence over the
    # default block of this Manager, which in turn takes precedence over the
    # class level Warden::Manager deserializers.
    #
    # Example:
    #   Warden::Manager.new(app) do |config|
    #     config.serialize_from_session { |id| User.get(id) }
    #     config.serialize_from_session(:admin) { |id| AdminUser.get(id) }
    #   end
    #
    # :api: public
    def serialize_from_session(scope = nil, &block)
      self[:serializers][:fetch][scope] = block
    end

  protected

    def deep_dup(key, other)
      self[key] = hash = other[key].dup
      hash.each { |k, v| hash[k] = v.dup }
    end
  end
end
