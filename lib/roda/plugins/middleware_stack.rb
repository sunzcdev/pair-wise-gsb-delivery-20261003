# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The middleware_stack plugin adds methods to remove middleware
    # from the middleware stack, insert new middleware at specific
    # positions in the middleware stack, and give middleware names so
    # that their order can be declared with constraints instead of relying
    # on the position of +use+ calls in the file.
    #
    #   plugin :middleware_stack
    #
    #   # Remove csrf middleware
    #   middleware_stack.remove{|m, *args| m == Rack::Csrf}
    #
    #   # Insert csrf middleware
    #   middleware_stack.before{|m, *args| m == Rack::CommonLogger}.use(Rack::Csrf, raise: true)
    #   middleware_stack.after{|m, *args| m == Rack::CommonLogger}.use(Rack::Csrf, raise: true)
    #
    # == Named middleware
    #
    # Middleware that needs to be in a particular order can be given a name,
    # and the order declared with +before+ and +after+:
    #
    #   middleware_stack.named(:logger).use(Rack::CommonLogger)
    #
    #   middleware_stack.named(:session).
    #     use(Rack::Session::Cookie, secret: '...').
    #     after(:logger)
    #
    #   middleware_stack.named(:csrf).
    #     use(Rack::Csrf, raise: true).
    #     after(:logger).
    #     before(:router)
    #
    #   middleware_stack.named(:router).use(MyRouter)
    #
    # The same middleware class can be added more than once under different
    # names, which is useful when the same middleware needs different
    # arguments in different positions:
    #
    #   middleware_stack.named(:cache_public).use(Cache, ttl: 60)
    #   middleware_stack.named(:cache_private).use(Cache, ttl: 5).after(:session)
    #
    # Middleware can also be added relative to a named entry without giving it
    # a name itself, either with <tt>Stack#before</tt>/<tt>Stack#after</tt> or
    # with <tt>Stack#use</tt>:
    #
    #   middleware_stack.before(:router).use(Rack::Head)
    #   middleware_stack.use(Rack::Head).before(:router)
    #   middleware_stack.after(:session).use(MyMiddleware)
    #
    # Middleware added with +Stack#use+ can also be named after the fact:
    #
    #   middleware_stack.use(Rack::CommonLogger).named(:logger)
    #
    # Named entries and constraints are collected while the class is being
    # configured, and the order is resolved when the rack application is
    # built.  The order is a stable topological sort: middleware that is not
    # referenced by any constraint (for example, middleware added with a
    # plain Roda.use) keeps its original position relative to the other
    # unconstrained middleware.
    #
    # The block forms of +before+ and +after+ search the middleware in the
    # order it was added and yield the same arguments as the block form of
    # +remove+.  They insert the middleware at a physical position in the
    # stack; use the named forms above if the position needs to interact with
    # ordering constraints.
    #
    # == Errors
    #
    # A RodaError is raised when building the rack application if:
    #
    # * a constraint references a name that was never declared, or a name for
    #   which no middleware has been added (for example, a misspelled name),
    # * the constraints form a cycle, or
    # * the same name is declared twice in the same class.
    #
    # All of these errors include the file and line number where the
    # offending constraint or declaration was made.  Constraints may refer to
    # names that are declared later in the file, so these errors are raised
    # when the application is built, not when the constraint is declared.
    #
    # == Inspecting and removing named middleware
    #
    # The resolved order can be inspected without making a request:
    #
    #   middleware_stack.order
    #   # => [:logger, :session, :csrf, Rack::Head, :router]
    #
    # Named entries are represented by their name, and unnamed entries by the
    # middleware class/object used for the entry.  Named middleware can be
    # removed by name:
    #
    #   middleware_stack.remove(:csrf)
    #
    # Removing a named middleware also removes any constraints that refer to
    # it, so the remaining middleware can still be built.
    module MiddlewareStack
      # An ordering constraint.  +subject+ is either the name of the middleware
      # the constraint applies to, or the middleware entry itself for middleware
      # added without a name via <tt>use(...).before(name)</tt> or
      # <tt>before(name).use(...)</tt>.  +type+ is either +:before+ or +:after+,
      # and +target+ is the name of the middleware the subject must be placed
      # before or after.  +location+ is a string with the file and line where
      # the constraint was declared.
      Constraint = Struct.new(:subject, :type, :target, :location)

      # Format a caller location as a string suitable for error messages.
      def self.format_location(loc)
        loc ? "#{loc.path}:#{loc.lineno}" : "(unknown location)"
      end

      # Represents a position in the middleware stack based on the position of
      # existing middleware in the order it was added and a block.
      class StackPosition
        def initialize(app, middleware, position)
          @app = app
          @middleware = middleware
          @position = position
        end

        # Insert a new middleware into the current position in the middleware stack.
        # Increments the position so that calling this multiple times adds later
        # middleware after earlier middleware, similar to how +Roda.use+ works.
        def use(*args, &block)
          @app.send(:check_middleware_stack_mutable!)
          @middleware.insert(@position, [args, block])
          @app.send(:invalidate_middleware_stack)
          @position += 1
          nil
        end
        ruby2_keywords(:use) if respond_to?(:ruby2_keywords, true)
      end

      # Represents middleware that has already been added to the stack, and can
      # be given a name and/or ordering constraints.  Returned by Stack#use.
      class MiddlewarePosition
        def initialize(stack, entry)
          @stack = stack
          @entry = entry
        end

        # Give this middleware a name, so ordering constraints for other
        # middleware can refer to it.  Returns self so constraints can be
        # chained.
        def named(name)
          loc = caller_locations(1, 1)[0]
          @stack.send(:name_entry, @entry, name, loc)
          self
        end

        # Require this middleware to come before all of the given named
        # middleware entries.  Returns self.
        def before(*names)
          loc = caller_locations(1, 1)[0]
          @stack.send(:add_constraint, @entry, :before, names, loc)
          self
        end

        # Require this middleware to come after all of the given named
        # middleware entries.  Returns self.
        def after(*names)
          loc = caller_locations(1, 1)[0]
          @stack.send(:add_constraint, @entry, :after, names, loc)
          self
        end
      end

      # Represents a named middleware entry in the application's middleware
      # stack.  Returned by Stack#named.  The middleware for the entry can be
      # added with #use, and ordering constraints can be added before or after
      # the middleware is added, as they can be chained.
      class NamedStackPosition
        def initialize(stack, name)
          @stack = stack
          @name = name
        end

        # The name of this middleware entry.
        attr_reader :name

        # Add middleware for this named entry.  Returns self so ordering
        # constraints can be chained.  The rack application is rebuilt on
        # next access.
        def use(*args, &block)
          @stack.send(:use_named, @name, args, block)
          self
        end
        ruby2_keywords(:use) if respond_to?(:ruby2_keywords, true)

        # Require this middleware to come before all of the given named
        # middleware entries.  Returns self.
        def before(*names)
          loc = caller_locations(1, 1)[0]
          @stack.send(:add_constraint, @name, :before, names, loc)
          self
        end

        # Require this middleware to come after all of the given named
        # middleware entries.  Returns self.
        def after(*names)
          loc = caller_locations(1, 1)[0]
          @stack.send(:add_constraint, @name, :after, names, loc)
          self
        end
      end

      # Represents unnamed middleware that is added relative to a named
      # middleware entry.  Returned by Stack#before and Stack#after when called
      # with a middleware name instead of a block.
      class RelativeStackPosition
        def initialize(stack, target, type)
          @stack = stack
          @target = target
          @type = type
        end

        # Add middleware that must be placed before or after the target named
        # middleware, depending on how this object was created.  The rack
        # application is rebuilt on next access.
        def use(*args, &block)
          loc = caller_locations(1, 1)[0]
          @stack.send(:use_relative, @target, @type, args, block, loc)
          nil
        end
        ruby2_keywords(:use) if respond_to?(:ruby2_keywords, true)
      end

      # Represents the applications middleware as a stack, allowing for easily
      # removing middleware, finding places to insert new middleware, and
      # naming middleware so ordering constraints can be declared.
      class Stack
        def initialize(app)
          @app = app
        end

        # If called with a block, return a StackPosition representing the position
        # after the middleware where the block returns true. Yields the middleware
        # and any middleware arguments given, but not the middleware block.
        # If the block never returns true, returns a StackPosition that will insert
        # new middleware at the end of the stack.
        #
        # If called with the name of a named middleware, return a RelativeStackPosition
        # that can be used to add middleware that must come after that middleware.
        def after(name=nil, &block)
          if name
            raise RodaError, "cannot provide both a middleware name and a block to middleware_stack.after" if block
            RelativeStackPosition.new(self, name, :after)
          else
            handle(1, &block)
          end
        end

        # If called with a block, return a StackPosition representing the position
        # before the middleware where the block returns true. Yields the middleware
        # and any middleware arguments given, but not the middleware block.
        # If the block never returns true, returns a StackPosition that will insert
        # new middleware at the end of the stack.
        #
        # If called with the name of a named middleware, return a RelativeStackPosition
        # that can be used to add middleware that must come before that middleware.
        def before(name=nil, &block)
          if name
            raise RodaError, "cannot provide both a middleware name and a block to middleware_stack.before" if block
            RelativeStackPosition.new(self, name, :before)
          else
            handle(0, &block)
          end
        end

        # Add middleware at the end of the stack.  Returns a MiddlewarePosition
        # that can be used to name the middleware and/or add ordering
        # constraints for it.
        def use(*args, &block)
          MiddlewarePosition.new(self, @app.send(:add_middleware_entry, args, block))
        end
        ruby2_keywords(:use) if respond_to?(:ruby2_keywords, true)

        # Return a NamedStackPosition for the given middleware name.  The name
        # must not already have been declared in the same class (a subclass can
        # re-declare a name inherited from its parent, which overrides it).
        def named(name)
          loc = caller_locations(1, 1)[0]
          @app.send(:register_middleware_name, name, loc)
          NamedStackPosition.new(self, name)
        end

        # If called with a middleware name, remove the middleware with that name,
        # along with any ordering constraints that reference it.
        #
        # If called with a block, removes any middleware where the block returns true.
        # Yields the middleware and any middleware arguments given, but not the
        # middleware block.
        def remove(name=nil, &block)
          if name
            raise RodaError, "cannot provide both a middleware name and a block to middleware_stack.remove" if block
            @app.send(:remove_named_middleware, name)
          elsif block
            @app.send(:remove_middleware_entries, &block)
          else
            raise RodaError, "middleware_stack.remove requires a middleware name or a block"
          end
          nil
        end

        # Return the resolved middleware order as an array.  Named middleware
        # entries are represented by their name, unnamed middleware entries by
        # the middleware class/object used for the entry.  Raises a RodaError if
        # the ordering constraints are invalid, using the same checks performed
        # when building the rack application.
        def order
          @app.send(:resolve_middleware_order).map do |entry|
            @app.send(:middleware_name_for_entry, entry) || entry[0][0]
          end
        end

        private

        # Add middleware for a named entry.
        def use_named(name, args, block)
          @app.send(:add_named_middleware, name, args, block)
        end

        # Add unnamed middleware relative to a named entry.
        def use_relative(target, type, args, block, loc)
          entry = @app.send(:add_middleware_entry, args, block)
          add_constraint(entry, type, [target], loc)
        end

        # Name an existing middleware entry.
        def name_entry(entry, name, loc)
          @app.send(:register_middleware_name, name, loc)
          @app.send(:set_named_middleware, name, entry)
        end

        # Add before/after constraints for the given subject (a middleware name
        # or entry) relative to the given target names.
        def add_constraint(subject, type, targets, loc)
          targets.each do |target|
            @app.send(:add_middleware_constraint, subject, type, target, loc)
          end
        end

        # Internals of the block forms of before and after.
        def handle(offset)
          raise RodaError, "middleware_stack.before/after requires a block or a middleware name" unless block_given?

          middleware = @app.send(:middleware_entries)
          middleware.each_with_index do |(m, _), i|
            if yield(*m)
              return StackPosition.new(@app, middleware, i+offset)
            end
          end

          StackPosition.new(@app, middleware, middleware.length)
        end
      end

      module ClassMethods
        # Return a new Stack that allows removing middleware, inserting
        # middleware at specific places in the stack, and naming middleware.
        def middleware_stack
          Stack.new(self)
        end

        # When inheriting with inherit_middleware disabled, the middleware stack
        # is not inherited, so discard the middleware names and ordering
        # constraints as well, as they refer to middleware that is not present.
        def inherited(subclass)
          super
          unless subclass.inherit_middleware
            opts = subclass.opts
            opts.delete(:middleware_stack_name_locations)
            opts.delete(:middleware_stack_names)
            opts.delete(:middleware_stack_constraints)
          end
        end

        # Clear the middleware stack, including any middleware names and
        # ordering constraints.
        def clear_middleware!
          super
          opts.delete(:middleware_stack_name_locations)
          opts.delete(:middleware_stack_names)
          opts.delete(:middleware_stack_constraints)
          @middleware_stack_declared_names.clear if @middleware_stack_declared_names
          nil
        end

        private

        # The middleware entries, in the order they were added.
        def middleware_entries
          @middleware
        end

        # The order middleware should be used in when building the rack
        # application, based on the declared ordering constraints.
        def middleware_for_rack_app
          resolve_middleware_order
        end

        # Invalidate the built rack application, so it is rebuilt using the
        # current middleware entries and constraints on next access.
        def invalidate_middleware_stack
          @app = nil
          nil
        end

        # Raise a RodaError if the class has been frozen, as middleware names
        # and ordering constraints cannot be modified after freezing.
        def check_middleware_stack_mutable!
          raise RodaError, "Cannot modify middleware stack of a frozen Roda class" if frozen?
          nil
        end

        # Add a middleware entry to the stack and return it.
        def add_middleware_entry(args, block)
          check_middleware_stack_mutable!
          entry = [args, block].freeze
          @middleware << entry
          invalidate_middleware_stack
          entry
        end

        # Declare a middleware name in the current class.  A name may only be
        # declared once per class, though a subclass may re-declare a name
        # inherited from its parent.
        def register_middleware_name(name, loc)
          check_middleware_stack_mutable!
          declared = (@middleware_stack_declared_names ||= {})
          if declared.key?(name)
            raise RodaError, "middleware name #{name.inspect} is already declared at #{declared[name]} (redeclared at #{MiddlewareStack.format_location(loc)})"
          end
          declared[name] = MiddlewareStack.format_location(loc)

          (opts[:middleware_stack_name_locations] ||= {})[name] = MiddlewareStack.format_location(loc)
          invalidate_middleware_stack
          nil
        end

        # Associate a middleware entry with an already declared name.
        def set_named_middleware(name, entry)
          check_middleware_stack_mutable!
          (opts[:middleware_stack_names] ||= {})[name] = entry
          invalidate_middleware_stack
          nil
        end

        # Add middleware for a named entry, replacing any middleware previously
        # added for the same name (which can happen when a subclass re-declares
        # a name inherited from its parent).
        def add_named_middleware(name, args, block)
          check_middleware_stack_mutable!
          locations = opts[:middleware_stack_name_locations] ||= {}
          names = opts[:middleware_stack_names] ||= {}

          unless locations.key?(name)
            raise RodaError, "middleware name #{name.inspect} was not declared via middleware_stack.named"
          end

          entry = [args, block].freeze
          if old_entry = names[name]
            if i = @middleware.index{|m| m.equal?(old_entry)}
              @middleware[i] = entry
            else
              @middleware << entry
            end
          else
            @middleware << entry
          end

          names[name] = entry
          invalidate_middleware_stack
          nil
        end

        # Add an ordering constraint.  +subject+ is either a middleware name or
        # a middleware entry, +target+ must be a middleware name.
        def add_middleware_constraint(subject, type, target, loc)
          check_middleware_stack_mutable!
          unless type == :before || type == :after
            raise RodaError, "invalid middleware ordering constraint type: #{type.inspect}"
          end

          (opts[:middleware_stack_constraints] ||= []) << Constraint.new(subject, type, target, MiddlewareStack.format_location(loc)).freeze
          invalidate_middleware_stack
          nil
        end

        # Remove the middleware with the given name, along with constraints
        # referencing it.
        def remove_named_middleware(name)
          check_middleware_stack_mutable!
          if names = opts[:middleware_stack_names]
            if entry = names.delete(name)
              @middleware.delete_if{|m| m.equal?(entry)}
            end
          end

          if locations = opts[:middleware_stack_name_locations]
            locations.delete(name)
          end

          if constraints = opts[:middleware_stack_constraints]
            constraints.delete_if do |c|
              c.target == name || (c.subject.is_a?(Symbol) || c.subject.is_a?(String)) && c.subject == name
            end
          end

          if declared = @middleware_stack_declared_names
            declared.delete(name)
          end

          invalidate_middleware_stack
          nil
        end

        # Remove middleware matching the block, along with constraints
        # referencing the removed middleware.
        def remove_middleware_entries
          check_middleware_stack_mutable!
          removed = {}.compare_by_identity
          @middleware.delete_if do |entry|
            if yield(*entry[0])
              removed[entry] = true
              true
            end
          end

          removed_names = {}
          if names = opts[:middleware_stack_names]
            names.delete_if do |name, entry|
              entry && removed[entry] && (removed_names[name] = true)
            end
          end

          if locations = opts[:middleware_stack_name_locations]
            locations.delete_if{|name, _| removed_names.key?(name)}
          end

          if declared = @middleware_stack_declared_names
            declared.delete_if{|name, _| removed_names.key?(name)}
          end

          if constraints = opts[:middleware_stack_constraints]
            constraints.delete_if do |c|
              subject_removed = if c.subject.is_a?(Symbol) || c.subject.is_a?(String)
                removed_names.key?(c.subject)
              else
                removed[c.subject]
              end
              subject_removed || removed_names.key?(c.target)
            end
          end

          invalidate_middleware_stack
          nil
        end

        # Return the name for the given middleware entry, if any.
        def middleware_name_for_entry(entry)
          if names = opts[:middleware_stack_names]
            names.each do |name, e|
              return name if e.equal?(entry)
            end
          end
          nil
        end

        # The name to show for a middleware entry in error messages.
        def middleware_entry_name(entry)
          middleware_name_for_entry(entry) || entry[0][0]
        end

        # Resolve the middleware order using a stable topological sort.  If no
        # middleware names or constraints have been declared, return the
        # middleware in the order it was added, preserving historical behavior.
        def resolve_middleware_order
          entries = @middleware
          names = opts[:middleware_stack_names]
          constraints = opts[:middleware_stack_constraints]

          if (names.nil? || names.empty?) && (constraints.nil? || constraints.empty?)
            return entries
          end

          return entries if entries.empty?

          entry_index = {}.compare_by_identity
          entries.each_with_index{|entry, i| entry_index[entry] = i}

          # Resolve all constraints to pairs of entries.  Each edge is
          # [from, to, constraint], meaning from must come before to.
          edges = []
          if constraints
            constraints.each do |constraint|
              subject = constraint_subject_entry(constraint)
              target = constraint_target_entry(constraint)

              # Ignore constraints involving middleware that is no longer in
              # the stack, such as when it was removed directly from
              # @middleware without going through middleware_stack.
              next unless entry_index[subject] && entry_index[target]

              if constraint.type == :before
                edges << [subject, target, constraint]
              else
                edges << [target, subject, constraint]
              end
            end
          end

          successors = {}.compare_by_identity
          indegree = {}.compare_by_identity
          entries.each do |entry|
            successors[entry] = []
            indegree[entry] = 0
          end

          edges.each do |from, to, _|
            successors[from] << to
            indegree[to] += 1
          end

          # Kahn's algorithm, always processing the available entry that was
          # added earliest.  This gives a stable order, so middleware not
          # involved in a constraint keeps its original position.
          available = []
          entries.each_with_index{|entry, i| available << i if indegree[entry] == 0}

          ordered = []
          until available.empty?
            entry = entries[available.shift]
            ordered << entry

            successors[entry].each do |to|
              indegree[to] -= 1
              if indegree[to] == 0
                available << entry_index[to]
              end
            end
            available.sort!
          end

          return ordered if ordered.length == entries.length

          raise RodaError, middleware_ordering_cycle_message(entries, ordered, edges)
        end

        # Return the middleware entry for a constraint's subject.  The subject
        # can be a middleware name or a middleware entry.
        def constraint_subject_entry(constraint)
          subject = constraint.subject
          return subject unless subject.is_a?(Symbol) || subject.is_a?(String)
          constraint_entry_for_name(subject, constraint, "references")
        end

        # Return the middleware entry for a constraint's target name.
        def constraint_target_entry(constraint)
          constraint_entry_for_name(constraint.target, constraint, "references")
        end

        # Return the middleware entry for the given name, raising a RodaError
        # pointing at the declaration location of the constraint if the name
        # was not declared or no middleware was added for it.
        def constraint_entry_for_name(name, constraint, verb)
          locations = opts[:middleware_stack_name_locations] || {}
          names = opts[:middleware_stack_names] || {}

          unless locations.key?(name)
            raise RodaError, "middleware ordering constraint declared at #{constraint.location} #{verb} undefined middleware name #{name.inspect}"
          end

          if entry = names[name]
            entry
          else
            raise RodaError, "middleware ordering constraint declared at #{constraint.location} #{verb} middleware name #{name.inspect}, but no middleware was added for it (name declared at #{locations[name]})"
          end
        end

        # Build an error message for a middleware ordering cycle, including the
        # declaration location for every constraint in the cycle.
        def middleware_ordering_cycle_message(entries, ordered, edges)
          done = {}.compare_by_identity
          ordered.each{|entry| done[entry] = true}
          remaining = entries.reject{|entry| done[entry]}
          remaining_set = {}.compare_by_identity
          remaining.each{|entry| remaining_set[entry] = true}

          # Every remaining entry has a predecessor among the remaining
          # entries, so walking predecessors must eventually revisit an entry,
          # which identifies a cycle.
          predecessors = {}.compare_by_identity
          edge_for = {}.compare_by_identity
          edges.each do |from, to, constraint|
            (predecessors[to] ||= []) << from
            (edge_for[to] ||= {}.compare_by_identity)[from] = constraint
          end

          chain = []
          position = {}.compare_by_identity
          current = remaining.first
          until position.key?(current)
            position[current] = chain.length
            chain << current
            current = predecessors[current].find{|from| remaining_set[from]}
          end

          cycle = chain[position[current]..-1]
          descriptions = cycle.each_with_index.map do |to, i|
            from = cycle[(i + 1) % cycle.length]
            constraint = edge_for[to][from]
            "#{middleware_entry_name(from).inspect} must come before #{middleware_entry_name(to).inspect} (constraint declared at #{constraint.location})"
          end

          "middleware ordering constraints form a cycle: #{descriptions.join(', ')}"
        end
      end
    end

    register_plugin(:middleware_stack, MiddlewareStack)
  end
end
