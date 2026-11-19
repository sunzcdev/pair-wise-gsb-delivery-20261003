require_relative "../spec_helper"

describe "named_routes plugin" do 
  before do
    app(:bare) do
      plugin :named_routes

      route(:p) do |r|
        r.is do
          'p'
        end
      end

      route(:q, :b) do |r|
        r.is do
          'q'
        end
      end

      route do |r|
        r.on "p" do
          r.route(:p)
        end

        r.on "q" do
          r.route(:q, :b)
        end
      end
    end
  end

  it "adds named routing support" do
    body('/p').must_equal 'p'
    body('/q').must_equal 'q'

    status('/').must_equal 404
    status('/b').must_equal 404
    status('/p/').must_equal 404
    status('/q/a').must_equal 404
  end

  it "works when freezing the app" do
    app.freeze
    body('/p').must_equal 'p'
    body('/q').must_equal 'q'

    status('/').must_equal 404
    status('/b').must_equal 404
    status('/p/').must_equal 404
    status('/q/a').must_equal 404
  end

  it "works when subclassing the app" do
    @app = Class.new(@app)
    body('/p').must_equal 'p'
    body('/q').must_equal 'q'

    status('/').must_equal 404
    status('/b').must_equal 404
    status('/p/').must_equal 404
    status('/q/a').must_equal 404
  end

  it "allows removing a hash branch" do
    status('/p').must_equal 200
    2.times do
      app.route(:p)
      proc{status('/p')}.must_raise TypeError
    end
  end
end
