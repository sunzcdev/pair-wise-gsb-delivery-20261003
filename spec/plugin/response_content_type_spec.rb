require_relative "../spec_helper"

describe "response_content_type plugin" do 
  it "allows getting and setting content-type" do
    app(:response_content_type) do |r|
      r.get "a" do
        response.content_type = "text/plain"
        "a:#{response.content_type}"
      end
      ":#{response.content_type}"
    end

    _, h, b = req
    h[RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/html'
    b.must_equal [":"]

    _, h, b = req("/a")
    h[RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/plain'
    b.must_equal ["a:text/plain"]
  end

  it "supports using symbols with plugin :mime_types option, and raises if an invalid symbol is provided" do
    app(:bare) do
      plugin :response_content_type, mime_types: {:txt => "text/plain"}
      route do |r|
        r.get "a" do
          response.content_type = :bad
        end
        response.content_type = :txt
        response.content_type
      end
    end

    _, h, b = req
    h[RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/plain'
    b.must_equal ["text/plain"]

    proc{req("/a")}.must_raise KeyError
  end

  it "supports plugin mime_types: :from_rack_mime option" do
    app(:bare) do
      plugin :response_content_type, mime_types: :from_rack_mime
      route do |r|
        r.get String do |s|
          response.content_type = s.to_sym
          ""
        end
        response.content_type = :txt
        response.content_type
      end
    end

    2.times do
      _, h, b = req
      h[RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/plain'
      b.must_equal ["text/plain"]

      header(RodaResponseHeaders::CONTENT_TYPE, "/pdf").must_equal "application/pdf"

      proc{req("/invalid-mime-type")}.must_raise KeyError

      # test when loading the plugin more than once
      app.plugin :response_content_type
    end
  end
end
