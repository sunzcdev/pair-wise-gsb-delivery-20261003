# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The tentative plugin adds an explicit tentative branch interface via
    # the +tentative+ request method.  A tentative branch gets the first
    # chance to recognize and handle a request.  If the branch handles the
    # request, all changes it made are committed.  If the branch gives up
    # by calling +abandon+, the changes it made to the remaining path,
    # captures, and response status, headers, and body are restored before
    # routing continues:
    #
    #   plugin :tentative
    #
    #   route do |r|
    #     r.tentative do
    #       r.on "foo", Integer do |id|
    #         response['X-Branch'] = 'first'
    #         r.abandon if id > 10
    #         "first #{id}"
    #       end
    #
    #       # Reached if the matcher above did not match.  Without an
    #       # explicit abandon, the tentative branch would be committed.
    #       r.abandon
    #     end
    #
    #     r.on "foo", Integer do |id|
    #       "second #{id}"
    #     end
    #   end
    #
    # In this example, requests to /foo/1 are handled by the first branch,
    # while requests to /foo/11 are abandoned by the first branch and
    # handled by the second branch, with the response containing only the
    # second branch's changes.
    #
    # If the tentative block finishes without halting and without +abandon+
    # being called, the branch is committed, and the block's return value
    # is handled like any other match block return value.
    #
    # Tentative branches can be nested.  Abandoning a nested tentative
    # branch only restores state back to the start of that branch, it does
    # not restore state recorded by an enclosing tentative branch.
    #
    # Note that only the remaining path, captures, and response status,
    # headers, and body are restored.  Other side effects of the branch,
    # such as changes to the session or the rack environment, are not
    # restored.
    #
    # If the branch read the request body before being abandoned, the body
    # must be restored for the abandonment to succeed.  While a tentative
    # branch is running, the rack input is wrapped to detect reads of the
    # request body.  If the request body was read and cannot be rewound to
    # its original position, a RodaError is raised, since the request state
    # cannot actually be restored in that case.
    module Tentative
      # Wrapper around the rack input that records whether the request
      # body was read while a tentative branch was running.
      class InputTracker
        # Wrap the given rack input.
        def initialize(input)
          @input = input
          @read = false
        end

        # Whether the request body was read through the wrapper.
        def read?
          @read
        end

        def read(*args, &block)
          @read = true
          @input.read(*args, &block)
        end

        def gets(*args, &block)
          @read = true
          @input.gets(*args, &block)
        end

        def each(*args, &block)
          @read = true
          @input.each(*args, &block)
        end

        def rewind(*args, &block)
          @input.rewind(*args, &block)
        end

        def method_missing(meth, *args, &block)
          @input.send(meth, *args, &block)
        end

        def respond_to_missing?(meth, include_private=false)
          @input.respond_to?(meth, include_private)
        end
      end

      module RequestMethods
        # Run the block as a tentative branch.  If the block calls +abandon+,
        # restore the remaining path, captures, and response state recorded
        # before the block was called, and return nil so routing continues.
        # Otherwise, commit the branch, handling the block's return value
        # like any other match block return value.
        def tentative
          remaining_path = @remaining_path
          captures = @captures.dup
          res = response
          snapshot = res.tentative_snapshot
          env = @env
          input = env['rack.input']
          if input
            input_pos = input.pos if input.respond_to?(:pos)
            tracker = InputTracker.new(input)
            env['rack.input'] = tracker
          end

          begin
            catch(:roda_tentative_abandon) do
              block_result(yield)
              throw :halt, res.finish
            end
          ensure
            if input
              env['rack.input'] = input
              env['rack.request.form_input'] = input if env['rack.request.form_input'].equal?(tracker)
            end
          end

          _tentative_restore_body(input, input_pos) if input && tracker.read?

          @remaining_path = remaining_path
          @captures.replace(captures)
          res.tentative_restore(snapshot)
          nil
        end

        # Give up on the current tentative branch, restoring the remaining
        # path, captures, and response state to what they were when the
        # branch was started.  Raises an UncaughtThrowError if called
        # outside a tentative branch.
        def abandon
          throw :roda_tentative_abandon
        end

        private

        # The request body was read during the tentative branch, so attempt
        # to restore it to its original position.  Raise a RodaError if the
        # body cannot be restored, since the request state cannot actually
        # be restored in that case.
        def _tentative_restore_body(input, input_pos)
          # If the input position can be checked and is unchanged, the
          # body was not actually consumed, so there is nothing to restore.
          return if input_pos && input.pos == input_pos

          begin
            input.rewind
          rescue
            raise RodaError, "cannot restore request body read by abandoned tentative branch"
          end

          if input_pos && input.pos != input_pos
            raise RodaError, "cannot restore request body read by abandoned tentative branch"
          end
        end
      end

      module ResponseMethods
        # Snapshot the mutable response state (status, headers, and body),
        # so it can be restored later if the tentative branch is abandoned.
        def tentative_snapshot
          [@status, @headers.dup, @body.dup, @length]
        end

        # Restore the response state from the given snapshot, which should
        # have been created by tentative_snapshot.
        def tentative_restore(snapshot)
          @status, headers, body, @length = snapshot
          @headers.replace(headers)
          @body.replace(body)
          nil
        end
      end
    end

    register_plugin(:tentative, Tentative)
  end
end
