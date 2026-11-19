# frozen_string_literal: true

class RedisClient
  # Detailed outcome of a pipelined execution.
  #
  # Unlike the regular pipelined mode which either returns all the results
  # or raises a single error, the detailed mode reports exactly which
  # commands were replied to, which commands are in an unknown state (the
  # server may or may not have received and executed them) and why the
  # pipeline stopped.
  #
  # Because commands in the unknown state may already have been executed,
  # they are never automatically resent.
  class PipelinedResult
    # Placeholder used in #results for commands whose outcome is unknown.
    UNKNOWN = Object.new
    class << UNKNOWN
      def inspect
        "RedisClient::PipelinedResult::UNKNOWN"
      end
      alias_method :to_s, :inspect
    end
    UNKNOWN.freeze

    # The commands that were sent, in their original order.
    attr_reader :commands

    # The outcome of each command, aligned with #commands.
    # Positions that couldn't be confirmed contain UNKNOWN.
    # Server side errors are CommandError instances.
    attr_reader :results

    # The server side errors (CommandError) among the confirmed replies,
    # in command order. They are also present in #results.
    attr_reader :errors

    # The connection or protocol error that interrupted the pipeline,
    # or nil if the pipeline ran to completion.
    attr_reader :failure

    # The out-of-band RESP3 push messages received while waiting for
    # replies. They don't occupy a command position in #results.
    attr_reader :pushes

    # How many leading commands got their reply confirmed.
    attr_reader :confirmed_count

    def initialize(commands:, results:, errors:, confirmed_count:, failure: nil, failure_stage: nil, pushes: [])
      @commands = commands
      @results = results
      @errors = errors
      @confirmed_count = confirmed_count
      @failure = failure
      @failure_stage = failure_stage
      @pushes = pushes
    end

    # Where the pipeline was interrupted: `:write` if the commands couldn't
    # all be written (no reply is confirmed), `:read` if the connection was
    # lost while waiting for replies (a leading subset of the replies is
    # confirmed), or nil if the pipeline ran to completion.
    attr_reader :failure_stage

    # Whether every command got its reply. Note that a complete pipeline
    # may still contain server side errors, see #errors.
    def complete?
      @failure.nil?
    end

    # Whether the pipeline was interrupted before all replies were read.
    def interrupted?
      !complete?
    end

    # Whether the write of the commands was interrupted. In that case no
    # reply is confirmed and it is unknown which commands, if any, reached
    # the server.
    def write_failed?
      @failure_stage == :write
    end

    # Whether reading the replies was interrupted. In that case the leading
    # #confirmed_count replies are confirmed and the remaining commands are
    # in an unknown state.
    def read_failed?
      @failure_stage == :read
    end

    # The range of command positions whose outcome is unknown.
    # Empty when the pipeline is complete.
    def unconfirmed_range
      @confirmed_count...@commands.size
    end

    # The commands whose outcome is unknown. They may or may not have been
    # executed by the server, so they must not be blindly resent.
    def unconfirmed_commands
      @commands[@confirmed_count..] || []
    end

    # Whether the command at the given position got its reply confirmed.
    def confirmed?(index)
      index >= 0 && index < @confirmed_count
    end

    def [](index)
      @results[index]
    end

    def size
      @commands.size
    end
  end
end
