# :markup: markdown
require 'monitor'

# An abstract superclass for all store classes.
class HTTP::CookieJar::AbstractStore
  include MonitorMixin

  class << self

    # Gets an implementation class by the name.
    def implementation(symbol)
      case symbol
      when :hash
        HTTP::CookieJar::HashStore
      when :mozilla
        HTTP::CookieJar::MozillaStore
      else
        raise IndexError, 'cookie store unavailable: %s' % symbol.inspect
      end
    end
  end

  # Defines options and their default values.
  def default_options
    # {}
  end
  private :default_options

  # :call-seq:
  #   new(**options)
  #
  # Called by the constructor of each subclass using super().
  def initialize(options = nil)
    super() # MonitorMixin
    options ||= {}
    @logger = options[:logger]
    # Initializes each instance variable of the same name as option
    # keyword.
    default_options.each_pair { |key, default|
      instance_variable_set("@#{key}", options.fetch(key, default))
    }
  end

  # This is an abstract method that each subclass must override.
  def initialize_copy(other)
    # self
  end

  # Implements HTTP::CookieJar#add().
  #
  # This is an abstract method that each subclass must override.
  def add(cookie)
    # self
  end

  # Implements HTTP::CookieJar#delete().
  #
  # This is an abstract method that each subclass must override.
  def delete(cookie)
    # self
  end

  # Iterates over all cookies that are not expired.
  #
  # An optional argument +uri+ specifies a URI object indicating the
  # destination of the cookies being selected.  Every cookie yielded
  # should be good to send to the given URI,
  # i.e. cookie.valid_for_uri?(uri) evaluates to true.
  #
  # An optional keyword argument +partition+ specifies the top-level
  # site of the calling context.  Partitioned cookies are only
  # yielded when their partition key matches it, while unpartitioned
  # cookies are always yielded.
  #
  # If (and only if) the +uri+ option is given, last access time of
  # each cookie is updated to the current time.
  #
  # This is an abstract method that each subclass must override.
  def each(uri = nil, partition: nil, &block) # :yield: cookie
    # if uri
    #   ...
    # else
    #   synchronize {
    #     ...
    #   }
    # end
    # self
  end
  include Enumerable

  # Implements HTTP::CookieJar#empty?().
  def empty?
    each { return false }
    true
  end

  # Implements HTTP::CookieJar#clear().
  #
  # This is an abstract method that each subclass must override.
  def clear
    # self
  end

  # Implements HTTP::CookieJar#cleanup().
  #
  # This is an abstract method that each subclass must override.
  def cleanup(session = false)
    # if session
    #   select { |cookie| cookie.session? || cookie.expired? }
    # else
    #   select(&:expired?)
    # end.each { |cookie|
    #   delete(cookie)
    # }
    # # subclasses can optionally remove over-the-limit cookies.
    # self
  end

  protected

  # Tests if a cookie with the given partition key is visible in the
  # calling context identified by +context_partition+.  Unpartitioned
  # cookies are visible in every context.
  def partition_match?(cookie_partition, context_partition)
    cookie_partition.nil? || cookie_partition == context_partition
  end
end

require 'http/cookie_jar/hash_store'

unless defined?(JRUBY_VERSION)
  require 'http/cookie_jar/mozilla_store'
end
