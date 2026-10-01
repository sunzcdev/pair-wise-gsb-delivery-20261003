# frozen_string_literal: true
require 'support'
require 'mustermann'

describe Mustermann::Relation do
  def relation(*args, **options)
    Mustermann.relation(*args, **options)
  end

  def pattern(string, **options)
    Mustermann.new(string, **options)
  end

  describe :intersection do
    example 'two plain captures intersect with a witness' do
      result = relation('/users/:id', '/users/:name')
      result.should be_intersect
      result.witness.should be_a(String)
      pattern('/users/:id').should     match(result.witness)
      pattern('/users/:name').should   match(result.witness)
    end

    example 'distinct literals are disjoint' do
      result = relation('/a', '/b')
      result.should_not be_intersect
      result.should be_disjoint
      result.witness.should be_nil
    end

    example 'different fixed extensions are disjoint' do
      result = relation('/:file.png', '/:file.jpg')
      result.should be_disjoint
    end

    example 'splat and capture intersect' do
      relation('/*rest', '/:name').should be_intersect
    end

    example 'optional segments intersect their mandatory version' do
      relation('/:a(.:ext)?', '/:a').should be_intersect
    end

    example 'patterns that look overlapping but share no string are disjoint' do
      # /v1/... and /v2/... only differ by a literal, sampling could miss it
      relation('/v1/:id', '/v2/:id').should be_disjoint
    end

    example 'non-ASCII literals are compared without relying on samples' do
      result = relation("/caf\u00e9", "/caf\u00e9")
      result.should be_intersect
      result.subset?.should == true

      relation("/caf\u00e9", "/caf\u00e8").should be_disjoint
      relation("/caf\u00e9", "/caf\u00e9/:x").should be_disjoint
    end

    example 'unions intersect when any pair of alternatives intersects' do
      result = relation(pattern('/a') | pattern('/b'), pattern('/b') | pattern('/c'))
      result.should be_intersect
      pattern('/b').should match(result.witness)
    end

    example 'disjoint unions' do
      result = relation(pattern('/a') | pattern('/b'), pattern('/c') | pattern('/d'))
      result.should be_disjoint
    end
  end

  describe :containment do
    example 'the same capture pattern contains itself' do
      relation('/users/:id', '/users/:id').should be_subset
    end

    example 'the optional-suffix version is contained in the plain capture' do
      relation('/:id(.:format)?', '/:id').should be_subset
    end

    example 'the plain capture is not contained in the optional-suffix one' do
      result = relation('/:id', '/:id(.:format)?')
      result.should_not be_subset
      pattern('/:id').should              match(result.counterexample)
      pattern('/:id(.:format)?').should_not match(result.counterexample)
    end

    example 'a literal is contained in a capture' do
      relation('/foo', '/:name').should be_subset
    end

    example 'a fixed path is contained under a splat prefix' do
      relation('/a/b', '/a/*rest').should be_subset
    end

    example 'a sibling literal is disjoint and therefore not contained' do
      relation('/a', '/b').should_not be_subset
    end

    example 'containment of unions requires every alternative to be covered' do
      result = relation(pattern('/a') | pattern('/c'),
                        pattern('/a') | pattern('/b'))
      result.should_not be_subset
      result.counterexample.should == '/c'
    end

    example 'a union is contained when all alternatives are covered' do
      relation(pattern('/a') | pattern('/b'), pattern('/:x')).should be_subset
    end
  end

  describe :witnesses do
    example 'intersection witnesses match both patterns via the public API' do
      result = relation('/:a/:b', '/:a/:c')
      result.witness.should be_a(String)
      (pattern('/:a/:b') === result.witness).should be true
      (pattern('/:a/:c') === result.witness).should be true
      result.should be_verified
    end

    example 'counterexamples match only the first pattern' do
      result = relation('/:x.png', '/:x.jpg')
      (pattern('/:x.png') === result.counterexample).should be true
      (pattern('/:x.jpg') === result.counterexample).should be false
      result.should be_verified
    end

    example 'witnesses are readable and contain no control characters' do
      result = relation('/users/:id', '/users/:name')
      result.witness.each_char do |char|
        (char >= ' ').should be true
      end
    end
  end

  describe :unknown do
    example 'a custom Regexp capture constraint yields an unknown result' do
      constrained = pattern('/:id', capture: /\d+/)
      result = relation(constrained, '/:other')
      result.should be_unknown
      result.intersect?.should be_nil
      result.subset?.should be_nil
      result.reason.should include('Regexp')
    end

    example 'the except option yields an unknown result with a reason' do
      result = relation(pattern('/:x', except: '/foo'), '/:y')
      result.should be_unknown
      result.reason.should include('except')
    end

    example 'an intersection composite cannot be flattened to a union' do
      result = relation(pattern('/:x') & pattern('/a'), pattern('/a'))
      result.should be_unknown
    end

    example 'a raw Regexp pattern is reported unknown' do
      result = relation(pattern(/.*/, type: :regexp), '/:x')
      result.should be_unknown
    end
  end

  describe :encoding do
    example 'patterns are compared by language, not source text' do
      decoded = pattern('/a b')
      encoded = pattern('/a b')
      relation(decoded, encoded).should be_subset
    end

    example 'different uri_decode options are still analyzed structurally' do
      decoded = pattern('/a')
      raw     = pattern('/%61', uri_decode: false)
      # With decoding enabled "/%61" matches "/a"; with it disabled only the
      # literal four-character string matches, so the languages differ.
      (decoded === '/%61').should be true
      (raw     === '/%61').should be true
      (raw     === '/a').should   be false
      relation(decoded, raw).should_not be_subset
    end
  end

  describe :result do
    example 'answers are booleans when decidable' do
      result = relation('/a', '/:x')
      result.intersect?.should == true
      result.subset?.should == true
      result.unknown?.should == false
    end

    describe 'Pattern#relation_to' do
      example 'exposes the analysis through an instance method' do
        result = pattern('/users/:id').relation_to('/users/:name')
        result.should be_intersect
        result.witness.should be_a(String)
      end
    end
  end
end
