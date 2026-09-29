# :markup: markdown
# frozen_string_literal: true
require 'http/cookie'
require 'time'

# This class provides a fidelity-preserving representation of a
# Set-Cookie header value.
#
# Unlike HTTP::Cookie, which normalizes a cookie definition into a
# semantic model, this class keeps the original attribute order,
# spelling and whitespace, and retains attributes it does not know
# about.  This makes it suitable for a proxy or an intermediary that
# needs to adjust a known attribute (typically Expires or Max-Age)
# without dropping or reformatting anything else.
#
# ### Duplicate attribute rule
#
# Following RFC 6265 5.3, the first occurrence of an attribute is
# authoritative:
#
# * Reading a known attribute returns the value of its first
#   occurrence.
# * Writing a known attribute rewrites the first occurrence in
#   place, preserving the original attribute name spelling and
#   leading whitespace, and removes any later occurrences of the
#   same attribute.
# * Writing an attribute that is absent appends a new segment in
#   canonical spelling at the end.
# * Assigning nil to a valued attribute, or false to a flag
#   attribute, removes every occurrence of that attribute.
#
# Segments that are not affected by an edit are serialized back
# byte-for-byte as they were parsed.
#
# CR and LF characters are rejected both at parse time and in any
# assigned value, so a serialized string never contains them.
class HTTP::Cookie::SetCookie
  # Canonical spellings used when appending a previously absent
  # attribute.
  CANONICAL_NAMES = {
    'expires'  => 'Expires',
    'max-age'  => 'Max-Age',
    'domain'   => 'Domain',
    'path'     => 'Path',
    'secure'   => 'Secure',
    'httponly' => 'HttpOnly',
  }.freeze

  # A single `;`-separated segment of a Set-Cookie string, keeping
  # the raw text so it can be serialized back unchanged.
  class Segment
    # The leading whitespace of the segment.
    attr_reader :leading
    # The name token as it appeared, with original case preserved.
    attr_reader :token
    # The value with surrounding whitespace stripped, or nil for a
    # flag segment.
    attr_reader :value

    def initialize(raw)
      @raw = raw
      match = /\A([ \t]*)([^=]*?)([ \t]*)(?:=(.*))?\z/m.match(raw)
      @leading = match[1]
      @token = match[2]
      @value = match[4] && match[4].strip
    end

    def flag?
      @value.nil?
    end

    def raw
      @raw
    end

    # Rewrites the value part, preserving the leading whitespace and
    # the original name token.
    def rewrite_value(new_value)
      @raw = +"#{@leading}#{@token}=#{new_value}"
      @value = new_value
    end
  end

  class << self
    # Splits a Set-Cookie string on semicolons that appear outside
    # of double-quoted values.
    def split_segments(string)
      segments = []
      current = +''
      in_quote = false
      index = 0
      while index < string.length
        char = string[index]
        case char
        when '\\'
          if in_quote && index + 1 < string.length
            current << char << string[index + 1]
            index += 2
            next
          end
          current << char
        when '"'
          in_quote = !in_quote
          current << char
        when ';'
          if in_quote
            current << char
          else
            segments << current
            current = +''
          end
        else
          current << char
        end
        index += 1
      end
      segments << current
      segments
    end

    # Removes surrounding double quotes and backslash escapes from a
    # value, if present.
    def unquote(value)
      if value.length >= 2 && value.start_with?('"') && value.end_with?('"')
        value[1..-2].gsub(/\\(.)/m) { $1 }
      else
        value
      end
    end
  end

  # The cookie name.
  attr_reader :name
  # The cookie value, unquoted if it was double-quoted.  nil if the
  # name-value pair has no value part.
  attr_reader :value

  # Parses a Set-Cookie header value.  ArgumentError is raised if
  # the string contains CR or LF, or has no cookie name.
  def initialize(string)
    string = (String.try_convert(string) or
      raise TypeError, "#{string.class} is not a String")
    if string.match(/[\r\n]/)
      raise ArgumentError, "Set-Cookie string must not contain CR or LF"
    end
    @segments = self.class.split_segments(string).map { |raw| Segment.new(raw) }
    head = @segments.first
    @name = head.token
    if @name.empty?
      raise ArgumentError, "Set-Cookie string has no cookie name"
    end
    @value = head.flag? ? nil : self.class.unquote(head.value)
  end

  # Returns the value of the first occurrence of the named
  # attribute (case-insensitive), unquoted if double-quoted.
  # Returns true for a flag attribute, and nil if the attribute is
  # absent.  Unknown attributes are accessible through this method.
  def [](name)
    segment = attribute_segment(name) or return nil
    segment.flag? ? true : self.class.unquote(segment.value)
  end

  # Returns the downcased attribute names in their original order,
  # including unknown ones.
  def attribute_names
    @segments[1..-1].map { |segment| segment.token.downcase }.reject(&:empty?)
  end

  # The Expires attribute value as a Time object, or nil.
  def expires
    if (raw = self['expires']).is_a?(String)
      HTTP::Cookie::Scanner.new(raw).parse_cookie_date(raw) ||
        (Time.parse(raw) rescue nil)
    end
  end

  # Sets the Expires attribute, rewriting the first occurrence and
  # dropping any duplicates.  Accepts a Time / DateTime object or a
  # string that Time.parse understands.  Assigning nil removes the
  # attribute.
  def expires=(time)
    time =
      case time
      when nil, Time
        time
      when DateTime
        time.to_time
      else
        Time.parse(check_value(time, 'Expires'))
      end
    rewrite_attribute('expires', time && time.httpdate)
  end

  # The Max-Age attribute value as an integer, or nil.
  def max_age
    if (raw = self['max-age']).is_a?(String) && /\A-?\d+\z/.match?(raw)
      raw.to_i
    end
  end

  # Sets the Max-Age attribute.  Assigning nil removes the
  # attribute.
  def max_age=(sec)
    case sec
    when Integer, nil
    else
      str = (String.try_convert(sec) or
        raise TypeError, "#{sec.class} is not an Integer or String")
      /\A-?\d+\z/.match?(str) or
        raise ArgumentError, "invalid Max-Age: #{sec.inspect}"
      sec = str.to_i
    end
    rewrite_attribute('max-age', sec && sec.to_s)
  end

  # The Path attribute value, or nil.
  def path
    raw = self['path']
    raw.is_a?(String) ? raw : nil
  end

  # Sets the Path attribute.  Assigning nil removes the attribute.
  def path=(path)
    rewrite_attribute('path', path && check_value(path, 'Path'))
  end

  # The Domain attribute value, or nil.
  def domain
    raw = self['domain']
    raw.is_a?(String) ? raw : nil
  end

  # Sets the Domain attribute.  Assigning nil removes the
  # attribute.
  def domain=(domain)
    rewrite_attribute('domain', domain && check_value(domain, 'Domain'))
  end

  # Tests if the Secure attribute is present.
  def secure?
    !self['secure'].nil?
  end

  # Sets or removes the Secure flag attribute.
  def secure=(flag)
    rewrite_flag('secure', flag)
  end

  # Tests if the HttpOnly attribute is present.
  def httponly?
    !self['httponly'].nil?
  end

  # Sets or removes the HttpOnly flag attribute.
  def httponly=(flag)
    rewrite_flag('httponly', flag)
  end

  # Returns an HTTP::Cookie semantic view of this Set-Cookie string.
  # If `origin` is given, the cookie's origin is set accordingly.
  def to_cookie(origin = nil)
    origin = HTTP::Cookie::URIParser.parse(origin) if origin
    result = nil
    HTTP::Cookie::Scanner.new(to_s).scan_set_cookie do |name, value, attrs|
      break if name.nil? || name.empty?

      begin
        cookie = HTTP::Cookie.new(name, value)
      rescue
        next
      end
      attrs.each { |aname, avalue|
        begin
          case aname
          when 'domain'
            cookie.for_domain = true
            cookie.domain = avalue
          when 'path'
            cookie.path = avalue
          when 'expires'
            cookie.expires = avalue unless cookie.max_age
          when 'max-age'
            cookie.max_age = avalue
          when 'secure'
            cookie.secure = avalue
          when 'httponly'
            cookie.httponly = avalue
          end
        rescue
          # Ignore an unparsable attribute, mirroring HTTP::Cookie.parse.
        end
      }
      cookie.origin = origin if origin
      result = cookie
      break
    end
    result
  end

  # Serializes back to a Set-Cookie header value.  Segments that
  # were not edited are emitted byte-for-byte as parsed.  The result
  # never contains CR or LF.
  def to_s
    @segments.map(&:raw).join(';')
  end
  alias to_str to_s

  def inspect
    '#<%s: %s>' % [self.class, to_s]
  end

  private

  def attribute_segment(name)
    name = name.downcase
    @segments[1..-1].find { |segment| segment.token.downcase == name }
  end

  def attribute_indexes(name)
    indexes = []
    @segments.each_with_index { |segment, index|
      next if index.zero?
      indexes << index if segment.token.downcase == name
    }
    indexes
  end

  def check_value(value, label)
    value = (String.try_convert(value) or
      raise TypeError, "#{label} value must be a String")
    if value.match(/[\r\n]/)
      raise ArgumentError, "#{label} value must not contain CR or LF"
    end
    value
  end

  # Implements the duplicate attribute rule for valued attributes:
  # rewrite the first occurrence in place, drop later occurrences,
  # append in canonical spelling if absent, remove all if nil.
  def rewrite_attribute(name, new_value)
    indexes = attribute_indexes(name)
    if new_value.nil?
      indexes.reverse_each { |index| @segments.delete_at(index) }
    elsif indexes.empty?
      @segments << Segment.new(" #{CANONICAL_NAMES[name] || name}=#{new_value}")
    else
      @segments[indexes.first].rewrite_value(new_value)
      indexes[1..-1].reverse_each { |index| @segments.delete_at(index) }
    end
    self
  end

  def rewrite_flag(name, flag)
    indexes = attribute_indexes(name)
    if flag
      if indexes.empty?
        @segments << Segment.new(" #{CANONICAL_NAMES[name] || name}")
      else
        indexes[1..-1].reverse_each { |index| @segments.delete_at(index) }
      end
    else
      indexes.reverse_each { |index| @segments.delete_at(index) }
    end
    self
  end
end
