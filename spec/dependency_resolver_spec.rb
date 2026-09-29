require 'spec_helper'
require 'dentaku/dependency_resolver'

describe Dentaku::DependencyResolver do
  it 'sorts expressions in dependency order' do
    dependencies = {"first" => ["second"], "second" => ["third"], "third" => []}
    expect(described_class.find_resolve_order(dependencies)).to eq(
      ["third", "second", "first"]
    )
  end

  it 'handles case differences' do
    dependencies = {"FIRST" => ["second"], "SeCoNd" => ["third"], "THIRD" => []}
    expect(described_class.find_resolve_order(dependencies)).to eq(
      ["THIRD", "SeCoNd", "FIRST"]
    )
  end

  it 'sorts mixed-case expressions in dependency order when case-sensitive' do
    dependencies = {"TopLevel" => ["MidLevel"], "MidLevel" => ["BottomLevel"], "BottomLevel" => []}
    expect(described_class.find_resolve_order(dependencies, true)).to eq(
      ["BottomLevel", "MidLevel", "TopLevel"]
    )
  end

  it 'treats variables that differ only in case as distinct when case-sensitive' do
    dependencies = {"foo" => ["FOO"], "FOO" => []}
    expect(described_class.find_resolve_order(dependencies, true)).to eq(
      ["FOO", "foo"]
    )
  end

  describe '.find_cycles' do
    it 'returns an empty list for an acyclic graph' do
      dependencies = {"first" => ["second"], "second" => ["third"], "third" => []}
      expect(described_class.find_cycles(dependencies)).to eq([])
    end

    it 'lists each independent cycle group' do
      dependencies = {
        "a" => ["b"], "b" => ["a"],
        "c" => ["d"], "d" => ["c"],
        "e" => ["a"],
        "f" => []
      }
      expect(described_class.find_cycles(dependencies)).to eq(
        [["a", "b"], ["c", "d"]]
      )
    end

    it 'detects self-references' do
      expect(described_class.find_cycles({"lonely" => ["lonely"]})).to eq([["lonely"]])
    end

    it 'maps cycle members back to their declared case' do
      dependencies = {"Alpha" => ["BETA"], "BETA" => ["alpha"]}
      expect(described_class.find_cycles(dependencies)).to eq([["Alpha", "BETA"]])
    end

    it 'does not merge variables that differ only in case when case-sensitive' do
      dependencies = {"foo" => ["bar"], "FOO" => ["foo"]}
      expect(described_class.find_cycles(dependencies, true)).to eq([])
    end
  end
end
