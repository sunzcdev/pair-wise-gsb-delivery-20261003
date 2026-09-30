# frozen_string_literal: true

# Copyright (C) Bob Aman
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


require "spec_helper"

require "bigdecimal"
require "timeout"
require "addressable/template"

shared_examples_for 'expands' do |tests|
  tests.each do |template, expansion|
    exp = expansion.is_a?(Array) ? expansion.first : expansion
    it "#{template} to #{exp}" do
      tmpl = Addressable::Template.new(template).expand(subject)
      expect(tmpl.to_str).to eq(expansion)
    end
  end
end

describe "eql?" do
  let(:template) { Addressable::Template.new('https://www.example.com/{foo}') }
  it 'is equal when the pattern matches' do
    other_template = Addressable::Template.new('https://www.example.com/{foo}')
    expect(template).to be_eql(other_template)
    expect(other_template).to be_eql(template)
  end
  it 'is not equal when the pattern differs' do
    other_template = Addressable::Template.new('https://www.example.com/{bar}')
    expect(template).to_not be_eql(other_template)
    expect(other_template).to_not be_eql(template)
  end
  it 'is not equal to non-templates' do
    uri = 'https://www.example.com/foo/bar'
    addressable_template = Addressable::Template.new uri
    addressable_uri = Addressable::URI.parse uri
    expect(addressable_template).to_not be_eql(addressable_uri)
    expect(addressable_uri).to_not be_eql(addressable_template)
  end
end

describe "==" do
  let(:template) { Addressable::Template.new('https://www.example.com/{foo}') }
  it 'is equal when the pattern matches' do
    other_template = Addressable::Template.new('https://www.example.com/{foo}')
    expect(template).to eq other_template
    expect(other_template).to eq template
  end
  it 'is not equal when the pattern differs' do
    other_template = Addressable::Template.new('https://www.example.com/{bar}')
    expect(template).not_to eq other_template
    expect(other_template).not_to eq template
  end
  it 'is not equal to non-templates' do
    uri = 'https://www.example.com/foo/bar'
    addressable_template = Addressable::Template.new uri
    addressable_uri = Addressable::URI.parse uri
    expect(addressable_template).not_to eq addressable_uri
    expect(addressable_uri).not_to eq addressable_template
  end
end

describe "#to_regexp" do
  it "does not match the first line of multiline strings" do
    uri = "https://www.example.com/bar"
    template = Addressable::Template.new(uri)
    expect(template.match(uri)).not_to be_nil
    expect(template.match("#{uri}\ngarbage")).to be_nil
  end
end

describe "Type conversion" do
  subject {
    {
      :var => true,
      :hello => 1234,
      :nothing => nil,
      :sym => :symbolic,
      :decimal => BigDecimal('1')
    }
  }

  it_behaves_like 'expands', {
    '{var}' => 'true',
    '{hello}' => '1234',
    '{nothing}' => '',
    '{sym}' => 'symbolic',
    '{decimal}' => RUBY_VERSION < '2.4.0' ? '0.1E1' : '0.1e1'
  }
end

describe "Level 1:" do
  subject {
    {:var => "value", :hello => "Hello World!"}
  }
  it_behaves_like 'expands', {
    '{var}' => 'value',
    '{hello}' => 'Hello%20World%21'
  }
end

describe "Level 2" do
  subject {
    {
      :var => "value",
      :hello => "Hello World!",
      :path => "/foo/bar"
    }
  }
  context "Operator +:" do
    it_behaves_like 'expands', {
      '{+var}' => 'value',
      '{+hello}' => 'Hello%20World!',
      '{+path}/here' => '/foo/bar/here',
      'here?ref={+path}' => 'here?ref=/foo/bar'
    }
  end
  context "Operator #:" do
    it_behaves_like 'expands', {
      'X{#var}' => 'X#value',
      'X{#hello}' => 'X#Hello%20World!'
    }
  end
end

describe "Level 3" do
  subject {
    {
      :var => "value",
      :hello => "Hello World!",
      :empty => "",
      :path => "/foo/bar",
      :x => "1024",
      :y => "768"
    }
  }
  context "Operator nil (multiple vars):" do
    it_behaves_like 'expands', {
      'map?{x,y}' => 'map?1024,768',
      '{x,hello,y}' => '1024,Hello%20World%21,768'
    }
  end
  context "Operator + (multiple vars):" do
    it_behaves_like 'expands', {
      '{+x,hello,y}' => '1024,Hello%20World!,768',
      '{+path,x}/here' => '/foo/bar,1024/here'
    }
  end
  context "Operator # (multiple vars):" do
    it_behaves_like 'expands', {
      '{#x,hello,y}' => '#1024,Hello%20World!,768',
      '{#path,x}/here' => '#/foo/bar,1024/here'
    }
  end
  context "Operator ." do
    it_behaves_like 'expands', {
      'X{.var}' => 'X.value',
      'X{.x,y}' => 'X.1024.768'
    }
  end
  context "Operator /" do
    it_behaves_like 'expands', {
      '{/var}' => '/value',
      '{/var,x}/here' => '/value/1024/here'
    }
  end
  context "Operator ;" do
    it_behaves_like 'expands', {
      '{;x,y}' => ';x=1024;y=768',
      '{;x,y,empty}' => ';x=1024;y=768;empty'
    }
  end
  context "Operator ?" do
    it_behaves_like 'expands', {
      '{?x,y}' => '?x=1024&y=768',
      '{?x,y,empty}' => '?x=1024&y=768&empty='
    }
  end
  context "Operator &" do
    it_behaves_like 'expands', {
      '?fixed=yes{&x}' => '?fixed=yes&x=1024',
      '{&x,y,empty}' => '&x=1024&y=768&empty='
    }
  end
end

