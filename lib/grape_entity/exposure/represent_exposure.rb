# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class RepresentExposure < Base
        attr_reader :using_class_name, :subexposure

        def setup(using_class_name, subexposure)
          @using_class = nil
          @using_class_name = using_class_name
          @subexposure = subexposure
        end

        def dup_args
          [*super, using_class_name, subexposure]
        end

        def ==(other)
          super &&
            @using_class_name == other.using_class_name &&
            @subexposure == other.subexposure
        end

        def value(entity, options)
          new_options = options.for_nesting(key(entity))
          associated = @subexposure.value(entity, options)

          register_associations(associated, options)

          using_class.represent(associated, new_options)
        end

        def valid?(entity)
          @subexposure.valid? entity
        end

        def using_class
          @using_class ||= if @using_class_name.respond_to? :constantize
                             @using_class_name.constantize
                           else
                             @using_class_name
                           end
        end

        private

        # Bring every selected nested object, and a versioned association
        # collection itself, into the consistency session. This must happen
        # after the association was read but before (and while) it is
        # serialized, so a change made exactly in that gap is detected.
        #
        # A collection may be a plain Array or any object that exposes its
        # members through +to_ary+ (e.g. an association proxy).
        def register_associations(associated, options)
          session = options[:consistency_session]
          return unless session

          if associated.respond_to?(:to_ary)
            session.register(associated, reread: is_reread)
            associated.to_ary.each { |item| session.register(item, reread: is_reread) }
          else
            session.register(associated, reread: is_reread)
          end
        end

        def using_options_for(options)
          options.for_nesting(key)
        end
      end
    end
  end
end
