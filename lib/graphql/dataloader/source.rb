# frozen_string_literal: true
#--
# typed: true

module GraphQL
  class Dataloader
    class Source
      # Called by [Dataloader](rdoc-ref:Dataloader) to prepare the [Source](rdoc-ref:Source)'s internal state
      def setup(dataloader) # :nodoc:
        # These keys have been requested but haven't been fetched yet
        @pending = {}
        # These keys have been passed to `fetch` but haven't been finished yet
        @fetching = {}
        # { result_key => Set<GraphQL selection result> } tracking which branch
        # asked for each pending key. A key stays pending only while at least one
        # of its requesters is still active; when the last requester goes away
        # (e.g. its branch died on a non-null error), the key is removed so it's
        # never fetched.
        @pending_requesters = Hash.new { |h, k| h[k] = Set.new.compare_by_identity }
        # { result_key => Object }
        @results = {}
        @dataloader = dataloader
      end

      attr_reader :dataloader

      # **Returns**
      #
      # - `Dataloader::Request` — a pending request for a value from `key`. Call `.load` on that object to wait for the result.
      #
      # :call-seq:
      #   request(value) -> Dataloader::Request
      def request(value)
        res_key = result_key_for(value)
        add_pending_key(res_key, value)
        Dataloader::Request.new(self, value)
      end

      # Implement this method to return a stable identifier if different
      # key objects should load the same data value.
      #
      # **Parameters**
      #
      # - `value` (`Object`) — A value passed to `.request` or `.load`, for which a value will be loaded
      #
      # **Returns**
      #
      # - `Object` — The key for tracking this pending data
      #
      # :call-seq:
      #   result_key_for(Object value) -> Object
      def result_key_for(value)
        value
      end

      # Implement this method if varying values given to [load](rdoc-ref:load) (etc) should be consolidated
      # or normalized before being handed off to your [fetch](rdoc-ref:fetch) implementation.
      #
      # This is different than [result_key_for](rdoc-ref:result_key_for) because _that_ method handles unification inside Dataloader's cache,
      # but this method changes the value passed into [fetch](rdoc-ref:fetch).
      #
      # **Parameters**
      #
      # - `value` (`Object`) — The value passed to [load](rdoc-ref:load), [load_all](rdoc-ref:load_all), [request](rdoc-ref:request), or [request_all](rdoc-ref:request_all)
      #
      # **Returns**
      #
      # - `Object` — The value given to [fetch](rdoc-ref:fetch)
      #
      # :call-seq:
      #   normalize_fetch_key(Object value) -> Object
      def normalize_fetch_key(value)
        value
      end

      # **Returns**
      #
      # - `Dataloader::Request` — a pending request for a values from `keys`. Call `.load` on that object to wait for the results.
      #
      # :call-seq:
      #   request_all(values) -> Dataloader::Request
      def request_all(values)
        values.each do |v|
          res_key = result_key_for(v)
          add_pending_key(res_key, v)
        end
        Dataloader::RequestAll.new(self, values)
      end

      # **Parameters**
      #
      # - `value` (`Object`) — A loading value which will be passed to [fetch](rdoc-ref:#fetch) if it isn't already in the internal cache.
      #
      # **Returns**
      #
      # - `Object` — The result from [fetch](rdoc-ref:#fetch) for `key`. If `key` hasn't been loaded yet, the Fiber will yield until it's loaded.
      #
      # :call-seq:
      #   load(Object value) -> Object
      def load(value)
        result_key = result_key_for(value)
        if @results.key?(result_key)
          result_for(result_key)
        else
          add_pending_key(result_key, value)
          sync([result_key])
          result_for(result_key)
        end
      end

      # **Parameters**
      #
      # - `values` (`Array<Object>`) — Loading keys which will be passed to `#fetch` (or read from the internal cache).
      #
      # **Returns**
      #
      # - `Object` — The result from [fetch](rdoc-ref:#fetch) for `keys`. If `keys` haven't been loaded yet, the Fiber will yield until they're loaded.
      #
      # :call-seq:
      #   load_all(Array[Object] values) -> Object
      def load_all(values)
        result_keys = []
        pending_keys = []
        values.each { |v|
          k = result_key_for(v)
          result_keys << k
          if add_pending_key(k, v)
            pending_keys << k
          end
        }

        if !pending_keys.empty?
          sync(pending_keys)
        end

        result_keys.map! { |k| result_for(k) }
        result_keys
      end

      # Subclasses must implement this method to return a value for each of `keys`
      #
      # **Parameters**
      #
      # - `keys` (`Array<Object>`) — keys passed to [load](rdoc-ref:#load), [load all](rdoc-ref:#load_all), [request](rdoc-ref:#request), or [request all](rdoc-ref:#request_all)
      #
      # **Returns**
      #
      # - `Array<Object>` — A loaded value for each of `keys`. The array must match one-for-one to the list of `keys`.
      #
      # :call-seq:
      #   fetch(Array[Object] keys) -> Array[Object]
      def fetch(keys)
        # somehow retrieve these from the backend
        raise "Implement `#{self.class}#fetch(#{keys.inspect}) to return a record for each of the keys"
      end

      MAX_ITERATIONS = 1000
      # Wait for a batch, if there's anything to batch.
      # Then run the batch and update the cache.
      #
      # **Returns**
      #
      # - `void`
      #
      # :call-seq:
      #   sync(pending_result_keys) -> void
      def sync(pending_result_keys)
        @dataloader.queue_pending_source(self) if pending?
        @dataloader.yield(self)
        iterations = 0
        while pending_result_keys.any? { |key| !@results.key?(key) }
          iterations += 1
          if iterations > MAX_ITERATIONS
            raise "#{self.class}#sync tried #{MAX_ITERATIONS} times to load pending keys (#{pending_result_keys}), but they still weren't loaded. There is likely a circular dependency#{@dataloader.fiber_limit ? " or `fiber_limit: #{@dataloader.fiber_limit}` is set too low" : ""}."
          end
          @dataloader.yield(self)
        end
        nil
      end

      # **Returns**
      #
      # - `Boolean` — True if this source has any pending requests for data.
      #
      # :call-seq:
      #   pending?() -> bool
      def pending?
        !@pending.empty?
      end

      # Add these key-value pairs to this source's cache
      # (future loads will use these merged values).
      #
      # **Parameters**
      #
      # - `new_results` (`Hash<Object => Object>`) — key-value pairs to cache in this source
      #
      # **Returns**
      #
      # - `void`
      #
      # :call-seq:
      #   merge(Hash[Object, Object] new_results) -> void
      def merge(new_results)
        new_results.each do |new_k, new_v|
          key = result_key_for(new_k)
          @results[key] = new_v
        end
        nil
      end

      # Called by [GraphQL::Dataloader](rdoc-ref:GraphQL::Dataloader) to resolve and pending requests to this source.
      #
      # **Returns**
      #
      # - `void`
      def run_pending_keys # :nodoc:
        @fetching.each_key { |k| @pending.delete(k) }
        return if @pending.empty?
        fetch_h = @pending
        @fetching.merge!(fetch_h)
        @pending = {}
        # These keys are now being fetched; their requester tracking is no longer
        # needed for deciding whether to skip a load.
        requesters_for_batch = @pending_requesters.values.flat_map(&:to_a).uniq
        fetch_h.each_key { |k| @pending_requesters.delete(k) }
        # Tell the dataloader to prune requesters that have no pending keys left
        # on this source, so its registry doesn't retain entries for branches
        # that finished normally.
        @dataloader.prune_requesters(self, requesters_for_batch)
        results = fetch(fetch_h.values)
        idx = 0

        fetch_h.each_key do |key|
          @results[key] = results[idx]
          @fetching.delete(key)
          idx += 1
        end
        nil
      rescue StandardError => error
        fetch_h.each_key { |key|
          @results[key] = error
          @fetching.delete(key)
          @pending_requesters.delete(key)
        }
      end

      # These arguments are given to `dataloader.with(source_class, ...)`. The object
      # returned from this method is used to de-duplicate batch loads under the hood
      # by using it as a Hash key.
      #
      # By default, the arguments are all put in an Array. To customize how this source's
      # batches are merged, override this method to return something else.
      #
      # For example, if you pass `ActiveRecord::Relation`s to `.with(...)`, you could override
      # this method to call `.to_sql` on them, thus merging `.load(...)` calls when they apply
      # to equivalent relations.
      #
      # **Parameters**
      #
      # - `batch_args` (`Array<Object>`)
      # - `batch_kwargs` (`Hash`)
      #
      # **Returns**
      #
      # - `Object`
      #
      # :call-seq:
      #   batch_key_for(Array[Object] *batch_args, Hash **batch_kwargs) -> Object
      def self.batch_key_for(*batch_args, **batch_kwargs)
        if batch_kwargs.any? # rubocop:disable Development/NoneWithoutBlockCop
          [*batch_args, **batch_kwargs]
        else
          batch_args
        end
      end

      # Clear any already-loaded objects for this source
      #
      # **Returns**
      #
      # - `void`
      #
      # :call-seq:
      #   clear_cache() -> void
      def clear_cache
        @results.clear
        nil
      end

      # Detach all pending keys (and their requester tracking) from this source,
      # returning the pending `{ key => fetch_key }` hash. Used by
      # {Dataloader#run_isolated} to park pending work for the duration of an
      # isolated run.
      #
      # **Returns**
      #
      # - `Hash<Object, Object>`
      def detach_pending # :nodoc:
        pending = @pending
        @pending = {}
        @pending_requesters.clear
        pending
      end

      # Re-attach a pending key previously removed by {#detach_pending}, unless
      # it has since been loaded into the cache. Used by {Dataloader#run_isolated}.
      #
      # **Returns**
      #
      # - `void`
      def attach_pending(key, fetch_key) # :nodoc:
        return if @results.key?(key)

        @dataloader.queue_pending_source(self) if @pending.empty?
        @pending[key] = fetch_key
        nil
      end

      attr_reader :pending, :results

      # Remove pending keys that the given requester (a GraphQL selection
      # result) was the last remaining requester for. Keys still requested by
      # another active requester (shared reads) and keys already being fetched
      # are left untouched.
      #
      # Called by [Dataloader](rdoc-ref:Dataloader) when a selection branch is
      # marked dead (e.g. a non-null field resolved to nil).
      #
      # **Returns**
      #
      # - `void`
      def release_pending_keys_for(requester) # :nodoc:
        return if @pending.empty?

        orphaned_keys = nil
        @pending.each_key do |key|
          requesters = @pending_requesters[key]
          requesters.delete(requester)
          if requesters.empty?
            @pending_requesters.delete(key)
            (orphaned_keys ||= []) << key
          end
        end

        if orphaned_keys
          orphaned_keys.each { |key| @pending.delete(key) }
        end
        nil
      end

      private

      def add_pending_key(result_key, value)
        return false if @results.key?(result_key)

        was_empty = @pending.empty?
        @pending[result_key] ||= normalize_fetch_key(value)
        if (selection_result = @dataloader.current_selection_result) &&
           @pending_requesters[result_key].add?(selection_result)
          @dataloader.register_pending_source_for(selection_result, self)
        end
        @dataloader.queue_pending_source(self) if was_empty
        true
      end

      # Reads and returns the result for the key from the internal cache, or raises an error if the result was an error
      #
      # **Parameters**
      #
      # - `key` (`Object`) — key passed to [load](rdoc-ref:#load) or [load all](rdoc-ref:#load_all)
      #
      # **Returns**
      #
      # - `Object` — The result from [fetch](rdoc-ref:#fetch) for `key`.
      def result_for(key) # :nodoc:
        if !@results.key?(key)
          raise GraphQL::InvariantError, <<-ERR
Fetching result for a key on #{self.class} that hasn't been loaded yet (#{key.inspect}, loaded: #{@results.keys})

This key should have been loaded already. This is a bug in GraphQL::Dataloader, please report it on GitHub: https://github.com/rmosolgo/graphql-ruby/issues/new.
ERR
        end
        result = @results[key]
        if result.is_a?(StandardError)
          # Dup it because the rescuer may modify it.
          # (This happens for GraphQL::ExecutionErrors, at least)
          raise result.dup
        end

        result
      end
    end
  end
end
