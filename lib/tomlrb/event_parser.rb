# frozen_string_literal: true

require 'tomlrb/generated_parser'
require 'tomlrb/event_handler'

module Tomlrb
  # Runs the existing racc grammar against a chunked tokenizer while
  # streaming structural events to a consumer.
  #
  # Unlike {Parser}, no output Hash is produced: after {#parse} returns, the
  # handler only holds the duplicate-key trie and other validation state.
  class EventParser < Tomlrb::GeneratedParser
    def initialize(tokenizer, consumer:, **options)
      @tokenizer = tokenizer
      @handler = EventHandler.new(consumer: consumer, **options)
      super()
    end

    def next_token
      @tokenizer.next_token
    end

    def parse
      begin
        do_parse
      rescue Racc::ParseError
        error = @handler.unclosed_structure_error
        raise error if error

        raise
      end
      @handler
    end
  end
end
