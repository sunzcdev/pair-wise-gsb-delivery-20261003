# frozen_string_literal: true

require "test_helper"

class RedisClient
  class CommandBuilderTest < RedisClientTestCase
    def test_positional
      assert_equal ["a", "b", "c"], call("a", "b", "c")
    end

    def test_array
      assert_equal ["a", "b", "c"], call("a", ["b", "c"])
    end

    def test_hash
      assert_equal ["a", "b", "c"], call("a", { "b" => "c" })
    end

    def test_symbol
      assert_equal ["a", "b", "c", "d"], call(:a, { b: :c }, :d)
    end

    def test_numeric
      assert_equal ["1", "2.3"], call(1, 2.3)
    end

    def test_kwargs_boolean
      assert_equal ["withscores"], call(ttl: nil, ex: false, withscores: true)
    end

    def test_kwargs_values
      assert_equal ["ttl", "42"], call(ttl: 42)
    end

    def test_nil_kwargs
      assert_equal ["a", "b", "c"], CommandBuilder.generate(%i(a b c))
    end

    def test_legacy_nil_and_booleans_are_rejected
      assert_raises TypeError do
        CommandBuilder.generate(["SET", "key", nil])
      end

      assert_raises TypeError do
        CommandBuilder.generate(["SET", "key", true])
      end

      assert_raises TypeError do
        CommandBuilder.generate(["SET", "key", false])
      end
    end

    def test_legacy_only_flattens_one_level
      error = assert_raises TypeError do
        CommandBuilder.generate(["LPUSH", "list", [1, [2, 3]]])
      end
      assert_includes error.message, "Unsupported command argument type: Array"
    end

    def test_legacy_empty_command
      assert_raises ArgumentError do
        CommandBuilder.generate([])
      end
    end

    def test_build_scalar
      assert_equal ["SET", "key", "42", "2.5"], build("SET", :key, 42, 2.5)
      assert_equal ["a", "b", "7"], build(CommandBuilder.scalar("a"), CommandBuilder.scalar(:b), CommandBuilder.scalar(7))
    end

    def test_build_returns_only_strings
      tokens = build(:a, 1, 2.0)
      assert(tokens.all? { |token| token.is_a?(String) })
    end

    def test_build_empty_string_is_a_token
      assert_equal ["SET", "key", ""], build("SET", "key", "")
    end

    def test_build_list_expands_in_order
      assert_equal ["LPUSH", "list", "1", "2", "3", "4"],
        build("LPUSH", "list", CommandBuilder.list([1, 2, 3]), 4)
    end

    def test_build_raw_array_expands
      assert_equal ["a", "1", "2", "b"], build("a", [1, 2], "b")
    end

    def test_build_nested_list_recursively_expands
      assert_equal ["a", "1", "2", "3", "4"],
        build("a", CommandBuilder.list([1, CommandBuilder.list([2, 3]), 4]))
    end

    def test_build_empty_list_emits_nothing_unlike_empty_string
      assert_equal ["MGET", ""], build("MGET", CommandBuilder.list([]), "")
    end

    def test_build_option_true_emits_only_the_name
      assert_equal ["ZADD", "key", "withscores", "CH"],
        build("ZADD", "key", CommandBuilder.option(:withscores, true), CommandBuilder.option("CH", true))
    end

    def test_build_option_nil_and_false_are_omitted
      assert_equal ["ZADD", "key", "ch"],
        build("ZADD", "key", CommandBuilder.option(:nx, nil), CommandBuilder.option(:xx, false), CommandBuilder.option(:ch, true))
    end

    def test_build_option_value_emits_name_then_value
      assert_equal ["SET", "key", "value", "ex", "60"],
        build("SET", "key", "value", CommandBuilder.option(:ex, 60))
    end

    def test_build_option_with_empty_string_value_keeps_an_empty_token
      assert_equal ["X", ""], build(CommandBuilder.option("X", ""))
    end

    def test_build_option_value_can_be_a_list
      assert_equal ["CMD", "IDS", "1", "2"],
        build("CMD", CommandBuilder.option("IDS", CommandBuilder.list([1, 2])))
    end

    def test_build_hash_preserves_insertion_order
      assert_equal ["HMSET", "hash", "b", "2", "a", "1"],
        build("HMSET", "hash", { "b" => 2, "a" => 1 })
    end

    def test_build_hash_applies_declared_nil_and_boolean_rules
      assert_equal ["CMD", "withscores", "count", "42"],
        build("CMD", { ttl: nil, ex: false, withscores: true, count: 42 })
    end

    def test_build_binary_preserves_bytes_and_encoding
      payload = "\xFF\x00\xFE".dup.force_encoding(Encoding::BINARY)
      tokens = build("SET", "key", CommandBuilder.binary(payload))

      assert_equal(payload, tokens.last)
      assert_equal(Encoding::BINARY, tokens.last.encoding)
    end

    def test_build_binary_copies_the_input
      payload = "héllo".dup.force_encoding(Encoding::UTF_8)
      tokens = build(CommandBuilder.binary(payload))

      assert_equal(Encoding::BINARY, tokens.first.encoding)
      assert_equal(Encoding::UTF_8, payload.encoding)
    end

    def test_build_binary_rejects_non_strings
      error = assert_raises TypeError do
        CommandBuilder.binary(42)
      end
      assert_includes error.message, "binary expects a String, got: Integer"
    end

    def test_build_rejects_nil_and_booleans_as_scalars
      assert_raises TypeError do
        build("SET", "key", nil)
      end

      assert_raises TypeError do
        build("SET", "key", true)
      end

      assert_raises TypeError do
        build("SET", "key", false)
      end
    end

    def test_build_rejects_unsupported_scalar_types_with_a_path
      error = assert_raises TypeError do
        build("a", Object.new)
      end
      assert_includes error.message, "Unsupported command argument type: Object at [1]"
    end

    def test_build_error_path_points_to_the_nested_argument
      error = assert_raises TypeError do
        build("a", [Object.new])
      end
      assert_includes error.message, "[1][0]"
    end

    def test_build_error_path_points_to_the_offending_option
      error = assert_raises TypeError do
        build("a", { bad: Object.new })
      end
      assert_includes error.message, "[1][:bad]"
    end

    def test_build_rejects_non_symbol_or_string_option_names
      error = assert_raises TypeError do
        build("a", CommandBuilder.option(42, "value"))
      end
      assert_includes error.message, "Unsupported command option name type: Integer at [1]"
    end

    def test_build_list_rejects_non_arrays
      assert_raises TypeError do
        CommandBuilder.list("nope")
      end
    end

    def test_build_scalar_rejects_nil
      assert_raises TypeError do
        CommandBuilder.scalar(nil)
      end
    end

    def test_build_rejects_non_array_input
      assert_raises TypeError do
        CommandBuilder.build("SET")
      end
    end

    def test_build_empty_command
      assert_raises CommandBuilder::EmptyCommandError do
        CommandBuilder.build([])
      end
    end

    def test_build_is_empty_when_all_options_are_omitted
      assert_raises CommandBuilder::EmptyCommandError do
        CommandBuilder.build([CommandBuilder.option(:a, nil)])
      end
    end

    def test_build_is_empty_when_all_lists_are_empty
      assert_raises CommandBuilder::EmptyCommandError do
        CommandBuilder.build([CommandBuilder.list([])])
      end
    end

    def test_build_limits_recursion_depth
      deep = (0...12).reduce("leaf") { |inner, _| CommandBuilder.list([inner]) }

      error = assert_raises CommandBuilder::DepthError do
        CommandBuilder.build([deep])
      end
      assert_includes error.message, "limit 10"
    end

    def test_build_rejects_self_referential_structures
      cyclic = []
      cyclic << cyclic

      assert_raises CommandBuilder::DepthError do
        CommandBuilder.build([cyclic])
      end
    end

    def test_build_does_not_modify_the_input
      input = ["LPUSH", "list", [1, 2], { a: 1 }, CommandBuilder.list([3, 4])]
      nested_array = input[2]
      nested_hash = input[3]

      CommandBuilder.build(input)

      assert_equal(["LPUSH", "list", [1, 2], { a: 1 }, input.last], input)
      assert_equal([1, 2], nested_array)
      assert_equal({ a: 1 }, nested_hash)
    end

    def test_build_accepts_frozen_input
      input = ["CMD", [1, [2, 3]], { k: [4, 5] }, CommandBuilder.list([])].freeze

      assert_equal ["CMD", "1", "2", "3", "k", "4", "5"], CommandBuilder.build(input)
    end

    def test_token_generation_and_resp_encoding_are_separately_checkable
      tokens = CommandBuilder.build(["SET", "key", CommandBuilder.binary("\x00\xFF".b)])

      assert_equal "*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$2\r\n\x00\xFF\r\n".b,
        RESP3.dump(tokens)
      assert_equal RESP3.dump(tokens),
        CommandBuilder.dump(["SET", "key", CommandBuilder.binary("\x00\xFF".b)])
    end

    def test_resp_encoding_distinguishes_empty_string_and_empty_list
      assert_equal "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$0\r\n\r\n".b,
        CommandBuilder.dump(["SET", "k", ""])

      assert_equal "*1\r\n$4\r\nMGET\r\n".b,
        CommandBuilder.dump(["MGET", CommandBuilder.list([])])
    end

    def test_dump_appends_to_the_provided_buffer
      buffer = RESP3.new_buffer
      CommandBuilder.dump(["PING"], buffer)

      assert_equal "*1\r\n$4\r\nPING\r\n".b, buffer
    end

    private

    def call(*args, **kwargs)
      CommandBuilder.generate(args, kwargs)
    end

    def build(*args)
      CommandBuilder.build(args)
    end
  end
end