describe "Level 4" do
  subject {
    {
      :var => "value",
      :hello => "Hello World!",
      :path => "/foo/bar",
      :semi => ";",
      :list => %w(red green blue),
      :keys => {"semi" => ';', "dot" => '.', :comma => ','}
    }
  }
  context "Expansion with value modifiers" do
    it_behaves_like 'expands', {
      '{var:3}' => 'val',
      '{var:30}' => 'value',
      '{list}' => 'red,green,blue',
      '{list*}' => 'red,green,blue',
      '{keys}' => 'semi,%3B,dot,.,comma,%2C',
      '{keys*}' => 'semi=%3B,dot=.,comma=%2C',
    }
  end
  context "Operator + with value modifiers" do
    it_behaves_like 'expands', {
      '{+path:6}/here' => '/foo/b/here',
      '{+list}' => 'red,green,blue',
      '{+list*}' => 'red,green,blue',
      '{+keys}' => 'semi,;,dot,.,comma,,',
      '{+keys*}' => 'semi=;,dot=.,comma=,',
    }
  end
  context "Operator # with value modifiers" do
    it_behaves_like 'expands', {
      '{#path:6}/here' => '#/foo/b/here',
      '{#list}' => '#red,green,blue',
      '{#list*}' => '#red,green,blue',
      '{#keys}' => '#semi,;,dot,.,comma,,',
      '{#keys*}' => '#semi=;,dot=.,comma=,',
    }
  end
  context "Operator . with value modifiers" do
    it_behaves_like 'expands', {
      'X{.var:3}' => 'X.val',
      'X{.list}' => 'X.red,green,blue',
      'X{.list*}' => 'X.red.green.blue',
      'X{.keys}' => 'X.semi,%3B,dot,.,comma,%2C',
      'X{.keys*}' => 'X.semi=%3B.dot=..comma=%2C',
    }
  end
  context "Operator / with value modifiers" do
    it_behaves_like 'expands', {
      '{/var:1,var}' => '/v/value',
      '{/list}' => '/red,green,blue',
      '{/list*}' => '/red/green/blue',
      '{/list*,path:4}' => '/red/green/blue/%2Ffoo',
      '{/keys}' => '/semi,%3B,dot,.,comma,%2C',
      '{/keys*}' => '/semi=%3B/dot=./comma=%2C',
    }
  end
  context "Operator ; with value modifiers" do
    it_behaves_like 'expands', {
      '{;hello:5}' => ';hello=Hello',
      '{;list}' => ';list=red,green,blue',
      '{;list*}' => ';list=red;list=green;list=blue',
      '{;keys}' => ';keys=semi,%3B,dot,.,comma,%2C',
      '{;keys*}' => ';semi=%3B;dot=.;comma=%2C',
    }
  end
  context "Operator ? with value modifiers" do
    it_behaves_like 'expands', {
      '{?var:3}' => '?var=val',
      '{?list}' => '?list=red,green,blue',
      '{?list*}' => '?list=red&list=green&list=blue',
      '{?keys}' => '?keys=semi,%3B,dot,.,comma,%2C',
      '{?keys*}' => '?semi=%3B&dot=.&comma=%2C',
    }
  end
  context "Operator & with value modifiers" do
    it_behaves_like 'expands', {
      '{&var:3}' => '&var=val',
      '{&list}' => '&list=red,green,blue',
      '{&list*}' => '&list=red&list=green&list=blue',
      '{&keys}' => '&keys=semi,%3B,dot,.,comma,%2C',
      '{&keys*}' => '&semi=%3B&dot=.&comma=%2C',
    }
  end
end
describe "Modifiers" do
  subject {
    {
      :var => "value",
      :semi => ";",
      :year => [1965, 2000, 2012],
      :dom => %w(example com)
    }
  }
  context "length" do
    it_behaves_like 'expands', {
      '{var:3}' => 'val',
      '{var:30}' => 'value',
      '{var}' => 'value',
      '{semi}' => '%3B',
      '{semi:2}' => '%3B'
    }
  end
  context "explode" do
    it_behaves_like 'expands', {
      'find{?year*}' => 'find?year=1965&year=2000&year=2012',
      'www{.dom*}' => 'www.example.com',
    }
  end
