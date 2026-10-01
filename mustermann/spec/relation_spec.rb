# frozen_string_literal: true
require 'support'
require 'mustermann/sinatra'
require 'mustermann/identity'
require 'mustermann/relation'

describe Mustermann::Relation do
  def relation(left, right, **options)
    left  = Mustermann::Sinatra.new(left, **options)  if left.is_a?(String)
    right = Mustermann::Sinatra.new(right, **options) if right.is_a?(String)
    Mustermann.relate(left, right)
  end

  describe 'intersection' do
    example 'two identical literal segments intersect' do
      relation('/users', '/users').tap do |r|
        r.intersect?.should be == true
        r.witness.should be == '/users'
      end
    end

    example 'disjoint literal prefixes do not intersect' do
      relation('/users', '/posts').intersect?.should be == false
    end

    example 'two ordinary captures intersect' do
      relation('/users/:id', '/users/:name').intersect?.should be == true
    end

    example 'a literal is reached by a capture' do
      relation('/:id', '/42').intersect?.should be == true
    end

    example 'splats and captures intersect' do
      relation('/*rest', '/:id').intersect?.should be == true
    end

    example 'different static structures can be disjoint' do
      relation('/a/b', '/a/c').intersect?.should be == false
    end

    example 'in-pattern unions behave as alternative captures' do
      # {a|b} parses as a union of two named captures, so /c/:x is reached.
      relation('/{a|b}/:x', '/c/:x').intersect?.should be == true
      relation('/(a|b)/:x', '/c/:x').intersect?.should be == false
      relation('/(a|b)/:x', '/a/:x').intersect?.should be == true
    end

    example 'an optional segment intersects both branches' do
      relation('/x(/:y)?', '/x').intersect?.should be == true
      relation('/x(/:y)?', '/x/1').intersect?.should be == true
    end

    example 'the conclusion is not based on the pattern source string' do
      # Same language, different pattern spelling and encoding option.
      left  = Mustermann::Sinatra.new('/:id')
      right = Mustermann::Identity.new('/a')
      Mustermann.relate(left, right).intersect?.should be == true
    end

    example 'disjoint patterns with a superficial textual overlap' do
      # Both contain "a" as text but no concrete path matches both.
      relation('/a/b', '/x/a').intersect?.should be == false
    end
  end

  describe 'containment' do
    example 'a single literal path is contained in a capture segment' do
      relation('/a', '/:x').subset?.should be == true
    end

    example 'a capture segment is not contained in a literal' do
      relation('/:x', '/a').subset?.should be == false
      relation('/:x', '/a').counterexample.should_not be_nil
    end

    example 'an optional segment widens the language' do
      relation('/x', '/x(/:y)?').subset?.should be == true
      relation('/x(/:y)?', '/x').subset?.should be == false
    end

    example 'typed and untyped captures' do
      integer = Mustermann::Sinatra.new('/:id', capture: Integer)
      Mustermann.relate(integer, Mustermann::Sinatra.new('/:id')).subset?.should be == true
      Mustermann.relate(Mustermann::Sinatra.new('/:id'), integer).subset?.should be == false
    end

    example 'an except option narrows the language' do
      restricted = Mustermann::Sinatra.new('/auth/*', except: '/auth/login')
      Mustermann.relate(restricted, Mustermann::Sinatra.new('/auth/*')).subset?.should be == true
      Mustermann.relate(restricted, Mustermann::Sinatra.new('/auth/login')).intersect?.should be == false
      Mustermann.relate(restricted, Mustermann::Sinatra.new('/auth/loginx')).intersect?.should be == true
    end
  end

  describe 'evidence' do
    example 'an intersection witness matches both real patterns' do
      r = relation('/users/:id', '/users/:name')
      r.left.match(r.witness).should be_a(Mustermann::Match)
      r.right.match(r.witness).should be_a(Mustermann::Match)
    end

    example 'a counterexample matches the left pattern but not the right' do
      r = relation('/:x', '/a')
      (r.left === r.counterexample).should be == true
      (r.right === r.counterexample).should be == false
    end

    example 'greedy captures with an optional suffix are handled' do
      r = relation('/:file(.:ext)?', '/:file')
      r.subset?.should be == true
      r = relation('/:file(.:ext)?/x', '/:file.png/x')
      r.intersect?.should be == true
    end
  end

  describe 'unknown constructs' do
    example 'arbitrary Regexp constraints produce an unknown answer with a reason' do
      constrained = Mustermann::Sinatra.new('/:id', capture: /\d+/)
      r = Mustermann.relate(constrained, Mustermann::Sinatra.new('/:name'))
      r.intersect?.should be_nil
      r.subset?.should be_nil
      r.unknown?.should be == true
      r.reasons.should_not be_empty
    end

    example 'non structural pattern types produce an unknown answer' do
      r = Mustermann.relate(Mustermann.new(/foo/), Mustermann::Sinatra.new('/foo'))
      r.intersect?.should be_nil
      r.reasons.first.should include('Regular')
    end
  end

  describe 'entry points' do
    example 'Mustermann.relate accepts strings' do
      Mustermann.relate('/a', '/a').intersect?.should be == true
    end

    example 'Pattern#relation_to returns a Relation' do
      Mustermann::Sinatra.new('/a').relation_to('/b').should be_a(described_class)
    end
  end

  describe 'robustness' do
    example 'a disjoint result gives no witness path' do
      relation('/a', '/b').witness.should be_nil
      relation('/a', '/b').intersect?.should be == false
    end

    example 'array capture constraints are modeled as alternatives' do
      png_or_jpg = Mustermann::Sinatra.new('/:e', capture: %w[png jpg])
      r = Mustermann.relate(png_or_jpg, Mustermann::Sinatra.new('/png'))
      r.intersect?.should be == true
      r = Mustermann.relate(png_or_jpg, Mustermann::Sinatra.new('/gif'))
      r.intersect?.should be == false
    end

    example 'composite unions built with | are analyzed' do
      union = Mustermann::Sinatra.new('/a') | Mustermann::Sinatra.new('/b')
      Mustermann.relate(union, Mustermann::Sinatra.new('/b')).intersect?.should be == true
      Mustermann.relate(union, Mustermann::Sinatra.new('/c')).intersect?.should be == false
    end

    example 'equivalent patterns compare as equivalent' do
      relation('/:x', '/:y').equivalent?.should be == true
      relation('/:x', '/a').equivalent?.should be == false
    end

    example 'POSIX symbol constraints are handled' do
      digits = Mustermann::Sinatra.new('/:d', capture: :digit)
      Mustermann.relate(digits, Mustermann::Sinatra.new('/7')).intersect?.should be == true
      Mustermann.relate(digits, Mustermann::Sinatra.new('/a')).intersect?.should be == false
    end

    example 'a splat reaching further is a strict superset of a capture' do
      r = relation('/*s', '/:x')
      r.intersect?.should be == true
      r.subset?.should be == false
    end
  end
end
