require_relative 'test_helper'

class RootCauseError < RuntimeError; end
class MiddleCauseError < RuntimeError; end
class TopLevelError < RuntimeError; end

class ErrorCausesTest < Minitest::Test
  describe 'matching error handlers against exception causes' do
    it 'does not match causes when the setting is disabled' do
      mock_app do
        set :raise_errors, false
        error(RootCauseError) { 'cause handler' }
        error(500) { 'status handler' }
        get('/') do
          begin
            raise RootCauseError, 'root cause'
          rescue
            raise TopLevelError, 'wrapped'
          end
        end
      end

      get '/'
      assert_equal 'status handler', body
    end

    it 'invokes the handler registered for the cause when enabled' do
      mock_app do
        set :raise_errors, false
        enable :match_error_causes
        error(RootCauseError) { 'cause handler' }
        get('/') do
          begin
            raise RootCauseError, 'root cause'
          rescue
            raise TopLevelError, 'wrapped'
          end
        end
      end

      get '/'
      assert_equal 'cause handler', body
    end

    it 'prefers the handler for the raised exception over cause handlers' do
      mock_app do
        set :raise_errors, false
        enable :match_error_causes
        error(TopLevelError) { 'top handler' }
        error(RootCauseError) { 'cause handler' }
        get('/') do
          begin
            raise RootCauseError, 'root cause'
          rescue
            raise TopLevelError, 'wrapped'
          end
        end
      end

      get '/'
      assert_equal 'top handler', body
    end

    it 'prefers superclass handlers of the raised exception over deeper cause handlers' do
      mock_app do
        set :raise_errors, false
        enable :match_error_causes
        error(RuntimeError) { 'superclass handler' }
        error(RootCauseError) { 'cause handler' }
        get('/') do
          begin
            raise RootCauseError, 'root cause'
          rescue
            raise TopLevelError, 'wrapped'
          end
        end
      end

      get '/'
      assert_equal 'superclass handler', body
    end

    it 'walks the chain down to deeper causes' do
      mock_app do
        set :raise_errors, false
        enable :match_error_causes
        error(RootCauseError) { 'deepest cause handler' }
        get('/') do
          begin
            begin
              raise RootCauseError, 'root cause'
            rescue
              raise MiddleCauseError, 'middle'
            end
          rescue
            raise TopLevelError, 'wrapped'
          end
        end
      end

      get '/'
      assert_equal 'deepest cause handler', body
    end

    it 'passes the matched cause to the handler and keeps sinatra.error as the raised exception' do
      matched = nil
      raised = nil
      mock_app do
        set :raise_errors, false
        enable :match_error_causes
        error(RootCauseError) do |e|
          matched = e
          raised = env['sinatra.error']
          'handled'
        end
        get('/') do
          begin
            raise RootCauseError, 'root cause'
          rescue
            raise TopLevelError, 'wrapped'
          end
        end
      end

      get '/'
      assert_equal 'handled', body
      assert_instance_of RootCauseError, matched
      assert_instance_of TopLevelError, raised
    end

    it 'terminates when the cause chain contains a cycle' do
      mock_app do
        set :raise_errors, false
        enable :match_error_causes
        error(MiddleCauseError) { 'cyclic cause handler' }
        get('/') do
          first = TopLevelError.new('first')
          second = MiddleCauseError.new('second')
          first.define_singleton_method(:cause) { second }
          second.define_singleton_method(:cause) { first }
          raise first
        end
      end

      get '/'
      assert_equal 'cyclic cause handler', body
    end

    it 'does not recurse infinitely when the cause handler itself raises' do
      mock_app do
        set :raise_errors, false
        enable :match_error_causes
        error(RootCauseError) { raise TopLevelError, 'handler blew up' }
        get('/') do
          begin
            raise RootCauseError, 'root cause'
          rescue
            raise MiddleCauseError, 'wrapped'
          end
        end
      end

      assert_raises(TopLevelError) { get '/' }
    end

    it 'matches causes with handlers inherited from a base class' do
      base = Class.new(Sinatra::Base)
      base.set :raise_errors, false
      base.enable :match_error_causes
      base.error(RootCauseError) { 'base class' }

      mock_app(base) do
        get('/') do
          begin
            raise RootCauseError, 'root cause'
          rescue
            raise TopLevelError, 'wrapped'
          end
        end
      end

      get '/'
      assert_equal 'base class', body
    end
  end
end
