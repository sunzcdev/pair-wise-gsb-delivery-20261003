require_relative "spec_helper"

describe "Roda::RodaCache" do
  before do
    @cache = Roda::RodaCache.new
  end

  it "should provide a hash like interface" do
    @cache[1].must_be_nil
    @cache[1] = 2
    @cache[1].must_equal 2
  end

  it "should have dup return a copy of the cache" do
    @cache[1].must_be_nil
    @cache[1] = 2

    cache = @cache.dup
    @cache[2] = 3
    cache[3] = 4

    cache[1].must_equal 2
    cache[2].must_be_nil
    cache[3].must_equal 4

    @cache[1].must_equal 2
    @cache[2].must_equal 3
    @cache[3].must_be_nil
  end

  it "should have freeze return a frozen hash" do
    v = @cache.freeze
    v.must_equal({})
    v.must_be_instance_of(Hash)
  end
end

