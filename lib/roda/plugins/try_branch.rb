# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The try_branch plugin adds a +try_branch+ request method, which
    # tentatively runs a routing branch, rolling back all changes the
    # branch made if the branch does not commit to handling the request:
    #
    #   plugin :try_branch
    #
    #   route do |r|
    #     r.try_branch "foo", String do |seg|
    #       response['X-Tentative'] = seg
    #       r.abandon_branch unless seg == 'wanted'
    #
    #       r.is do
    #         "handled /foo/#{seg}"
    #       end
    #     end
    #
    #     r.on "foo", String do |seg|
    #       "fallback /foo/#{seg}"
    #     end
    #   end
    #
    # +try_branch+ takes the same matcher arguments as +on+.  If the
    # matchers do not match, the block is not called and nothing changes.
    # If the matchers match, the block is called with the captures, and
    # runs speculatively:
    #
    # Commit :: If the block handles the request by halting (a terminal
    #           match such as +r.is+/+r.get+, or +r.halt+, +r.redirect+,
    #           or +r.run+), all changes the branch made are committed,
    #           and the response the branch produced is returned.
    # Rollback :: If the block calls +r.abandon_branch+, or returns
    #             normally without handling the request, all state the
    #             branch modified is rolled back to what it was before
    #             +try_branch+ was called, and routing continues after
    #             the +try_branch+ call as if the branch never ran.
    #
    # The state rolled back includes:
    #
    # * The remaining path (path cursor)
    # * The matcher captures
    # * The response status, headers, and body
    # * The request body (rack.input) and cached form data, if possible
    #
    # Because the request body may not be replayable, a branch that has
    # read the request body can only be rolled back if the body can be
    # rewound.  If the branch read the request body and it cannot be
    # rewound, the rollback is not attempted and a RodaError is raised,
    # since pretending the rollback succeeded would route the request
    # with a consumed body.
    #
    # +try_branch+ calls can be nested.  Each nested call snapshots its
    # own state, so rolling back an inner branch restores the state at
    # the point the inner branch started, without affecting the outer
    # branch's snapshot.
    module TryBranch
      # Wraps the request body (rack.input) while a tentative branch
      # runs, recording whether the body was read, so a rollback knows
      # whether it needs to (and whether it can) replay the body.
      class InputWrapper
        # Whether anything was read from the input while wrapped.
        attr_reader :read
        alias read? read

        def initialize(input)
          @input = input
          @read = false
        end

        def read(*args)
          @read = true
          @input.read(*args)
        end

        def gets(*args)
          @read = true
          @input.gets(*args)
        end

        def each(*args, &block)
          @read = true
          @input.each(*args, &block)
        end

        def rewind
          @input.rewind
        end

        def method_missing(meth, *args, &block)
          @input.send(meth, *args, &block)
        end

        def respond_to_missing?(meth, include_private=false)
          @input.respond_to?(meth, include_private)
        end
      end

      module RequestMethods
        # Tentatively run the given block if the matchers match.  See
        # TryBranch for details.
        def try_branch(*args)
          state = _try_branch_snapshot

          begin
            if args.empty? || match_all(args)
              captures = @captures[state[:captures].length..-1] || []
              catch(:roda_try_branch_abandon) do
                yield(*captures)
              end
            end

            _try_branch_rollback(state)
            nil
          ensure
            # Never leave the request body wrapped after the branch.
            @env['rack.input'] = state[:input] if state[:input]
          end
        end

        # Abandon the current tentative branch started by +try_branch+,
        # rolling back all changes made inside the branch.  See
        # TryBranch for details.
        def abandon_branch
          throw :roda_try_branch_abandon
        end

        private

        # Environment keys rack uses to cache parsed form data.  These
        # are derived from the request body, so they must be rolled back
        # if the request body is rolled back.
        TRY_BRANCH_FORM_KEYS = %w[rack.request.form_input rack.request.form_hash rack.request.form_pairs].freeze

        # Snapshot the state that a tentative branch is allowed to
        # change, so it can be restored if the branch is abandoned.
        def _try_branch_snapshot
          res = response
          env = @env
          input = env['rack.input']
          form = {}
          TRY_BRANCH_FORM_KEYS.each{|k| form[k] = env[k] if env.key?(k)}

          wrapper = pos = nil
          if input
            # Wrap the input for each branch, so that reads are tracked
            # per branch, and nested branches do not interfere with the
            # read tracking of outer branches.
            wrapper = InputWrapper.new(input)
            env['rack.input'] = wrapper
            begin
              pos = input.pos
            rescue IOError, SystemCallError, NoMethodError
              # Input position is not trackable, will fall back to
              # rewinding the input on rollback.
            end
          end

          {
            remaining_path: @remaining_path,
            captures: @captures.dup,
            status: res.status,
            headers: res.headers.dup,
            body: res.body.dup,
            length: res.instance_variable_get(:@length),
            input: input,
            input_wrapper: wrapper,
            input_pos: pos,
            form: form
          }
        end

        # Restore the state snapshot by _try_branch_snapshot.  Raises
        # RodaError if the branch read the request body and the body
        # cannot be replayed.
        def _try_branch_rollback(state)
          _try_branch_rollback_input(state)

          @remaining_path = state[:remaining_path]
          @captures.replace(state[:captures])

          res = response
          res.status = state[:status]
          res.headers.replace(state[:headers])
          res.body.replace(state[:body])
          res.instance_variable_set(:@length, state[:length])
          nil
        end

        # Restore the request body and cached form data.  If the branch
        # read the request body, attempt to rewind it so later routing
        # sees the same body.  If the body cannot be rewound, the
        # rollback cannot be completed honestly, so raise.
        def _try_branch_rollback_input(state)
          env = @env
          form = state[:form]
          TRY_BRANCH_FORM_KEYS.each{|k| env.delete(k)}
          form.each{|k, v| env[k] = v}

          return unless (input = state[:input]) && state[:input_wrapper].read?

          pos = state[:input_pos]
          begin
            if pos && input.respond_to?(:pos=)
              input.pos = pos
            elsif pos.nil? || pos == 0
              input.rewind
            else
              raise IOError, "input does not support restoring the read position"
            end
          rescue NoMethodError, IOError, SystemCallError => e
            raise RodaError, "cannot abandon try_branch: request body was read and is not replayable (#{e.class}: #{e.message})"
          end
        end
      end
    end

    register_plugin(:try_branch, TryBranch)
  end
end
