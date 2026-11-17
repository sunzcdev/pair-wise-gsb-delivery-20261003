# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/query_parser'
end

describe Rack::QueryParser do
  it "can normalize values with missing values" do
    query_parser = Rack::QueryParser.make_default(8)
    query_parser.parse_nested_query("a=a").must_equal({"a" => "a"})
    query_parser.parse_nested_query("a=").must_equal({"a" => ""})
    query_parser.parse_nested_query("a").must_equal({"a" => nil})
    query_parser.parse_query_pairs("a=a").must_equal([["a", "a"]])
    query_parser.parse_query_pairs("a=").must_equal([["a", ""]])
    query_parser.parse_query_pairs("a").must_equal([["a", nil]])
  end

  it "accepts bytesize_limit to specify maximum size of query string to parse" do
    query_parser = Rack::QueryParser.make_default(32, bytesize_limit: 3)
    query_parser.parse_query("a=a").must_equal({"a" => "a"})
    query_parser.parse_nested_query("a=a").must_equal({"a" => "a"})
    query_parser.parse_nested_query("a=a", '&').must_equal({"a" => "a"})
    query_parser.parse_query_pairs("a=a").must_equal([["a", "a"]])
    proc { query_parser.parse_query("a=aa") }.must_raise Rack::QueryParser::QueryLimitError
    proc { query_parser.parse_nested_query("a=aa") }.must_raise Rack::QueryParser::QueryLimitError
    proc { query_parser.parse_nested_query("a=aa", '&') }.must_raise Rack::QueryParser::QueryLimitError
    proc { query_parser.parse_query_pairs("a=aa") }.must_raise Rack::QueryParser::QueryLimitError
  end

  it "does not enforce a bytesize limit when bytesize_limit is nil" do
    query_parser = Rack::QueryParser.make_default(32, bytesize_limit: nil)
    query_parser.parse_query("a=aa").must_equal({"a" => "aa"})
    query_parser.parse_nested_query("a=aa").must_equal({"a" => "aa"})
    query_parser.parse_nested_query("a=aa", '&').must_equal({"a" => "aa"})
    query_parser.parse_query_pairs("a=aa").must_equal([["a", "aa"]])
  end

  it "accepts params_limit to specify maximum number of query parameters to parse" do
    query_parser = Rack::QueryParser.make_default(32, params_limit: 2)
    query_parser.parse_query("a=a&b=b").must_equal({"a" => "a", "b" => "b"})
    query_parser.parse_nested_query("a=a&b=b").must_equal({"a" => "a", "b" => "b"})
    query_parser.parse_nested_query("a=a&b=b", '&').must_equal({"a" => "a", "b" => "b"})
    query_parser.parse_query_pairs("a=a&b=b").must_equal([["a", "a"], ["b", "b"]])
    query_parser.parse_query_pairs("a=1&a=2").must_equal([["a", "1"], ["a", "2"]])
    proc { query_parser.parse_query("a=a&b=b&c=c") }.must_raise Rack::QueryParser::QueryLimitError
    proc { query_parser.parse_nested_query("a=a&b=b&c=c", '&') }.must_raise Rack::QueryParser::QueryLimitError
    proc { query_parser.parse_query("b[]=a&b[]=b&b[]=c") }.must_raise Rack::QueryParser::QueryLimitError
    proc { query_parser.parse_query_pairs("a=a&b=b&c=c") }.must_raise Rack::QueryParser::QueryLimitError
  end

  it "does not enforce a params limit when params_limit is nil" do
    query_parser = Rack::QueryParser.make_default(32, params_limit: nil)
    query_parser.parse_query("a=a&b=b&c=c").must_equal({"a" => "a", "b" => "b", "c" => "c"})
    query_parser.parse_nested_query("a=a&b=b&c=c").must_equal({"a" => "a", "b" => "b", "c" => "c"})
  end

  it "raises when normalizing params with incompatible encoding such as UTF-16LE" do
    query_parser = Rack::QueryParser.make_default(8)
    name = "utf-16le".dup.force_encoding("UTF-16LE")
    value = "Alice?".dup.force_encoding("UTF-16LE")
    lambda {
      query_parser.normalize_params({}, name, value)
    }.must_raise(::Rack::QueryParser::IncompatibleEncodingError)
  end

  it "ignores nil name when normalizing params" do
    params = Rack::QueryParser.make_default(32).make_params
    Rack::QueryParser.make_default(32).normalize_params(params, nil, "v").must_be_nil
    params.to_h.must_equal({})
  end

  it "supports custom unescaper when parsing query" do
    Rack::QueryParser.make_default(32).parse_query("a=b&&c=d") { |s| s.upcase }.
      must_equal("A" => "B", "C" => "D")
  end

  describe Rack::QueryParser::ParamBudget do
    it "validates the configured limits" do
      proc { Rack::QueryParser::ParamBudget.new(keys_limit: -1) }.
        must_raise ArgumentError
      proc { Rack::QueryParser::ParamBudget.new(nodes_limit: "x") }.
        must_raise ArgumentError
      # nil means unlimited and is always valid:
      Rack::QueryParser::ParamBudget.new(keys_limit: nil, nodes_limit: nil, bytes_limit: nil)
    end

    it "derives its limits from the query parser limits" do
      parser = Rack::QueryParser.make_default(32, bytesize_limit: 100, params_limit: 7)
      budget = parser.make_budget

      budget.keys_limit.must_equal 7
      budget.bytes_limit.must_equal 100
      budget.nodes_limit.must_be_nil
    end

    it "counts keys, expanded nodes (including nested arrays) and decoded bytes" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new

      parser.parse_nested_query("a=1&b[c]=2&d[][e]=3", '&', budget: budget)

      budget.keys.must_equal 3
      # one node per traversed nesting segment:
      # a=1 -> a (1); b[c]=2 -> b + c (2); d[][e]=3 -> d + [] + e (3)
      budget.nodes.must_equal 5
      # decoded keys "a"(1) + "b[c]"(4) + "d[][e]"(6) and values 1+1+1 = 14
      budget.bytes.must_equal 14
    end

    it "counts every element of flat and nested arrays" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new

      parser.parse_nested_query("x[]=1&x[]=2&y[][z]=1&y[][z]=2", '&', budget: budget)

      budget.keys.must_equal 4
      # x[]=* traverses x + [] twice (2 each = 4);
      # y[][z]=* traverses y + [] + z twice (3 each = 6)
      budget.nodes.must_equal 6
    end

    it "charges parse_query and parse_query_pairs against the same ledger" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new

      parser.parse_query("a=1&b=2", '&', budget: budget)
      parser.parse_query_pairs("c=3&d=4", '&', budget: budget)

      budget.keys.must_equal 4
      budget.bytes.must_equal 8
      budget.nodes.must_equal 0
    end

    it "raises before the limit is crossed and reports source, phase and counters" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new(keys_limit: 2)

      error = proc do
        parser.parse_nested_query("a=1&b=2&c=3", '&',
          budget: budget, source: :form_body)
      end.must_raise Rack::QueryParser::QueryLimitError

      error.source.must_equal :form_body
      error.phase.must_equal :pair_parsing
      error.counter.must_equal :keys
      error.limit.must_equal 2
      error.usage.must_equal 3
      error.message.must_include "form body"
      error.message.must_include "pair parsing"
    end

    it "attributes nested expansion overages to the nested_expansion phase" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new(nodes_limit: 1)

      error = proc do
        parser.parse_nested_query("a[b]=1", '&', budget: budget)
      end.must_raise Rack::QueryParser::QueryLimitError

      error.source.must_equal :query_string
      error.phase.must_equal :nested_expansion
      error.counter.must_equal :nodes
      # the first node (a) was charged, the second (b) crosses the limit
      error.usage.must_equal 2
    end

    it "raises when decoded bytes exceed the byte budget" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new(bytes_limit: 4)

      error = proc do
        parser.parse_nested_query("abc=de", '&', budget: budget)
      end.must_raise Rack::QueryParser::QueryLimitError

      error.counter.must_equal :bytes
      error.usage.must_equal 5
      error.limit.must_equal 4
    end

    it "does not mutate the parameter structure when the budget is exhausted" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new(keys_limit: 1)

      proc do
        parser.parse_nested_query("a=1&b=2", '&', budget: budget)
      end.must_raise Rack::QueryParser::QueryLimitError

      # only the first pair was inserted before the limit was hit
      budget.keys.must_equal 1
    end

    it "charges conflicting scalar/map pairs before the conflict can overwrite data" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new(nodes_limit: 1)

      # a=1 establishes a scalar, a[b]=1 then wants a Hash. The node for the
      # second pair is charged before the structure is touched, so the budget
      # error wins and the existing scalar is never overwritten.
      error = proc do
        parser.parse_nested_query("a=1&a[b]=1", '&', budget: budget)
      end.must_raise Rack::QueryParser::QueryLimitError

      error.phase.must_equal :nested_expansion
      error.counter.must_equal :nodes
      error.usage.must_equal 2
      budget.nodes.must_equal 1
    end

    it "keeps per-parser limit behavior when no budget is supplied" do
      parser = Rack::QueryParser.make_default(32, params_limit: 1)
      proc { parser.parse_nested_query("a=1&b=2", '&') }.
        must_raise Rack::QueryParser::QueryLimitError
    end

    it "lets a standalone parse run without creating or requiring global state" do
      parser = Rack::QueryParser.make_default(32)
      # calling without a budget works exactly as before:
      parser.parse_nested_query("a=1&b=2", '&').must_equal("a" => "1", "b" => "2")
      parser.parse_query_pairs("a=1", '&').must_equal([["a", "1"]])
    end

    it "counts nodes through normalize_params as well" do
      parser = Rack::QueryParser.make_default(32)
      budget = Rack::QueryParser::ParamBudget.new
      params = parser.make_params

      parser.normalize_params(params, "a[b][c]", "1", nil, budget: budget)

      budget.nodes.must_equal 3
      params.to_h.must_equal("a" => {"b" => {"c" => "1"}})
    end
  end
end
