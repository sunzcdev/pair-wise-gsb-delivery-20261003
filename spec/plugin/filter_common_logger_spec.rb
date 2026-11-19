require_relative "../spec_helper"

describe "filter_common_logger plugin" do
  it 'allows skipping logging of certain requests' do
    logger = rack_input
    app(:bare) do
      plugin :common_logger, logger
      plugin :filter_common_logger do |result|
        return false if result[0] >= 300 && result[0] < 400
        return false if request.path_info.start_with?('/foo/')
        true
      end
      route do |r|
        r.on 'foo' do
          'aa'
        end

        r.on 'redir' do
          r.redirect '/'
        end

        r.on 'err' do
          raise 'foo'
        end

        'bbb'
      end
    end

    body("HTTP_VERSION"=>'HTTP/1.0').must_equal 'bbb'
    logger.rewind
    logger.read.must_match(/\A- - - \[\d\d\/[A-Z][a-z]{2}\/\d\d\d\d:\d\d:\d\d:\d\d [-+]\d\d\d\d\] "GET \/ HTTP\/1.0" 200 3 0.\d\d\d\d\n\z/)

    logger.rewind
    logger.truncate(0)
    body('/foo/bar').must_equal 'aa'
    logger.rewind
    logger.read.must_be_empty

    logger.rewind
    logger.truncate(0)
    body('/redir').must_equal ''
    logger.rewind
    logger.read.must_be_empty

    logger.rewind
    logger.truncate(0)
    proc do
      body('/err')
    end.must_raise(RuntimeError)
    logger.rewind
    logger.read.must_be_empty
  end
end
