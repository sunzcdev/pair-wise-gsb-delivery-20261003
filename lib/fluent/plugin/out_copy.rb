#
# Fluentd
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#

require 'fluent/plugin/multi_output'
require 'fluent/config/error'
require 'fluent/event'

module Fluent::Plugin
  class CopyOutput < MultiOutput
    Fluent::Plugin.register_output('copy', self)

    desc 'If true, pass different record to each `store` plugin.'
    config_param :deep_copy, :bool, default: false, deprecated: "use 'copy_mode' parameter instead"
    desc 'Pass different record to each `store` plugin by specified method. `no_copy` shares records among all `store` plugins, so they must not modify them. `deep` gives each `store` plugin fully independent records, including nested values.'
    config_param :copy_mode, :enum, list: [:no_copy, :shallow, :deep, :marshal], default: :no_copy

    config_section :store do
      desc 'Pass different record to this `store` plugin by specified method. It overrides the plugin-wide `copy_mode`. `no_copy` shares records with the other `store` plugins, so this `store` must not modify them. `deep` gives this `store` plugin fully independent records, including nested values.'
      config_param :copy_mode, :enum, list: [:no_copy, :shallow, :deep, :marshal], default: nil
    end

    # Event stream that copies records lazily, one record at a time, while a `store`
    # consumes it. Nothing is copied before the `store` reads the first event, so
    # handing out independent records never materializes a copy of the whole stream
    # up front (which matters for streaming inputs).
    # Copies are memoized per position: reading the same stream again in the same
    # `store` returns the same objects, so modifications the `store` made are kept,
    # just as with the previous eager copy. Only the events the `store` actually
    # consumed are kept.
    # Timestamps and the order of events are passed through as they are, so
    # `Fluent::EventTime` keeps its nanoseconds and is never recreated.
    class CopiedEventStream < Fluent::EventStream
      def initialize(es, copier, cached = [])
        @es = es
        @copier = copier
        @copies = cached
      end

      # A duplicated wrapper is independent of this one: everything already produced
      # is copied again, and the rest is copied lazily from the shared source.
      def dup
        self.class.new(@es, @copier, @copies.map { |record| @copier.call(record) })
      end

      def size
        @es.size
      end

      def empty?
        @es.empty?
      end

      def repeatable?
        @es.repeatable?
      end

      # Slices share the copies already produced for the same positions,
      # like `MultiEventStream#slice` shares its records.
      def slice(index, num)
        cached = if index < @copies.size
                   @copies.slice(index, num) || []
                 else
                   []
                 end
        self.class.new(@es.slice(index, num), @copier, cached)
      end

      def each(unpacker: nil, &block)
        index = 0
        @es.each(unpacker: unpacker) do |time, record|
          copied = if index < @copies.size
                     @copies[index]
                   else
                     @copies[index] = @copier.call(record)
                   end
          index += 1
          block.call(time, copied)
        end
        nil
      end
    end
    private_constant :CopiedEventStream

    SHALLOW_COPIER = ->(record) { record.dup }
    MARSHAL_COPIER = ->(record) { Marshal.load(Marshal.dump(record)) }

    # The deep copier uses its own thread-local packer/unpacker on purpose.
    # `each` is called while an outer buffer may be mid-serialization with the
    # general thread-local packer/unpacker (see `handle_stream_*` in Output),
    # so reusing those would clear/discard the outer buffer's partial data.
    # A record is packed and unpacked on its own, so nothing is accumulated.
    DEEP_PACKER_KEY = :__fluent_copy_output_deep_packer
    DEEP_UNPACKER_KEY = :__fluent_copy_output_deep_unpacker
    DEEP_COPIER = ->(record) {
      packer = (Thread.current[DEEP_PACKER_KEY] ||= Fluent::MessagePackFactory.msgpack_packer)
      unpacker = (Thread.current[DEEP_UNPACKER_KEY] ||= Fluent::MessagePackFactory.msgpack_unpacker)
      packer.clear
      packer.write(record)
      data = packer.full_pack
      unpacker.reset
      copied = nil
      unpacker.feed_each(data) { |r| copied = r }
      copied
    }
    private_constant :SHALLOW_COPIER, :MARSHAL_COPIER, :DEEP_COPIER, :DEEP_PACKER_KEY, :DEEP_UNPACKER_KEY

    attr_reader :ignore_errors, :ignore_if_prev_successes

    def initialize
      super
      @ignore_errors = []
      @ignore_if_prev_successes = []
      @copy_procs = []
    end

    def configure(conf)
      super

      @copy_mode = :shallow if @deep_copy
      @copy_procs = @stores.map { |store| gen_copy_proc(store.copy_mode || @copy_mode) }
      @stores.each_with_index { |store, i|
        if i == 0 && store.arg.include?('ignore_if_prev_success')
          raise Fluent::ConfigError, "ignore_if_prev_success must specify 2nd or later <store> directives"
        end
        @ignore_errors << (store.arg.include?('ignore_error'))
        @ignore_if_prev_successes << (store.arg.include?('ignore_if_prev_success'))
      }
      if @ignore_errors.uniq.size == 1 && @ignore_errors.include?(true) && !@ignore_if_prev_successes.include?(true)
        log.warn "ignore_errors are specified in all <store>, but ignore_if_prev_success is not specified. Is this intended?"
      end
    end

    def multi_workers_ready?
      true
    end

    def process(tag, es)
      # Every `store` has to traverse the same events, so a streaming (non-repeatable)
      # input is made repeatable here once. This keeps only the event references.
      # Copying records for the stores happens lazily in CopiedEventStream while
      # each `store` consumes the stream, so a deep copy never materializes
      # another copy of the whole stream up front.
      unless es.repeatable?
        m = Fluent::MultiEventStream.new
        es.each {|time,record|
          m.add(time, record)
        }
        es = m
      end
      success = Array.new(outputs.size)
      outputs.each_with_index do |output, i|
        begin
          if i > 0 && success[i - 1] && @ignore_if_prev_successes[i]
            log.debug "ignore copy because prev_success in #{output.plugin_id}", index: i
          else
            copy_proc = @copy_procs[i]
            output.emit_events(tag, copy_proc ? CopiedEventStream.new(es, copy_proc) : es)
            success[i] = true
          end
        rescue => e
          if @ignore_errors[i]
            log.error "ignore emit error in #{output.plugin_id}", error: e
          else
            raise e
          end
        end
      end
    end

    private

    # Returns a per-record copier, or nil to share records as they are.
    def gen_copy_proc(copy_mode)
      case copy_mode
      when :no_copy
        nil
      when :shallow
        SHALLOW_COPIER
      when :deep
        DEEP_COPIER
      when :marshal
        MARSHAL_COPIER
      else
        raise "BUG: unknown copy_mode: #{copy_mode.inspect}"
      end
    end
  end
end