end
describe "Expansion" do
  subject {
    {
      :count => ["one", "two", "three"],
      :dom => ["example", "com"],
      :dub   => "me/too",
      :hello => "Hello World!",
      :half  => "50%",
      :var   => "value",
      :who   => "fred",
      :base  => "http://example.com/home/",
      :path  => "/foo/bar",
      :list  => ["red", "green", "blue"],
      :keys  => {"semi" => ";","dot" => ".",:comma => ","},
      :v     => "6",
      :x     => "1024",
      :y     => "768",
      :empty => "",
      :empty_keys  => {},
      :undef => nil
    }
  }
  context "concatenation" do
    it_behaves_like 'expands', {
      '{count}' => 'one,two,three',
      '{count*}' => 'one,two,three',
      '{/count}' => '/one,two,three',
      '{/count*}' => '/one/two/three',
      '{;count}' => ';count=one,two,three',
      '{;count*}' => ';count=one;count=two;count=three',
      '{?count}' => '?count=one,two,three',
      '{?count*}' => '?count=one&count=two&count=three',
      '{&count*}' => '&count=one&count=two&count=three'
    }
  end
  context "simple expansion" do
    it_behaves_like 'expands', {
      '{var}' => 'value',
      '{hello}' => 'Hello%20World%21',
      '{half}' => '50%25',
      'O{empty}X' => 'OX',
      'O{undef}X' => 'OX',
      '{x,y}' => '1024,768',
      '{x,hello,y}' => '1024,Hello%20World%21,768',
      '?{x,empty}' => '?1024,',
      '?{x,undef}' => '?1024',
      '?{undef,y}' => '?768',
      '{var:3}' => 'val',
      '{var:30}' => 'value',
      '{list}' => 'red,green,blue',
      '{list*}' => 'red,green,blue',
      '{keys}' => 'semi,%3B,dot,.,comma,%2C',
      '{keys*}' => 'semi=%3B,dot=.,comma=%2C',
    }
  end
  context "reserved expansion (+)" do
    it_behaves_like 'expands', {
      '{+var}' => 'value',
      '{+hello}' => 'Hello%20World!',
      '{+half}' => '50%25',
      '{base}index' => 'http%3A%2F%2Fexample.com%2Fhome%2Findex',
      '{+base}index' => 'http://example.com/home/index',
      'O{+empty}X' => 'OX',
      'O{+undef}X' => 'OX',
      '{+path}/here' => '/foo/bar/here',
      'here?ref={+path}' => 'here?ref=/foo/bar',
      'up{+path}{var}/here' => 'up/foo/barvalue/here',
      '{+x,hello,y}' => '1024,Hello%20World!,768',
      '{+path,x}/here' => '/foo/bar,1024/here',
      '{+path:6}/here' => '/foo/b/here',
      '{+list}' => 'red,green,blue',
      '{+list*}' => 'red,green,blue',
      '{+keys}' => 'semi,;,dot,.,comma,,',
      '{+keys*}' => 'semi=;,dot=.,comma=,',
    }
  end
  context "fragment expansion (#)" do
    it_behaves_like 'expands', {
      '{#var}' => '#value',
      '{#hello}' => '#Hello%20World!',
      '{#half}' => '#50%25',
      'foo{#empty}' => 'foo#',
      'foo{#undef}' => 'foo',
      '{#x,hello,y}' => '#1024,Hello%20World!,768',
      '{#path,x}/here' => '#/foo/bar,1024/here',
      '{#path:6}/here' => '#/foo/b/here',
      '{#list}' => '#red,green,blue',
      '{#list*}' => '#red,green,blue',
      '{#keys}' => '#semi,;,dot,.,comma,,',
      '{#keys*}' => '#semi=;,dot=.,comma=,',
    }
  end
  context "label expansion (.)" do
    it_behaves_like 'expands', {
      '{.who}' => '.fred',
      '{.who,who}' => '.fred.fred',
      '{.half,who}' => '.50%25.fred',
      'www{.dom*}' => 'www.example.com',
      'X{.var}' => 'X.value',
      'X{.empty}' => 'X.',
      'X{.undef}' => 'X',
      'X{.var:3}' => 'X.val',
      'X{.list}' => 'X.red,green,blue',
      'X{.list*}' => 'X.red.green.blue',
      'X{.keys}' => 'X.semi,%3B,dot,.,comma,%2C',
      'X{.keys*}' => 'X.semi=%3B.dot=..comma=%2C',
      'X{.empty_keys}' => 'X',
      'X{.empty_keys*}' => 'X'
    }
  end
  context "path expansion (/)" do
    it_behaves_like 'expands', {
      '{/who}' => '/fred',
      '{/who,who}' => '/fred/fred',
      '{/half,who}' => '/50%25/fred',
      '{/who,dub}' => '/fred/me%2Ftoo',
      '{/var}' => '/value',
      '{/var,empty}' => '/value/',
      '{/var,undef}' => '/value',
      '{/var,x}/here' => '/value/1024/here',
      '{/var:1,var}' => '/v/value',
      '{/list}' => '/red,green,blue',
      '{/list*}' => '/red/green/blue',
      '{/list*,path:4}' => '/red/green/blue/%2Ffoo',
      '{/keys}' => '/semi,%3B,dot,.,comma,%2C',
      '{/keys*}' => '/semi=%3B/dot=./comma=%2C',
    }
  end
  context "path-style expansion (;)" do
    it_behaves_like 'expands', {
      '{;who}' => ';who=fred',
      '{;half}' => ';half=50%25',
      '{;empty}' => ';empty',
      '{;v,empty,who}' => ';v=6;empty;who=fred',
      '{;v,bar,who}' => ';v=6;who=fred',
      '{;x,y}' => ';x=1024;y=768',
      '{;x,y,empty}' => ';x=1024;y=768;empty',
      '{;x,y,undef}' => ';x=1024;y=768',
      '{;hello:5}' => ';hello=Hello',
      '{;list}' => ';list=red,green,blue',
      '{;list*}' => ';list=red;list=green;list=blue',
      '{;keys}' => ';keys=semi,%3B,dot,.,comma,%2C',
      '{;keys*}' => ';semi=%3B;dot=.;comma=%2C',
    }
  end
  context "form query expansion (?)" do
    it_behaves_like 'expands', {
      '{?who}' => '?who=fred',
      '{?half}' => '?half=50%25',
      '{?x,y}' => '?x=1024&y=768',
      '{?x,y,empty}' => '?x=1024&y=768&empty=',
      '{?x,y,undef}' => '?x=1024&y=768',
      '{?var:3}' => '?var=val',
      '{?list}' => '?list=red,green,blue',
      '{?list*}' => '?list=red&list=green&list=blue',
      '{?keys}' => '?keys=semi,%3B,dot,.,comma,%2C',
      '{?keys*}' => '?semi=%3B&dot=.&comma=%2C',
    }
  end
  context "form query expansion (&)" do
    it_behaves_like 'expands', {
      '{&who}' => '&who=fred',
      '{&half}' => '&half=50%25',
      '?fixed=yes{&x}' => '?fixed=yes&x=1024',
      '{&x,y,empty}' => '&x=1024&y=768&empty=',
      '{&x,y,undef}' => '&x=1024&y=768',
      '{&var:3}' => '&var=val',
      '{&list}' => '&list=red,green,blue',
      '{&list*}' => '&list=red&list=green&list=blue',
      '{&keys}' => '&keys=semi,%3B,dot,.,comma,%2C',
      '{&keys*}' => '&semi=%3B&dot=.&comma=%2C',
    }
  end
  context "non-string key in match data" do
    subject {Addressable::Template.new("http://example.com/{one}")}

    it "raises TypeError" do
      expect { subject.expand(Object.new => "1") }.to raise_error TypeError
    end
  end
end

class ExampleTwoProcessor
  def self.restore(name, value)
    return value.gsub(/-/, " ") if name == "query"
    return value
  end

  def self.match(name)
    return ".*?" if name == "first"
    return ".*"
  end
  def self.validate(name, value)
    return !!(value =~ /^[\w ]+$/) if name == "query"
    return true
  end

  def self.transform(name, value)
    return value.gsub(/ /, "+") if name == "query"
    return value
  end
end

class DumbProcessor
  def self.match(name)
    return ".*?" if name == "first"
  end
end

