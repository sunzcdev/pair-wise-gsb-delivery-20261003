require_relative "../spec_helper"

describe "not_allowed plugin" do 
  it "skips the current block if pass is called" do
    app(:not_allowed) do |r|
      r.root do
        'a'
      end

      r.is "c" do
        r.get do
          "cg"
        end

        r.post do
          "cp"
        end
      end

      r.on "q" do
        r.is do
          r.get do
            "q"
          end
        end
      end

      r.get do
        r.is 'b' do
          'b'
        end
        r.is(/(d)/) do |s|
          s
        end
        r.get(/(e)/) do |s|
          s
        end
      end
    end

    body.must_equal 'a'
    s, h, b = req('REQUEST_METHOD'=>'POST')
    s.must_equal 405
    h[RodaResponseHeaders::ALLOW].must_equal 'GET'
    b.must_be_empty

    body('/b').must_equal 'b'
    status('/b', 'REQUEST_METHOD'=>'POST').must_equal 404

    body('/d').must_equal 'd'
    status('/d', 'REQUEST_METHOD'=>'POST').must_equal 404

    body('/e').must_equal 'e'
    status('/e', 'REQUEST_METHOD'=>'POST').must_equal 404

    body('/q').must_equal 'q'
    s, _, b = req('/q', 'REQUEST_METHOD'=>'POST')
    s.must_equal 405
    b.must_be_empty

    body('/c').must_equal 'cg'
    body('/c', 'REQUEST_METHOD'=>'POST').must_equal 'cp'
    s, h, b = req('/c', 'REQUEST_METHOD'=>'PATCH')
    s.must_equal 405
    h[RodaResponseHeaders::ALLOW].must_equal 'GET, POST'
    b.must_be_empty

    @app.plugin :head
    header(RodaResponseHeaders::ALLOW, 'REQUEST_METHOD'=>'POST').must_equal 'HEAD, GET'
    header(RodaResponseHeaders::ALLOW, '/c', 'REQUEST_METHOD'=>'PATCH').must_equal 'HEAD, GET, POST'
    
    @app.plugin :status_handler
    @app.status_handler(405, :keep_headers=>[RodaResponseHeaders::ALLOW]){'a'}

    s, h, b = req('REQUEST_METHOD'=>'POST')
    s.must_equal 405
    h[RodaResponseHeaders::ALLOW].must_equal 'HEAD, GET'
    b.must_equal ['a']

    s, h, b = req('/c', 'REQUEST_METHOD'=>'PATCH')
    s.must_equal 405
    h[RodaResponseHeaders::ALLOW].must_equal 'HEAD, GET, POST'
    b.must_equal ['a']
  end
end
