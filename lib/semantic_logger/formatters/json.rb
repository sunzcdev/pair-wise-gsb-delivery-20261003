require "json"
module SemanticLogger
  module Formatters
    # Writes each log entry as one line of JSON, with the fields of Formatters::Raw.
    #
    # A batch (see #batch) holds the entries of any number of loggers, in the order they were
    # logged, each written exactly as #call writes it on its own. A log that cannot be encoded is
    # replaced, in its place, by a summary of the failure (see Base#unencodable_entry), so that one
    # bad log neither loses the logs around it nor makes the whole batch impossible to write.
    class Json < Raw
      # Default JSON time format is ISO8601
      def initialize(time_format: :iso_8601, time_key: :timestamp, **args)
        super
      end

      # Returns log messages in JSON format
      def call(log, logger)
        json_of(super)
      end

      # Returns a batch of log messages as a single JSON array.
      #
      # Each message is formatted by #call, so a message written in a batch has exactly the fields
      # it has when it is written on its own, whatever logger it came from. The entries stay in the
      # order they were logged, and a message that cannot be encoded is written as a summary in its
      # place, rather than failing the batch (see Base#encode_batch).
      def batch(logs, logger)
        encode_batch(logs, logger)
      end

      private

      # Cleans backtraces with the application's cleaner, as the text formatters do. Error services
      # receive the exception itself rather than this output, and the formatters written for a
      # particular service, such as Ecs, are not built on this one.
      def default_backtrace_cleaner
        true
      end
    end
  end
end
