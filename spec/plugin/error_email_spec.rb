require_relative "../spec_helper"

describe "error_email plugin" do 
  def app(opts={})
    @emails = emails = [] unless defined?(@emails)
    @app ||= super(:bare) do
      plugin :error_email, {:to=>'t', :from=>'f', :emailer=>lambda{|h| emails << h}}.merge(opts)

      route do |r|
        r.get('noerror'){error_email("Problem"); 'g'}
        raise ArgumentError, 'bad foo' rescue error_email($!)
        'e'
      end
    end
  end

  def email
    @emails.last
  end

  it "adds error_email method for emailing exceptions" do
    app
    body('rack.input'=>rack_input, 'QUERY_STRING'=>'b=c', 'rack.session'=>{'d'=>'e'}).must_equal 'e'
    email[:to].must_equal 't'
    email[:from].must_equal 'f'
    email[:host].must_equal 'localhost'
    email[:message].must_match(/^Subject: ArgumentError: bad foo/)
    email[:message].must_match(/^Backtrace:$.+^ENV:$.+^"rack\.input" => .+^Params:$\s+^"b" => "c"$\s+^Session:$\s+^"d" => "e"$/m)
  end

  it "have error_email method support string arguments" do
    app
    body('/noerror', 'rack.input'=>rack_input, 'QUERY_STRING'=>'b=c', 'rack.session'=>{'d'=>'e'}).must_equal 'g'
    email[:to].must_equal 't'
    email[:from].must_equal 'f'
    email[:host].must_equal 'localhost'
    email[:message].must_match(/^Subject: Problem/)
    email[:message].must_match(/^ENV:$.+^"rack\.input" => .+^Params:$\s+^"b" => "c"$\s+^Session:$\s+^"d" => "e"$/m)
    email[:message].wont_include('Backtrace')
  end

  it "supports error_email_content for the content of the email" do
    app.route do |r|
      raise ArgumentError, 'bad foo' rescue error_email_content($!)
    end
    b = body('rack.input'=>rack_input, 'QUERY_STRING'=>'b=c', 'rack.session'=>{'d'=>'e'})
    b.must_match(/^Subject: ArgumentError: bad foo/)
    b.must_match(/^Backtrace:$.+^ENV:$.+^"rack\.input" => .+^Params:$\s+^"b" => "c"$\s+^Session:$\s+^"d" => "e"$/m)
  end

  it "supports :filter plugin option for filtering parameters, environment variables, and session values" do
    app.route do |r|
      raise ArgumentError, 'bad foo' rescue error_email_content($!)
    end
    app.plugin :error_email, :filter=>proc{|k, v| k == 'b' || k == 'd' || k == 'rack.input'}
    b = body('rack.input'=>rack_input, 'QUERY_STRING'=>'b=c&f=g', 'rack.session'=>{'d'=>'e', 'h'=>'i'})
    b.must_match(/^Subject: ArgumentError: bad foo/)
    b.must_match(/^Backtrace:.+^ENV:.+^"rack\.input" => FILTERED.+^Params:\s+^"b" => FILTERED\s+"f" => "g"\s+^Session:\s+^"d" => FILTERED\s+"h" => "i"/m)
  end

  it "handles invalid parameters in error_email_content" do
    app.route do |r|
      raise ArgumentError, 'bad foo' rescue error_email_content($!)
    end
    b = body('rack.input'=>rack_input, 'QUERY_STRING'=>"b=%c", 'rack.session'=>{'d'=>'e'})
    b.must_match(/^Subject: ArgumentError: bad foo/)
    b.must_match(/^Backtrace:$.+^ENV:$.+^"rack\.input" => .+^Params:$\s+^Invalid Parameters!$\s+^Session:$\s+^"d" => "e"$/m)
  end

  it "uses :host option" do
    app(:host=>'foo.bar.com')
    body('rack.input'=>rack_input).must_equal 'e'
    email[:host].must_equal 'foo.bar.com'
  end

  it "handles error messages with new lines" do
    app.route do |r|
      raise "foo\nbar\nbaz" rescue error_email($!)
      'e'
    end
    body('rack.input'=>rack_input).must_equal 'e'
    email[:message].must_match %r{From: f\r\nSubject: RuntimeError: foo\r\n bar\r\n baz\r\nTo: t\r\n\r\n}
  end

  it "adds :prefix option to subject line" do
    app(:prefix=>'TEST ')
    body('rack.input'=>rack_input).must_equal 'e'
    email[:message].must_match(/^Subject: TEST ArgumentError/)
  end

  it "uses :headers option for additional headers" do
    app(:headers=>{'Foo'=>'Bar', 'Baz'=>'Quux'})
    body('rack.input'=>rack_input).must_equal 'e'
    email[:message].must_match(/^Foo: Bar/)
    email[:message].must_match(/^Baz: Quux/)
  end

  it "requires the :to and :from options" do
    proc{app :from=>nil}.must_raise(Roda::RodaError)
    proc{app :to=>nil}.must_raise(Roda::RodaError)
  end

  it "works correctly in subclasses" do
    @app = Class.new(app)
    @app.route do |r|
      raise ArgumentError rescue error_email($!)
      'e'
    end
    body('rack.input'=>rack_input).must_equal 'e'
    email[:to].must_equal 't'
    email[:from].must_equal 'f'
    email[:host].must_equal 'localhost'
    email[:message].must_match(/^Subject: ArgumentError/)
    email[:message].must_match(/Backtrace.*ENV/m)
  end
end
