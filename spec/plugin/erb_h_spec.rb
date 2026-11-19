require_relative "../spec_helper"

begin
  require 'erb/escape'
rescue LoadError
  #warn "erb/escape not installed, skipping erb_h plugin test"  
else
describe "erb_h plugin" do 
  it "adds h method for html escaping" do
    app(:erb_h) do |r|
      h("<form>") + h(:form) + h("test&<>/'")
    end
  end

  it "does not allocate object if escaping not needed" do
    app(:erb_h) do |r|
      s1 = 'a'
      s2 = h(s1)
      "#{s1}-#{s2}-#{s1.equal?(s2)}"
    end

    body.must_equal 'a-a-true'
  end

  it "works even if loading h plugin after" do
    app(:bare) do
      plugin :erb_h
      plugin :h

      route do |r|
        r.get 'a' do
          s1 = 'a'
          s2 = h(s1)
          "#{s1}-#{s2}-#{s1.equal?(s2)}"
        end

        h("<form>") + h(:form) + h("test&<>/'")
      end
    end

    body.must_equal '&lt;form&gt;formtest&amp;&lt;&gt;/&#39;'
    body('/a').must_equal 'a-a-true'
  end
end
end
