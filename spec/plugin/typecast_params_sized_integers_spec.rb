require_relative "../spec_helper"
require 'tempfile'

describe "typecast_params_sized_integers plugin" do 
  def tp(arg)
    @tp.call(arg)
  end

  before(:all) do
    res = nil
    app(:typecast_params_sized_integers) do |r|
      res = typecast_params
      ''
    end

    @tp = lambda do |i|
      req('QUERY_STRING'=>"i=#{i}&a[]=#{i}", 'rack.input'=>rack_input)
      res
    end

    @tp_error = Roda::RodaPlugins::TypecastParams::Error
  end

  {
    8 => ['', '-129', '128'],
    16 => ['', '-32769', '32768'],
    32 => ['', '-2147483649', '2147483648'],
    64 => ['', '-9223372036854775809', '9223372036854775808'],
  }.each do |i, vals|
    [:"int#{i}", :"Integer#{i}"].each do |meth|
      vals.each do |v|
        it "##{meth} should return nil for #{v.inspect}" do
          tp(v).send(meth, 'i').must_be_nil
        end

        it "#array!(:#{meth}) should raise for [#{v.inspect}]" do
          proc{tp(v).array!(meth, 'a')}.must_raise @tp_error
        end
      end
    end

    [:"int#{i}!", :"Integer#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should raise for #{v.inspect}" do
          proc{tp(v).send(meth, 'i')}.must_raise @tp_error
        end
      end
    end
  end

  {
    8 => ['0', '-128', '127'],
    16 => ['0', '-32768', '32767'],
    32 => ['0', '-2147483648', '2147483647'],
    64 => ['0', '-9223372036854775808', '9223372036854775807'],
  }.each do |i, vals|
    [:"int#{i}", :"Integer#{i}", :"int#{i}!", :"Integer#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should return #{v} for #{v.inspect}" do
          tp(v).send(meth, 'i').must_equal(v.to_i)
        end

        it "#array(:#{meth}) should return [#{v}] for [#{v.inspect}]" do
          tp(v).array(meth, 'a').must_equal [v.to_i]
        end unless meth.to_s.end_with?('!')
      end
    end
  end

  {
    8 => ['', '0', '128'],
    16 => ['', '0', '32768'],
    32 => ['', '0', '2147483648'],
    64 => ['', '0', '9223372036854775808'],
  }.each do |i, vals|
    [:"pos_int#{i}"].each do |meth|
      vals.each do |v|
        it "##{meth} should return nil for #{v.inspect}" do
          tp(v).send(meth, 'i').must_be_nil
        end

        it "#array!(:#{meth}) should raise for [#{v.inspect}]" do
          proc{tp(v).array!(meth, 'a')}.must_raise @tp_error
        end
      end
    end

    [:"pos_int#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should raise for #{v.inspect}" do
          proc{tp(v).send(meth, 'i')}.must_raise @tp_error
        end
      end
    end
  end

  {
    8 => ['1', '127'],
    16 => ['1', '32767'],
    32 => ['1', '2147483647'],
    64 => ['1', '9223372036854775807'],
  }.each do |i, vals|
    [:"pos_int#{i}", :"pos_int#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should return #{v} for #{v.inspect}" do
          tp(v).send(meth, 'i').must_equal(v.to_i)
        end

        it "#array(:#{meth}) should return [#{v}] for [#{v.inspect}]" do
          tp(v).array(meth, 'a').must_equal [v.to_i]
        end unless meth.to_s.end_with?('!')
      end
    end
  end

  {
    8 => ['', '-1', '256'],
    16 => ['', '-1', '65536'],
    32 => ['', '-1', '4294967296'],
    64 => ['', '-1', '18446744073709551616'],
  }.each do |i, vals|
    [:"uint#{i}", :"Integeru#{i}"].each do |meth|
      vals.each do |v|
        it "##{meth} should return nil for #{v.inspect}" do
          tp(v).send(meth, 'i').must_be_nil
        end

        it "#array!(:#{meth}) should raise for [#{v.inspect}]" do
          proc{tp(v).array!(meth, 'a')}.must_raise @tp_error
        end
      end
    end

    [:"uint#{i}!", :"Integeru#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should raise for #{v.inspect}" do
          proc{tp(v).send(meth, 'i')}.must_raise @tp_error
        end
      end
    end
  end

  {
    8 => ['0', '255'],
    16 => ['0', '65535'],
    32 => ['0', '4294967295'],
    64 => ['0', '18446744073709551615'],
  }.each do |i, vals|
    [:"uint#{i}", :"Integeru#{i}", :"uint#{i}!", :"Integeru#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should return #{v} for #{v.inspect}" do
          tp(v).send(meth, 'i').must_equal(v.to_i)
        end

        it "#array(:#{meth}) should return [#{v}] for [#{v.inspect}]" do
          tp(v).array(meth, 'a').must_equal [v.to_i]
        end unless meth.to_s.end_with?('!')
      end
    end
  end

  {
    8 => ['', '0', '256'],
    16 => ['', '0', '65536'],
    32 => ['', '0', '4294967296'],
    64 => ['', '0', '18446744073709551616'],
  }.each do |i, vals|
    [:"pos_uint#{i}"].each do |meth|
      vals.each do |v|
        it "##{meth} should return nil for #{v.inspect}" do
          tp(v).send(meth, 'i').must_be_nil
        end

        it "#array!(:#{meth}) should raise for [#{v.inspect}]" do
          proc{tp(v).array!(meth, 'a')}.must_raise @tp_error
        end
      end
    end

    [:"pos_uint#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should raise for #{v.inspect}" do
          proc{tp(v).send(meth, 'i')}.must_raise @tp_error
        end
      end
    end
  end

  {
    8 => ['1', '255'],
    16 => ['1', '65535'],
    32 => ['1', '4294967295'],
    64 => ['1', '18446744073709551615'],
  }.each do |i, vals|
    [:"pos_uint#{i}", :"pos_uint#{i}!"].each do |meth|
      vals.each do |v|
        it "##{meth} should return #{v} for #{v.inspect}" do
          tp(v).send(meth, 'i').must_equal(v.to_i)
        end

        it "#array(:#{meth}) should return [#{v}] for [#{v.inspect}]" do
          tp(v).array(meth, 'a').must_equal [v.to_i]
        end unless meth.to_s.end_with?('!')
      end
    end
  end
end

describe "typecast_params_sized_integers plugin" do 
  it "should support a :sizes option for only creating methods for the given sizes" do
    app(:bare) do
      plugin :typecast_params_sized_integers, :sizes=>[8]
      route do |_|
        "#{typecast_params.respond_to?(:int8)}-#{typecast_params.respond_to?(:int16)}-#{typecast_params.respond_to?(:int32)}"
      end
    end
    body('rack.input'=>rack_input).must_equal 'true-false-false'
    app.plugin :typecast_params_sized_integers, :sizes=>[8, 16]
    body('rack.input'=>rack_input).must_equal 'true-true-false'
  end

  it "should support a :default_size option for making the unsuffixed methods use the given size" do
    app(:bare) do
      plugin :typecast_params_sized_integers, :default_size=>8
      route do |r|
        tp = typecast_params
        r.is String do |type|
          %w'ts min no z o max tl umax utl'.map do |param|
            tp.send(type, param).inspect
          end.join(" ")
        end
      end
    end
    h = {'QUERY_STRING'=>'ts=-129&min=-128&no=-1&z=0&o=1&max=127&umax=255&tl=128&umax=255&utl=256', 'rack.input'=>rack_input}
    body('/int', h).must_equal 'nil -128 -1 0 1 127 nil nil nil'
    body('/uint', h).must_equal 'nil nil nil 0 1 127 128 255 nil'
    body('/pos_int', h).must_equal 'nil nil nil nil 1 127 nil nil nil'
    body('/pos_uint', h).must_equal 'nil nil nil nil 1 127 128 255 nil'
    body('/Integer', h).must_equal 'nil -128 -1 0 1 127 nil nil nil'
    body('/Integeru', h).must_equal 'nil nil nil 0 1 127 128 255 nil'
  end
end
