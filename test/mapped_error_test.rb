require_relative 'test_helper'

class FooError < RuntimeError
end

class FooNotFound < Sinatra::NotFound
end

class FooSpecialError < Sinatra::Error
  def http_status; 501 end
end

class FooStatusOutOfRangeError < Sinatra::Error
  def code; 4000 end
end

class FooWithCode < Sinatra::Error
  def code; 419 end
end

class FirstError < RuntimeError; end
class SecondError < RuntimeError; end

class InfraError < RuntimeError; end

class CyclicCauseError < RuntimeError
  attr_accessor :cause
end

class MappedErrorTest < Minitest::Test
  def test_default
    assert true
  end

  describe 'Exception Mappings' do
    it 'invokes handlers registered with ::error when raised' do
      mock_app do
        set :raise_errors, false
        error(FooError) { 'Foo!' }
        get('/') { raise FooError }
      end
      get '/'
      assert_equal 500, status
      assert_equal 'Foo!', body
    end

    it 'passes the exception object to the error handler' do
      mock_app do
        set :raise_errors, false
        error(FooError) { |e| assert_equal(FooError, e.class) }
        get('/') { raise FooError }
      end
      get('/')
    end

    it 'uses the Exception handler if no matching handler found' do
      mock_app do
        set :raise_errors, false
        error(Exception) { 'Exception!' }
        get('/') { raise FooError }
      end

      get '/'
      assert_equal 500, status
      assert_equal 'Exception!', body
    end

    it 'walks down inheritance chain for errors' do
      mock_app do
        set :raise_errors, false
        error(RuntimeError) { 'Exception!' }
        get('/') { raise FooError }
      end

      get '/'
      assert_equal 500, status
      assert_equal 'Exception!', body
    end

    it 'favors subclass handler over superclass handler if available' do
      mock_app do
        set :raise_errors, false
        error(Exception) { 'Exception!' }
        error(FooError) { 'FooError!' }
        error(RuntimeError) { 'Exception!' }
        get('/') { raise FooError }
      end

      get '/'
      assert_equal 500, status
      assert_equal 'FooError!', body
    end

    it "sets env['sinatra.error'] to the rescued exception" do
      mock_app do
        set :raise_errors, false
        error(FooError) do
          assert env.include?('sinatra.error')
          assert env['sinatra.error'].kind_of?(FooError)
          'looks good'
        end
        get('/') { raise FooError }
      end
      get '/'
      assert_equal 'looks good', body
    end

    it "raises errors from the app when raise_errors set and no handler defined" do
      mock_app do
        set :raise_errors, true
        get('/') { raise FooError }
      end
      assert_raises(FooError) { get '/' }
    end

    it "calls error handlers before raising errors even when raise_errors is set" do
      mock_app do
        set :raise_errors, true
        error(FooError) { "she's there." }
        get('/') { raise FooError }
      end
      get '/'
      assert_equal 500, status
    end

    it "never raises Sinatra::NotFound beyond the application" do
      mock_app(Sinatra::Application) do
        get('/') { raise Sinatra::NotFound }
      end
      get '/'
      assert_equal 404, status
    end

    it "cascades for subclasses of Sinatra::NotFound" do
      mock_app do
        set :raise_errors, true
        error(FooNotFound) { "foo! not found." }
        get('/') { raise FooNotFound }
      end
      get '/'
      assert_equal 404, status
      assert_equal 'foo! not found.', body
    end

    it 'has a not_found method for backwards compatibility' do
      mock_app { not_found { "Lost, are we?" } }

      get '/test'
      assert_equal 404, status
      assert_equal "Lost, are we?", body
    end

    it 'inherits error mappings from base class' do
      base = Class.new(Sinatra::Base)
      base.error(FooError) { 'base class' }

      mock_app(base) do
        set :raise_errors, false
        get('/') { raise FooError }
      end

      get '/'
      assert_equal 'base class', body
    end

    it 'overrides error mappings in base class' do
      base = Class.new(Sinatra::Base)
      base.error(FooError) { 'base class' }

      mock_app(base) do
        set :raise_errors, false
        error(FooError) { 'subclass' }
        get('/') { raise FooError }
      end

      get '/'
      assert_equal 'subclass', body
    end

    it 'honors Exception#http_status if present' do
      mock_app do
        set :raise_errors, false
        error(501) { 'Foo!' }
        get('/') { raise FooSpecialError }
      end
      get '/'
      assert_equal 501, status
      assert_equal 'Foo!', body
    end

    it 'does not use Exception#code by default' do
      mock_app do
        set :raise_errors, false
        get('/') { raise FooWithCode }
      end
      get '/'
      assert_equal 500, status
    end

    it 'uses Exception#code if use_code is enabled' do
      mock_app do
        set :raise_errors, false
        set :use_code, true
        get('/') { raise FooWithCode }
      end
      get '/'
      assert_equal 419, status
    end

    it 'does not rely on Exception#code for invalid codes' do
      mock_app do
        set :raise_errors, false
        set :use_code, true
        get('/') { raise FooStatusOutOfRangeError }
      end
      get '/'
      assert_equal 500, status
    end

    it "allows a stack of exception_handlers" do
      mock_app do
        set :raise_errors, false
        error(FirstError) { 'First!' }
        error(SecondError) { 'Second!' }
        get('/'){ raise SecondError }
      end
      get '/'
      assert_equal 500, status
      assert_equal 'Second!', body
    end

    it "allows an exception handler to pass control to the next exception handler" do
      mock_app do
        set :raise_errors, false
        error(500, FirstError) { 'First!' }
        error(500, SecondError) { pass }
        get('/') { raise 500 }
      end
      get '/'
      assert_equal 500, status
      assert_equal 'First!', body
    end

    it "allows an exception handler to handle the exception" do
      mock_app do
        set :raise_errors, false
        error(500, FirstError) { 'First!' }
        error(500, SecondError) { 'Second!' }
        get('/') { raise 500 }
      end
      get '/'
      assert_equal 500, status
      assert_equal 'Second!', body
    end
  end

  describe 'Exception Cause Mappings' do
    it 'does not match handlers against causes by default' do
      mock_app do
        set :raise_errors, false
        error(FooError) { 'cause handler' }
        error(Exception) { 'generic handler' }
        get('/') do
          begin
            raise FooError
          rescue FooError
            raise InfraError
          end
        end
      end

      get '/'
      assert_equal 500, status
      assert_equal 'generic handler', body
    end

    it 'matches handlers against causes when error_causes is enabled' do
      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(FooError) { 'cause handler' }
        error(Exception) { 'generic handler' }
        get('/') do
          begin
            raise FooError
          rescue FooError
            raise InfraError
          end
        end
      end

      get '/'
      assert_equal 500, status
      assert_equal 'cause handler', body
    end

    it 'prefers handlers for the raised exception over handlers for its cause' do
      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(InfraError) { 'top handler' }
        error(FooError) { 'cause handler' }
        get('/') do
          begin
            raise FooError
          rescue FooError
            raise InfraError
          end
        end
      end

      get '/'
      assert_equal 'top handler', body
    end

    it 'prefers superclass handlers of the raised exception over handlers for deeper causes' do
      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(RuntimeError) { 'superclass handler' }
        error(FooError) { 'cause handler' }
        get('/') do
          begin
            raise FooError
          rescue FooError
            raise InfraError
          end
        end
      end

      get '/'
      assert_equal 'superclass handler', body
    end

    it 'walks deeper causes until a handler matches' do
      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(FooError) { 'deep cause handler' }
        get('/') do
          begin
            begin
              raise FooError
            rescue FooError
              raise SecondError
            end
          rescue SecondError
            raise InfraError
          end
        end
      end

      get '/'
      assert_equal 'deep cause handler', body
    end

    it 'exposes both the raised exception and the matched cause to the handler' do
      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(FooError) do |matched|
          "matched: #{matched.message} / top: #{env['sinatra.error'].message}"
        end
        get('/') do
          begin
            raise FooError, 'business failure'
          rescue FooError
            raise InfraError, 'infrastructure wrapper'
          end
        end
      end

      get '/'
      assert_equal 'matched: business failure / top: infrastructure wrapper', body
    end

    it 'falls back to status code handlers when no cause matches' do
      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(FooError) { 'cause handler' }
        error(500) { 'status handler' }
        get('/') { raise InfraError }
      end

      get '/'
      assert_equal 500, status
      assert_equal 'status handler', body
    end

    it 'stops walking cyclic cause chains' do
      first = CyclicCauseError.new('first')
      second = CyclicCauseError.new('second')
      first.cause = second
      second.cause = first

      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(FooError) { 'cause handler' }
        get('/') { raise first }
      end

      get '/'
      assert_equal 500, status
      refute_equal 'cause handler', body
    end

    it 'does not recurse infinitely when a cause handler raises' do
      mock_app do
        set :raise_errors, false
        set :error_causes, true
        error(FooError) { raise FooError, 'from handler' }
        get('/') do
          begin
            raise FooError, 'business failure'
          rescue FooError
            raise InfraError
          end
        end
      end

      assert_raises(FooError) { get '/' }
    end

    it 'inherits cause handlers from the base class' do
      base = Class.new(Sinatra::Base)
      base.error(FooError) { 'base cause handler' }

      mock_app(base) do
        set :raise_errors, false
        set :error_causes, true
        get('/') do
          begin
            raise FooError
          rescue FooError
            raise InfraError
          end
        end
      end

      get '/'
      assert_equal 'base cause handler', body
    end
  end

  describe 'Custom Error Pages' do
    it 'allows numeric status code mappings to be registered with ::error' do
      mock_app do
        set :raise_errors, false
        error(500) { 'Foo!' }
        get('/') { [500, {}, 'Internal Foo Error'] }
      end
      get '/'
      assert_equal 500, status
      assert_equal 'Foo!', body
    end

    it 'allows ranges of status code mappings to be registered with :error' do
      mock_app do
        set :raise_errors, false
        error(500..550) { "Error: #{response.status}" }
        get('/') { [507, {}, 'A very special error'] }
      end
      get '/'
      assert_equal 507, status
      assert_equal 'Error: 507', body
    end

    it 'allows passing more than one range' do
      mock_app do
        set :raise_errors, false
        error(409..411, 503..509) { "Error: #{response.status}" }
        get('/') { [507, {}, 'A very special error'] }
      end
      get '/'
      assert_equal 507, status
      assert_equal 'Error: 507', body
    end
  end
end
