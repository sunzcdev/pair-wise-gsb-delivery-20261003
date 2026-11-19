require_relative "../spec_helper"

describe "map_matcher plugin" do 
  it "allows for matching next segment to a hash key, yielding hash value" do
    app(:bare) do 
      plugin :map_matcher
      route do |r|
        r.is :map=>{'a'=>'x', 'b'=>'y'} do |f|
          f
        end
        r.is({:map=>{'a'=>'x', 'b'=>'y'}}, :map=>{'c'=>'z'}) do |*a|
          a.join('-')
        end
        r.remaining_path
      end
    end

    body("/").must_equal '/'
    body("/a").must_equal 'x'
    body("/b").must_equal 'y'
    body("/c").must_equal '/c'
    body("/a/c").must_equal 'x-z'
    body("/b/c").must_equal 'y-z'
    body("/a/d").must_equal '/a/d'
  end
end