describe Addressable::Template do
  describe 'initialize' do
    context 'with a non-string' do
      it 'raises a TypeError' do
        expect { Addressable::Template.new(nil) }.to raise_error(TypeError)
      end
    end
  end

  describe 'freeze' do
    subject { Addressable::Template.new("http://example.com/{first}/{+second}/") }
    it 'freezes the template' do
      expect(subject.freeze).to be_frozen
    end
  end

  describe "Matching" do
    let(:uri){
      Addressable::URI.parse(
        "http://example.com/search/an-example-search-query/"
      )
    }
    let(:uri2){
      Addressable::URI.parse("http://example.com/a/b/c/")
    }
    let(:uri3){
      Addressable::URI.parse("http://example.com/;a=1;b=2;c=3;first=foo")
    }
    let(:uri4){
      Addressable::URI.parse("http://example.com/?a=1&b=2&c=3&first=foo")
    }
    let(:uri5){
      "http://example.com/foo"
    }
    context "first uri with ExampleTwoProcessor" do
      subject {
        Addressable::Template.new(
          "http://example.com/search/{query}/"
        ).match(uri, ExampleTwoProcessor)
      }
      its(:variables){ should == ["query"] }
      its(:captures){ should == ["an example search query"] }
    end

    context "second uri with ExampleTwoProcessor" do
      subject {
        Addressable::Template.new(
          "http://example.com/{first}/{+second}/"
        ).match(uri2, ExampleTwoProcessor)
      }
      its(:variables){ should == ["first", "second"] }
      its(:captures){ should == ["a", "b/c"] }
    end

    context "second uri with DumbProcessor" do
      subject {
        Addressable::Template.new(
          "http://example.com/{first}/{+second}/"
        ).match(uri2, DumbProcessor)
      }
      its(:variables){ should == ["first", "second"] }
      its(:captures){ should == ["a", "b/c"] }
    end

    context "second uri" do
      subject {
        Addressable::Template.new(
          "http://example.com/{first}{/second*}/"
        ).match(uri2)
      }
      its(:variables){ should == ["first", "second"] }
      its(:captures){ should == ["a", ["b","c"]] }
    end
    context "third uri" do
      subject {
        Addressable::Template.new(
          "http://example.com/{;hash*,first}"
        ).match(uri3)
      }
      its(:variables){ should == ["hash", "first"] }
      its(:captures){ should == [
        {"a" => "1", "b" => "2", "c" => "3", "first" => "foo"}, nil] }
    end
    # Note that this expansion is impossible to revert deterministically - the
    # * operator means first could have been a key of hash or a separate key.
    # Semantically, a separate key is more likely, but both are possible.
    context "fourth uri" do
      subject {
        Addressable::Template.new(
          "http://example.com/{?hash*,first}"
        ).match(uri4)
      }
      its(:variables){ should == ["hash", "first"] }
      its(:captures){ should == [
        {"a" => "1", "b" => "2", "c" => "3", "first"=> "foo"}, nil] }
    end
    context "fifth uri" do
      subject {
        Addressable::Template.new(
          "http://example.com/{path}{?hash*,first}"
        ).match(uri5)
      }
      its(:variables){ should == ["path", "hash", "first"] }
      its(:captures){ should == ["foo", nil, nil] }
    end
  end

  describe 'match' do
    subject { Addressable::Template.new('http://example.com/first/second/') }
    context 'when the URI is the same as the template' do
      it 'returns the match data itself with an empty mapping' do
        uri = Addressable::URI.parse('http://example.com/first/second/')
        match_data = subject.match(uri)
        expect(match_data).to be_an Addressable::Template::MatchData
        expect(match_data.uri).to eq(uri)
        expect(match_data.template).to eq(subject)
        expect(match_data.mapping).to be_empty
        expect(match_data.inspect).to be_an String
      end
    end
  end

  describe "extract" do
    let(:template) {
      Addressable::Template.new(
        "http://{host}{/segments*}/{?one,two,bogus}{#fragment}"
      )
    }
    let(:uri){ "http://example.com/a/b/c/?one=1&two=2#foo" }
    let(:uri2){ "http://example.com/a/b/c/#foo" }
    it "should be able to extract with queries" do
      expect(template.extract(uri)).to eq({
        "host" => "example.com",
        "segments" => %w(a b c),
        "one" => "1",
        "bogus" => nil,
        "two" => "2",
        "fragment" => "foo"
      })
    end
    it "should be able to extract without queries" do
      expect(template.extract(uri2)).to eq({
        "host" => "example.com",
        "segments" => %w(a b c),
        "one" => nil,
        "bogus" => nil,
        "two" => nil,
        "fragment" => "foo"
      })
    end

    context "issue #137" do
      subject { Addressable::Template.new('/path{?page,per_page}') }

      it "can match empty" do
        data = subject.extract("/path")
        expect(data["page"]).to eq(nil)
        expect(data["per_page"]).to eq(nil)
        expect(data.keys.sort).to eq(['page', 'per_page'])
      end

      it "can match first var" do
        data = subject.extract("/path?page=1")
        expect(data["page"]).to eq("1")
        expect(data["per_page"]).to eq(nil)
        expect(data.keys.sort).to eq(['page', 'per_page'])
      end

      it "can match second var" do
        data = subject.extract("/path?per_page=1")
        expect(data["page"]).to eq(nil)
        expect(data["per_page"]).to eq("1")
        expect(data.keys.sort).to eq(['page', 'per_page'])
      end

      it "can match both vars" do
        data = subject.extract("/path?page=2&per_page=1")
        expect(data["page"]).to eq("2")
        expect(data["per_page"]).to eq("1")
        expect(data.keys.sort).to eq(['page', 'per_page'])
      end
    end
  end

  describe "Partial expand with symbols" do
    context "partial_expand with two simple values" do
      subject {
        Addressable::Template.new("http://example.com/{one}/{two}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand(:one => "1").pattern).to eq(
          "http://example.com/1/{two}/"
        )
      end
    end
    context "partial_expand query with missing param in middle" do
      subject {
        Addressable::Template.new("http://example.com/{?one,two,three}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand(:one => "1", :three => "3").pattern).to eq(
          "http://example.com/?one=1{&two}&three=3/"
        )
      end
    end
    context "partial_expand form style query with missing param at beginning" do
      subject {
        Addressable::Template.new("http://example.com/{?one,two}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand(:two => "2").pattern).to eq(
          "http://example.com/?two=2{&one}/"
        )
      end
    end
    context "issue #307 - partial_expand form query with nil params" do
      subject do
        Addressable::Template.new("http://example.com/{?one,two,three}/")
      end
      it "builds a new pattern with two=nil" do
        expect(subject.partial_expand(two: nil).pattern).to eq(
          "http://example.com/{?one}{&three}/"
        )
      end
      it "builds a new pattern with one=nil and two=nil" do
        expect(subject.partial_expand(one: nil, two: nil).pattern).to eq(
          "http://example.com/{?three}/"
        )
      end
      it "builds a new pattern with one=1 and two=nil" do
        expect(subject.partial_expand(one: 1, two: nil).pattern).to eq(
          "http://example.com/?one=1{&three}/"
        )
      end
      it "builds a new pattern with one=nil and two=2" do
        expect(subject.partial_expand(one: nil, two: 2).pattern).to eq(
          "http://example.com/?two=2{&three}/"
        )
      end
      it "builds a new pattern with one=nil" do
        expect(subject.partial_expand(one: nil).pattern).to eq(
          "http://example.com/{?two}{&three}/"
        )
      end
    end
    context "partial_expand with query string" do
      subject {
        Addressable::Template.new("http://example.com/{?two,one}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand(:one => "1").pattern).to eq(
          "http://example.com/?one=1{&two}/"
        )
      end
    end
    context "partial_expand with path operator" do
      subject {
        Addressable::Template.new("http://example.com{/one,two}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand(:one => "1").pattern).to eq(
          "http://example.com/1{/two}/"
        )
      end
    end
    context "partial expand with unicode values" do
      subject do
        Addressable::Template.new("http://example.com/{resource}/{query}/")
      end
      it "normalizes unicode by default" do
        template = subject.partial_expand("query" => "Cafe\u0301")
        expect(template.pattern).to eq(
          "http://example.com/{resource}/Caf%C3%A9/"
        )
      end

      it "normalizes as unicode even with wrong encoding specified" do
        template = subject.partial_expand("query" => "Cafe\u0301".b)
        expect(template.pattern).to eq(
          "http://example.com/{resource}/Caf%C3%A9/"
        )
      end

      it "raises on invalid unicode input" do
        expect {
          subject.partial_expand("query" => "M\xE9thode".b)
        }.to raise_error(ArgumentError, "invalid byte sequence in UTF-8")
      end

      it "does not normalize unicode when byte semantics requested" do
        template = subject.partial_expand({"query" => "Cafe\u0301"}, nil, false)
        expect(template.pattern).to eq(
          "http://example.com/{resource}/Cafe%CC%81/"
        )
      end
    end
  end
  describe "Partial expand with strings" do
    context "partial_expand with two simple values" do
      subject {
        Addressable::Template.new("http://example.com/{one}/{two}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand("one" => "1").pattern).to eq(
          "http://example.com/1/{two}/"
        )
      end
    end
    context "partial_expand query with missing param in middle" do
      subject {
        Addressable::Template.new("http://example.com/{?one,two,three}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand("one" => "1", "three" => "3").pattern).to eq(
          "http://example.com/?one=1{&two}&three=3/"
        )
      end
    end
    context "partial_expand with query string" do
      subject {
        Addressable::Template.new("http://example.com/{?two,one}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand("one" => "1").pattern).to eq(
          "http://example.com/?one=1{&two}/"
        )
      end
    end
    context "partial_expand with path operator" do
      subject {
        Addressable::Template.new("http://example.com{/one,two}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand("one" => "1").pattern).to eq(
          "http://example.com/1{/two}/"
        )
      end
    end
  end
  describe "Expand" do
    context "expand with unicode values" do
      subject do
        Addressable::Template.new("http://example.com/search/{query}/")
      end
      it "normalizes unicode by default" do
        uri = subject.expand("query" => "Cafe\u0301").to_str
        expect(uri).to eq("http://example.com/search/Caf%C3%A9/")
      end

      it "normalizes as unicode even with wrong encoding specified" do
        uri = subject.expand("query" => "Cafe\u0301".b).to_str
        expect(uri).to eq("http://example.com/search/Caf%C3%A9/")
      end

      it "raises on invalid unicode input" do
        expect {
          subject.expand("query" => "M\xE9thode".b).to_str
        }.to raise_error(ArgumentError, "invalid byte sequence in UTF-8")
      end

      it "does not normalize unicode when byte semantics requested" do
        uri = subject.expand({ "query" => "Cafe\u0301" }, nil, false).to_str
        expect(uri).to eq("http://example.com/search/Cafe%CC%81/")
      end
    end
    context "expand with a processor" do
      subject {
        Addressable::Template.new("http://example.com/search/{query}/")
      }
      it "processes spaces" do
        expect(subject.expand({"query" => "an example search query"},
                      ExampleTwoProcessor).to_str).to eq(
          "http://example.com/search/an+example+search+query/"
        )
      end
      it "validates" do
        expect{
          subject.expand({"query" => "Bogus!"},
                      ExampleTwoProcessor).to_str
        }.to raise_error(Addressable::Template::InvalidTemplateValueError)
      end
    end
    context "partial_expand query with missing param in middle" do
      subject {
        Addressable::Template.new("http://example.com/{?one,two,three}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand("one" => "1", "three" => "3").pattern).to eq(
          "http://example.com/?one=1{&two}&three=3/"
        )
      end
    end
    context "partial_expand with query string" do
      subject {
        Addressable::Template.new("http://example.com/{?two,one}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand("one" => "1").pattern).to eq(
          "http://example.com/?one=1{&two}/"
        )
      end
    end
    context "partial_expand with path operator" do
      subject {
        Addressable::Template.new("http://example.com{/one,two}/")
      }
      it "builds a new pattern" do
        expect(subject.partial_expand("one" => "1").pattern).to eq(
          "http://example.com/1{/two}/"
        )
      end
    end
  end
  context "Matching with operators" do
    describe "Level 1:" do
      subject { Addressable::Template.new("foo{foo}/{bar}baz") }
      it "can match" do
        data = subject.match("foofoo/bananabaz")
        expect(data.mapping["foo"]).to eq("foo")
        expect(data.mapping["bar"]).to eq("banana")
      end
      it "can fail" do
        expect(subject.match("bar/foo")).to be_nil
        expect(subject.match("foobaz")).to be_nil
      end
      it "can match empty" do
        data = subject.match("foo/baz")
        expect(data.mapping["foo"]).to eq(nil)
        expect(data.mapping["bar"]).to eq(nil)
      end
      it "lists vars" do
        expect(subject.variables).to eq(["foo", "bar"])
      end
    end

    describe "Level 2:" do
      subject { Addressable::Template.new("foo{+foo}{#bar}baz") }
      it "can match" do
        data = subject.match("foo/test/banana#bazbaz")
        expect(data.mapping["foo"]).to eq("/test/banana")
        expect(data.mapping["bar"]).to eq("baz")
      end
      it "can match empty level 2 #" do
        data = subject.match("foo/test/bananabaz")
        expect(data.mapping["foo"]).to eq("/test/banana")
        expect(data.mapping["bar"]).to eq(nil)
        data = subject.match("foo/test/banana#baz")
        expect(data.mapping["foo"]).to eq("/test/banana")
        expect(data.mapping["bar"]).to eq("")
      end
      it "can match empty level 2 +" do
        data = subject.match("foobaz")
        expect(data.mapping["foo"]).to eq(nil)
        expect(data.mapping["bar"]).to eq(nil)
        data = subject.match("foo#barbaz")
        expect(data.mapping["foo"]).to eq(nil)
        expect(data.mapping["bar"]).to eq("bar")
      end
      it "lists vars" do
        expect(subject.variables).to eq(["foo", "bar"])
      end
    end

    describe "Level 3:" do
      context "no operator" do
        subject { Addressable::Template.new("foo{foo,bar}baz") }
        it "can match" do
          data = subject.match("foofoo,barbaz")
          expect(data.mapping["foo"]).to eq("foo")
          expect(data.mapping["bar"]).to eq("bar")
        end
        it "lists vars" do
          expect(subject.variables).to eq(["foo", "bar"])
        end
      end
      context "+ operator" do
        subject { Addressable::Template.new("{+foo,bar}") }
        it "can match" do
          data = subject.match("foo/bar,baz")
          expect(data.mapping["foo"]).to eq("foo/bar")
          expect(data.mapping["bar"]).to eq("baz")
        end
        it "lists vars" do
          expect(subject.variables).to eq(["foo", "bar"])
        end
      end
      context "+ operator with explode modifier" do
        subject { Addressable::Template.new("{+foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a," * 1000) + " ")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "# operator with explode modifier" do
        subject { Addressable::Template.new("{#foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("#" + "a," * 1000) + " ")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "+ operator with explode modifier and surrounding literals" do
        subject { Addressable::Template.new("x{+foo*}y") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match("x" + ("a," * 1000) + " ")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "+ operator with multiple explode-modified variables" do
        subject { Addressable::Template.new("{+foo*,bar*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a," * 1000) + " ")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "default operator with explode modifier" do
        subject { Addressable::Template.new("{foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a," * 1000) + "!")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "/ operator with explode modifier" do
        subject { Addressable::Template.new("{/foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a/" * 1000) + "!")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context ". operator with explode modifier" do
        subject { Addressable::Template.new("{.foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a." * 1000) + "!")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "; operator with explode modifier" do
        subject { Addressable::Template.new("{;foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a=1;" * 1000) + "!")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "? operator with explode modifier" do
        subject { Addressable::Template.new("{?foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match("?" + ("a=1&" * 1000) + "!")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "& operator with explode modifier" do
        subject { Addressable::Template.new("{&foo*}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("&a=1" * 1000) + "!")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "many variables in a single + expression" do
        subject { Addressable::Template.new("{+v1,v2,v3,v4,v5,v6,v7,v8,v9,v10}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a," * 1000) + " ")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "consecutive + expressions without separators" do
        subject { Addressable::Template.new("{+a}{+b}{+c}") }
        it "should match in linear time against a non-matching payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("a" * 1000) + " ")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context "percent-encoded payload with + explode modifier" do
        subject { Addressable::Template.new("{+foo*}") }
        it "should match in linear time against a non-matching percent-encoded payload" do
          expect do
            Timeout.timeout(10) do
              expect(subject.match(("%2F" * 1000) + " ")).to be_nil
            end
          end.not_to raise_error
        end
      end
      context ". operator" do
        subject { Addressable::Template.new("foo{.foo,bar}baz") }
        it "can match" do
          data = subject.match("foo.foo.barbaz")
          expect(data.mapping["foo"]).to eq("foo")
          expect(data.mapping["bar"]).to eq("bar")
        end
        it "lists vars" do
          expect(subject.variables).to eq(["foo", "bar"])
        end
      end
      context "/ operator" do
        subject { Addressable::Template.new("foo{/foo,bar}baz") }
        it "can match" do
          data = subject.match("foo/foo/barbaz")
          expect(data.mapping["foo"]).to eq("foo")
          expect(data.mapping["bar"]).to eq("bar")
        end
        it "lists vars" do
          expect(subject.variables).to eq(["foo", "bar"])
        end
      end
      context "; operator" do
        subject { Addressable::Template.new("foo{;foo,bar,baz}baz") }
        it "can match" do
          data = subject.match("foo;foo=bar%20baz;bar=foo;bazbaz")
          expect(data.mapping["foo"]).to eq("bar baz")
          expect(data.mapping["bar"]).to eq("foo")
          expect(data.mapping["baz"]).to eq("")
        end
        it "lists vars" do
          expect(subject.variables).to eq(%w(foo bar baz))
        end
      end
      context "? operator" do
        context "test" do
          subject { Addressable::Template.new("foo{?foo,bar}baz") }
          it "can match" do
            data = subject.match("foo?foo=bar%20baz&bar=foobaz")
            expect(data.mapping["foo"]).to eq("bar baz")
            expect(data.mapping["bar"]).to eq("foo")
          end
          it "lists vars" do
            expect(subject.variables).to eq(%w(foo bar))
          end
        end

        context "issue #137" do
          subject { Addressable::Template.new('/path{?page,per_page}') }

          it "can match empty" do
            data = subject.match("/path")
            expect(data.mapping["page"]).to eq(nil)
            expect(data.mapping["per_page"]).to eq(nil)
            expect(data.mapping.keys.sort).to eq(['page', 'per_page'])
          end

          it "can match first var" do
            data = subject.match("/path?page=1")
            expect(data.mapping["page"]).to eq("1")
            expect(data.mapping["per_page"]).to eq(nil)
            expect(data.mapping.keys.sort).to eq(['page', 'per_page'])
          end

          it "can match second var" do
            data = subject.match("/path?per_page=1")
            expect(data.mapping["page"]).to eq(nil)
            expect(data.mapping["per_page"]).to eq("1")
            expect(data.mapping.keys.sort).to eq(['page', 'per_page'])
          end

          it "can match both vars" do
            data = subject.match("/path?page=2&per_page=1")
            expect(data.mapping["page"]).to eq("2")
            expect(data.mapping["per_page"]).to eq("1")
            expect(data.mapping.keys.sort).to eq(['page', 'per_page'])
          end
        end

        context "issue #71" do
          subject { Addressable::Template.new("http://cyberscore.dev/api/users{?username}") }
          it "can match" do
            data = subject.match("http://cyberscore.dev/api/users?username=foobaz")
            expect(data.mapping["username"]).to eq("foobaz")
          end
          it "lists vars" do
            expect(subject.variables).to eq(%w(username))
            expect(subject.keys).to eq(%w(username))
          end
        end
      end
      context "& operator" do
        subject { Addressable::Template.new("foo{&foo,bar}baz") }
        it "can match" do
          data = subject.match("foo&foo=bar%20baz&bar=foobaz")
          expect(data.mapping["foo"]).to eq("bar baz")
          expect(data.mapping["bar"]).to eq("foo")
        end
        it "lists vars" do
          expect(subject.variables).to eq(%w(foo bar))
        end
      end
    end
  end

  context "support regexes:" do
    context "EXPRESSION" do
      subject { Addressable::Template::EXPRESSION }
      it "should be able to match an expression" do
        expect(subject).to match("{foo}")
        expect(subject).to match("{foo,9}")
        expect(subject).to match("{foo.bar,baz}")
        expect(subject).to match("{+foo.bar,baz}")
        expect(subject).to match("{foo,foo%20bar}")
        expect(subject).to match("{#foo:20,baz*}")
        expect(subject).to match("stuff{#foo:20,baz*}things")
      end
      it "should fail on non vars" do
        expect(subject).not_to match("!{foo")
        expect(subject).not_to match("{foo.bar.}")
        expect(subject).not_to match("!{}")
      end
    end
    context "VARNAME" do
      subject { Addressable::Template::VARNAME }
      it "should be able to match a variable" do
        expect(subject).to match("foo")
        expect(subject).to match("9")
        expect(subject).to match("foo.bar")
        expect(subject).to match("foo_bar")
        expect(subject).to match("foo_bar.baz")
        expect(subject).to match("foo%20bar")
        expect(subject).to match("foo%20bar.baz")
      end
      it "should fail on non vars" do
        expect(subject).not_to match("!foo")
        expect(subject).not_to match("foo.bar.")
        expect(subject).not_to match("foo%2%00bar")
        expect(subject).not_to match("foo_ba%r")
        expect(subject).not_to match("foo_bar*")
        expect(subject).not_to match("foo_bar:20")
      end

      it 'should parse in a reasonable time' do
        expect do
          Timeout.timeout(0.1) do
            expect(subject).not_to match("0"*25 + "!")
          end
        end.not_to raise_error
      end
    end
    context "VARIABLE_LIST" do
      subject { Addressable::Template::VARIABLE_LIST }
      it "should be able to match a variable list" do
        expect(subject).to match("foo,bar")
        expect(subject).to match("foo")
        expect(subject).to match("foo,bar*,baz")
        expect(subject).to match("foo.bar,bar_baz*,baz:12")
      end
      it "should fail on non vars" do
        expect(subject).not_to match(",foo,bar*,baz")
        expect(subject).not_to match("foo,*bar,baz")
        expect(subject).not_to match("foo,,bar*,baz")
      end
    end
    context "VARSPEC" do
      subject { Addressable::Template::VARSPEC }
      it "should be able to match a variable with modifier" do
        expect(subject).to match("9:8")
        expect(subject).to match("foo.bar*")
        expect(subject).to match("foo_bar:12")
        expect(subject).to match("foo_bar.baz*")
        expect(subject).to match("foo%20bar:12")
        expect(subject).to match("foo%20bar.baz*")
      end
      it "should fail on non vars" do
        expect(subject).not_to match("!foo")
        expect(subject).not_to match("*foo")
        expect(subject).not_to match("fo*o")
        expect(subject).not_to match("fo:o")
        expect(subject).not_to match("foo:")
      end
    end
  end
end

describe Addressable::Template::MatchData do
  let(:template) { Addressable::Template.new('{foo}/{bar}') }
  subject(:its) { template.match('ab/cd') }
  its(:uri) { should == Addressable::URI.parse('ab/cd') }
  its(:template) { should == template }
  its(:mapping) { should == { 'foo' => 'ab', 'bar' => 'cd' } }
  its(:variables) { should == ['foo', 'bar'] }
  its(:keys) { should == ['foo', 'bar'] }
  its(:names) { should == ['foo', 'bar'] }
  its(:values) { should == ['ab', 'cd'] }
  its(:captures) { should == ['ab', 'cd'] }
  its(:to_a) { should == ['ab/cd', 'ab', 'cd'] }
  its(:to_s) { should == 'ab/cd' }
  its(:string) { should == its.to_s }
  its(:pre_match) { should == "" }
  its(:post_match) { should == "" }

  describe 'values_at' do
    it 'returns an array with the values' do
      expect(its.values_at(0, 2)).to eq(['ab/cd', 'cd'])
    end
    it 'allows mixing integer an string keys' do
      expect(its.values_at('foo', 1)).to eq(['ab', 'ab'])
    end
    it 'accepts unknown keys' do
      expect(its.values_at('baz', 'foo')).to eq([nil, 'ab'])
    end
  end

  describe '[]' do
    context 'string key' do
      it 'returns the corresponding capture' do
        expect(its['foo']).to eq('ab')
        expect(its['bar']).to eq('cd')
      end
      it 'returns nil for unknown keys' do
        expect(its['baz']).to be_nil
      end
    end
    context 'symbol key' do
      it 'returns the corresponding capture' do
        expect(its[:foo]).to eq('ab')
        expect(its[:bar]).to eq('cd')
      end
      it 'returns nil for unknown keys' do
        expect(its[:baz]).to be_nil
      end
    end
    context 'integer key' do
      it 'returns the full URI for index 0' do
        expect(its[0]).to eq('ab/cd')
      end
      it 'returns the corresponding capture' do
        expect(its[1]).to eq('ab')
        expect(its[2]).to eq('cd')
      end
      it 'returns nil for unknown keys' do
        expect(its[3]).to be_nil
      end
    end
    context 'other key' do
      it 'raises an exception' do
        expect { its[Object.new] }.to raise_error(TypeError)
      end
    end
    context 'with length' do
      it 'returns an array starting at index with given length' do
        expect(its[0, 2]).to eq(['ab/cd', 'ab'])
        expect(its[2, 1]).to eq(['cd'])
      end
    end
  end
end

describe Addressable::Template::Binding do
  let(:template) do
    Addressable::Template.new("http://example.com/{one}/{two}/{three}")
  end

  describe Addressable::Template, "#bind" do
    it "returns a Binding" do
      expect(template.bind("one" => "1")).to be_a(
        Addressable::Template::Binding
      )
    end

    it "accepts symbol keys" do
      binding = template.bind(:one => "1")
      expect(binding.bound?(:one)).to eq(true)
    end

    it "does not modify the template" do
      template.bind("one" => "1")
      expect(template.pattern).to eq(
        "http://example.com/{one}/{two}/{three}"
      )
    end
  end

  describe "#variables" do
    it "lists the template variables in pattern order" do
      expect(template.bind({}).variables).to eq(["one", "two", "three"])
    end

    it "is aliased as keys and names" do
      binding = template.bind({})
      expect(binding.keys).to eq(binding.variables)
      expect(binding.names).to eq(binding.variables)
    end
  end

  describe "#missing_variables" do
    it "lists the variables that have not been bound" do
      binding = template.bind("one" => "1")
      expect(binding.missing_variables).to eq(["two", "three"])
    end

    it "treats an explicitly bound nil as present" do
      binding = template.bind("one" => nil)
      expect(binding.missing_variables).to eq(["two", "three"])
    end
  end

  describe "#bound?" do
    let(:binding) { template.bind("one" => "1") }

    it "returns true for variables with a value" do
      expect(binding.bound?("one")).to eq(true)
    end

    it "returns true when the value is nil" do
      expect(template.bind("one" => nil).bound?("one")).to eq(true)
    end

    it "returns false for unbound variables" do
      expect(binding.bound?("two")).to eq(false)
    end

    it "accepts symbol names" do
      expect(binding.bound?(:one)).to eq(true)
    end
  end

  describe "#complete?" do
    it "is false while variables are missing" do
      expect(template.bind("one" => "1").complete?).to eq(false)
    end

    it "is true once every variable has been bound" do
      binding = template.bind("one" => "1").
        bind("two" => "2").
        bind("three" => "3")
      expect(binding.complete?).to eq(true)
    end

    it "is aliased as ready?" do
      expect(template.bind({}).ready?).to eq(false)
    end
  end

  describe "#mapping" do
    it "returns the accumulated values" do
      binding = template.bind("one" => "1").bind("two" => "2")
      expect(binding.mapping).to eq("one" => "1", "two" => "2")
    end

    it "returns a frozen copy" do
      binding = template.bind("one" => "1")
      expect(binding.mapping).to be_frozen
      expect { binding.mapping["two"] = "2" }.to raise_error(
        FrozenError
      )
    end
  end

  describe "#bind" do
    it "accumulates values across batches" do
      binding = template.bind("one" => "1").
        bind("two" => "2").
        bind("three" => "3")
      expect(binding.mapping).to eq(
        "one" => "1", "two" => "2", "three" => "3"
      )
    end

    it "does not modify the receiver" do
      binding = template.bind("one" => "1")
      binding.bind("two" => "2")
      expect(binding.bound?("two")).to eq(false)
    end

    it "rejects re-binding a variable to a different value" do
      binding = template.bind("one" => "1")
      expect { binding.bind("one" => "2") }.to raise_error(
        Addressable::Template::InvalidTemplateValueError
      )
    end

    it "leaves the receiver usable after a rejected re-binding" do
      binding = template.bind("one" => "1")
      begin
        binding.bind("one" => "2")
      rescue Addressable::Template::InvalidTemplateValueError
      end
      completed = binding.bind("two" => "2").bind("three" => "3")
      expect(completed.expand.to_str).to eq(
        "http://example.com/1/2/3"
      )
    end

    it "allows re-binding to the same value" do
      binding = template.bind("one" => "1").
        bind("two" => "2").
        bind("three" => "3")
      expect(binding.bind("one" => "1").expand.to_str).to eq(
        "http://example.com/1/2/3"
      )
    end

    it "rejects replacing a bound nil with another value" do
      binding = template.bind("one" => nil)
      expect { binding.bind("one" => "1") }.to raise_error(
        Addressable::Template::InvalidTemplateValueError
      )
    end

    it "leaves the original template free to bind another dataset" do
      template.bind("one" => "1")
      other = template.bind("one" => "a").
        bind("two" => "b").
        bind("three" => "c")
      expect(other.expand.to_str).to eq(
        "http://example.com/a/b/c"
      )
    end

    it "ignores variables that are not used by the template" do
      binding = template.bind("one" => "1", "unknown" => "x").
        bind("two" => "2").
        bind("three" => "3")
      expect(binding.expand.to_str).to eq(
        "http://example.com/1/2/3"
      )
    end
  end

  describe "#expand" do
    it "produces the same URI as a one-shot expand" do
      binding = template.bind("one" => "1").
        bind("two" => "2").
        bind("three" => "3")
      expect(binding.expand).to be_a(Addressable::URI)
      expect(binding.expand.to_str).to eq(
        template.expand("one" => "1", "two" => "2", "three" => "3").to_str
      )
    end

    it "is aliased as to_uri" do
      binding = template.bind("one" => "1").
        bind("two" => "2").
        bind("three" => "3")
      expect(binding.to_uri.to_str).to eq(binding.expand.to_str)
    end

    it "raises before all variables are bound" do
      expect { template.bind("one" => "1").expand }.to raise_error(
        Addressable::Template::InvalidTemplateValueError, /two/
      )
    end

    it "forwards the processor and normalize_values options" do
      processor = Object.new
      def processor.validate(name, value)
        value !~ /!/
      end
      binding = template.bind({"one" => "a!"}, processor, false).
        bind("two" => "b").
        bind("three" => "c")
      expect { binding.expand }.to raise_error(
        Addressable::Template::InvalidTemplateValueError
      )
    end
  end

  # The key guarantee of incremental binding: no matter the order in
  # which a shared expression receives its variables, separators,
  # leaders, positions and encodings must match a one-shot expand.
  describe "byte-equivalence with one-shot expand" do
    CASES = [
      ["{x,y,z}",          { "x" => "a/b", "y" => ["p", "q"], "z" => nil }],
      ["{+x,y,z}",         { "x" => "a/b?c", "y" => "d,e", "z" => "r@f" }],
      ["{#x,y,z}",         {
        "x" => "/a/b", "y" => ["m", "n"], "z" => "k l"
      }],
      ["{.x,y,z}",         {
        "x" => "aa", "y" => ["bb", "cc"], "z" => nil
      }],
      ["{/x,y*,z}",        {
        "x" => "a", "y" => ["b", "c"], "z" => "d"
      }],
      ["{;x,y,z}",         { "x" => "a", "y" => nil, "z" => "c" }],
      ["{?x,y*,z}",        {
        "x" => nil, "y" => ["1", "2"], "z" => "3"
      }],
      ["{&x,y,z*}",        {
        "x" => "a", "y" => nil, "z" => { "k" => "v", "r" => "s" }
      }],
      ["search{?q,lang*}", {
        "q" => "hello world", "lang" => ["en", "fr"]
      }],
      ["{+path,query*}",   {
        "path" => "/foo/bar", "query" => { "a" => "1", "b" => "2" }
      }],
      ["{x:3,y*,z}",       {
        "x" => "abcdef", "y" => ["u v", "w"], "z" => nil
      }],
      ["http://ex.com/{a}{+b}/", { "a" => "x", "b" => "/y/z" }]
    ]

    CASES.each do |pattern, mapping|
      context pattern.inspect do
        let(:expected) do
          Addressable::Template.new(pattern).expand(mapping).to_str
        end

        it "matches when bound all at once" do
          got = Addressable::Template.new(pattern).bind(mapping).expand
          expect(got.to_str).to eq(expected)
        end

        mapping.keys.permutation.each do |order|
          it "matches when bound in order #{order.inspect}" do
            binding = Addressable::Template.new(pattern)
            order.each do |key|
              binding = binding.bind(key => mapping[key])
            end
            expect(binding.expand.to_str).to eq(expected)
          end
        end
      end
    end

    it "keeps bound nil in its query position for every binding order" do
      pattern = "http://example.com/{?one,two,three}/"
      mapping = { "one" => nil, "two" => "2", "three" => nil }
      expected = Addressable::Template.new(pattern).expand(mapping).to_str
      expect(expected).to eq("http://example.com/?two=2/")
      mapping.keys.permutation.each do |order|
        binding = Addressable::Template.new(pattern)
        order.each { |key| binding = binding.bind(key => mapping[key]) }
        expect(binding.expand.to_str).to eq(expected)
      end
    end
  end
end
