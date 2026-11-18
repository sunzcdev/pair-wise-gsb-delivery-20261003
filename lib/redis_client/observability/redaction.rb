# frozen_string_literal: true

class RedisClient
  module Observability
    # Commands that are guaranteed not to mutate server state. Everything else is
    # treated as a potential write, so that a command whose outcome is unknown
    # after a connection failure is never optimistically assumed to be a read.
    #
    # The list is intentionally conservative and covers the common read-only
    # Redis commands (including the `*SCAN` family and pub/sub introspection).
    # Bare command names whose whole command family is read-only.
    READ_ONLY_BARE_COMMANDS = %w[
      ASKING
      BITCOUNT
      BITFIELD_RO
      BITPOS
      CLIENT
      COMMAND
      DBSIZE
      DUMP
      ECHO
      EXISTS
      EXPIRETIME
      GEODIST
      GEOHASH
      GEOPOS
      GEOSEARCH
      GET
      GETBIT
      GETRANGE
      HEXISTS
      HGET
      HGETALL
      HKEYS
      HLEN
      HMGET
      HRANDFIELD
      HSCAN
      HSTRLEN
      HVALS
      INFO
      KEYS
      LASTSAVE
      LINDEX
      LLEN
      LMPOP
      LOLWUT
      LPOP
      LPOS
      LRANGE
      LREM
      MEMORY
      MGET
      OBJECT
      PELIST
      PFCOUNT
      PING
      PTTL
      PUBSUB
      RANDOMKEY
      ROLE
      RPOP
      SCAN
      SCARD
      SDIFF
      SELECT
      SINTER
      SINTERCARD
      SISMEMBER
      SMEMBERS
      SMISMEMBER
      SRANDMEMBER
      SSCAN
      STRLEN
      SUBSCRIBE
      SUNION
      TIME
      TTL
      TYPE
      UNSUBSCRIBE
      UNWATCH
      WATCH
      XLEN
      XPENDING
      XRANGE
      XREAD
      XREVRANGE
      ZCARD
      ZCOUNT
      ZDIFF
      ZINTER
      ZINTERCARD
      ZLEXCOUNT
      ZMPOP
      ZMSCORE
      ZPOPMAX
      ZPOPMIN
      ZRANDMEMBER
      ZRANGE
      ZRANGEBYLEX
      ZRANGEBYSCORE
      ZRANK
      ZREVRANGE
      ZREVRANGEBYLEX
      ZREVRANGEBYSCORE
      ZREVRANK
      ZSCAN
      ZSCORE
      ZUNION
    ].freeze

    # Only these specific command + subcommand pairs are read-only. An unknown
    # subcommand of one of these families is deliberately treated as a write.
    READ_ONLY_SUBCOMMANDS = [
      %w[CONFIG GET],
      %w[DEBUG OBJECT],
      %w[LATENCY DOCTOR],
      %w[LATENCY GRAPH],
      %w[LATENCY HISTORY],
      %w[LATENCY LATEST],
      %w[LATENCY RESET],
      %w[LATENCY STATS],
      %w[XINFO CONSUMERS],
      %w[XINFO GROUPS],
      %w[XINFO STREAM],
    ].freeze

    READ_ONLY_COMMANDS = (
      READ_ONLY_BARE_COMMANDS.map { |name| [name] } +
      READ_ONLY_SUBCOMMANDS
    ).each_with_object({}) { |words, table| table[words] = true }.freeze

    module_function

    # Determine whether +command+ is known to be read-only. The table contains
    # either a bare command name (the whole command is read-only) or a command +
    # subcommand pair (only that specific subcommand is read-only). For command
    # families that have registered subcommands (e.g. CONFIG), the exact pair
    # must match and an unknown subcommand is treated as a write. Every other
    # command is matched by its bare name regardless of how many arguments it
    # takes, so `GET key` stays read-only.
    def read_only_command?(command)
      return false if command.nil? || command.empty?

      first = command_name(command.first)
      return false unless first

      if command.size > 1 && subcommand_family?(first)
        second = command_name(command[1])
        READ_ONLY_COMMANDS.key?([first, second])
      else
        READ_ONLY_COMMANDS.key?([first])
      end
    end

    # Whether this command family has any command + subcommand entries.
    def subcommand_family?(first)
      READ_ONLY_SUBCOMMANDS.any? { |words| words[0] == first }
    end

    # Redact a command for inclusion in an event. By default only the command
    # name and the number of arguments are exposed, argument values never leave
    # the client. Callers that explicitly opt in (e.g. a trusted tracer) can
    # request the raw command instead.
    def redact(command, raw: false)
      return nil if command.nil?

      if raw
        command.dup
      else
        head = command.first
        {
          command: head.is_a?(Symbol) ? head.to_s.upcase : head.to_s.upcase,
          arguments: command.size - 1,
        }
      end
    end

    # Same as +redact+ but for a whole batch, preserving item positions.
    def redact_batch(commands, raw: false)
      return [] if commands.nil?

      commands.map { |command| redact(command, raw: raw) }
    end

    def command_name(token)
      return nil if token.nil?

      name = token.is_a?(Symbol) ? token.to_s : token.to_s
      name = name.upcase
      name.empty? ? nil : name
    end
  end
end
