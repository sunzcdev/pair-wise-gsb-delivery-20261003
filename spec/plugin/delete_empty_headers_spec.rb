require_relative "../spec_helper"

describe "delete_empty_headers plugin" do 
  it "automatically deletes headers that are empty" do
    app(:delete_empty_headers) do |r|
      response['foo'] = ''
      response[RodaResponseHeaders::CONTENT_TYPE] = ''
      response[RodaResponseHeaders::CONTENT_LENGTH] = ''
      response['bar'] = '1'
      'a'
    end

    req[1].must_equal('bar'=>'1')
  end

  it "is called when finishing with a body" do
    app(:delete_empty_headers) do |r|
      response['foo'] = ''
      response[RodaResponseHeaders::CONTENT_TYPE] = ''
      response[RodaResponseHeaders::CONTENT_LENGTH] = ''
      response['bar'] = '1'
      r.halt response.finish_with_body(['a'])
    end

    req[1].must_equal('bar'=>'1')
  end
end
