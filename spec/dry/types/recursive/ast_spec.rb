# frozen_string_literal: true

require "timeout"

module RecursiveAstTypes
  include Dry.Types()
end

RSpec.describe Dry::Types::Recursive, "AST export and rebuild" do
  subject(:tree) do
    Dry::Types.recursive(:condition) do |r|
      leaf = RecursiveAstTypes::Hash.schema(op: RecursiveAstTypes::String, value: RecursiveAstTypes::Any)
      node = RecursiveAstTypes::Hash.schema(
        op: RecursiveAstTypes::String,
        children: RecursiveAstTypes::Array.of(r[:condition])
      )
      leaf | node
    end
  end

  let(:compiler) { Dry::Types::Compiler.new(Dry::Types) }

  it "exports a finite :recursive node with a single body" do
    ast = tree.to_ast

    expect(ast[0]).to be(:recursive)

    root_name, members, _meta = ast[1]
    expect(root_name).to be(:condition)
    expect(members.map(&:first)).to eql([:condition])
  end

  it "uses :recursive_ref leaves instead of expanding forever" do
    serialized = tree.to_ast.inspect
    expect(serialized.scan("recursive_ref").size).to be(1)
  end

  it "terminates AST export" do
    Timeout.timeout(5) { expect(tree.to_ast).to be_a(::Array) }
  end

  it "rebuilds an equivalent type from AST" do
    rebuilt = compiler.(tree.to_ast)

    expect(rebuilt).to be_a(Dry::Types::Recursive::Scope)

    input = {op: "and", children: [{op: "eq", value: 1}]}
    expect(rebuilt.(input)).to eql(input)
  end

  it "produces a stable AST across rebuild" do
    rebuilt = compiler.(tree.to_ast)
    expect(rebuilt.to_ast).to eql(tree.to_ast)
  end

  it "rebuilds mutually recursive groups" do
    group = Dry::Types::Recursive.define do |r|
      r[:foo] = RecursiveAstTypes::Hash.schema(bar?: r[:bar])
      r[:bar] = RecursiveAstTypes::Hash.schema(foo?: r[:foo])
    end

    rebuilt_foo = compiler.(group.scope(:foo).to_ast)
    expect(rebuilt_foo.(bar: {})).to eql(bar: {})
  end

  it "prints without recursing forever" do
    Timeout.timeout(5) do
      string = tree.inspect
      expect(string).to start_with("#<Dry::Types[Recursive(condition)")
      expect(string).to include("Recursive<condition>")
    end
  end

  it "preserves meta on the scope" do
    with_meta = tree.meta(kind: :approval)
    rebuilt = compiler.(with_meta.to_ast)

    expect(rebuilt.meta).to include(kind: :approval)
  end
end