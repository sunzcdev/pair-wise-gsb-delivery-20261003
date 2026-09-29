# frozen_string_literal: true
require 'http/cookie'
require 'time'

# This class provides a fidelity-preserving representation of a
# Set-Cookie header value, designed for intermediaries (such as proxy
# services) that need to adjust a known attribute like Expires or Path
# without normalizing, reordering or dropping the rest of the header.
#
# Whereas HTTP::Cookie.parse reduces a Set-Cookie value to cookie
# semantics (discarding unknown attributes, original spelling and
# order), HTTP::Cookie::SetCookie keeps the raw segments exactly as
# received:
#
# * The original attribute order, name spelling, spacing and quoting
#   are preserved byte for byte.  Serializing an unmodified object
#   with #to_s reproduces the input exactly.
#
# * Unknown attributes (e.g. +SameSite+, +Priority+ or any
#   vendor-specific extension) are kept untouched and remain
#   available via #unknown_attributes.
#
# * Editing a known attribute (Expires, Max-Age, Domain, Path,
#   Secure, HttpOnly) rewrites only the segment(s) of that attribute;
#   every other segment is emitted unchanged.
#
# === Duplicate attribute rule
#
# When the same known attribute appears multiple times, reads follow
# HTTP::Cookie::Scanner semantics: the last occurrence with a valid
# value wins.  Writes replace the first occurrence in place (keeping
# its position and leading whitespace) with a canonically spelled
# segment and remove all later duplicates, so the serialized form
# carries exactly one segment for the attribute.  If the attribute is
# absent, a new segment is appended at the end.
#
# === Safety
#
# A header value containing CR or LF is rejected at parse time, and
# attribute values assigned through the writers may not contain
# control characters or a semicolon, so serialization can never
# introduce CR/LF (header injection) into the output.
#
# Note that this class represents a single cookie definition (one
# name-value pair followed by attributes); it does not split
# comma-concatenated header values.
class HTTP::Cookie::SetCookie
  # Known attributes, mapping the downcased name to the canonical
  # spelling used when (re)writing a segment.
  KNOWN_ATTRIBUTES = {
    'expires'  => 'Expires',
    'max-age'  => 'Max-Age',
    'domain'   => 'Domain',
    'path'     => 'Path',
    'secure'   => 'Secure',
    'httponly' => 'HttpOnly',
  }.freeze

  # Horizontal whitespace that may surround tokens.
  RE_WSP = /[ \t]/

  # Characters that must never appear in a header value or in an
  # attribute value written by this class.  CR and LF would allow
  # header injection; a semicolon would corrupt the segment
  # structure.
  RE_FORBIDDEN_VALUE_CHAR = /[\x00-\x1F\x7F;]/

  # The cookie name as found in the first segment.
  attr_reader :name

  # The raw cookie value as found in the first segment, including any
  # surrounding double quotes.  Use #to_cookie for the unquoted,
  # semantically interpreted value.
  attr_reader :value

  class << self
    # Parses a Set-Cookie header value into a fidelity-preserving
    # SetCookie object.  Raises ArgumentError if the string contains
    # CR or LF.
    def parse(set_cookie)
      new(set_cookie)
    end

    # Splits a Set-Cookie value into segments on semicolons, honoring
    # double-quoted strings (with backslash escapes) so that a
    # semicolon inside a quoted value does not split.
    def split_segments(string)
      segments = []
      start = i = 0
      len = string.length
      while i < len
        case string[i]
        when '"'
          i += 1
          until i >= len
            case string[i]
            when '\\'
              i += 2
            when '"'
              i += 1
              break
            else
              i += 1
            end
          end
        when ';'
          segments << string[start...i]
          start = i += 1
        else
          i += 1
        end
      end
      segments << (string[start..-1] || +'')
      segments
    end

    # Returns the attribute name of a raw segment in its original
    # spelling, stripped of surrounding whitespace.
    def segment_name(segment)
      segment.sub(/\A#{RE_WSP}+/, '')[/\A[^=]*/].sub(/#{RE_WSP}+\z/, '')
    end

    # Returns the attribute value of a raw segment, or nil if the
    # segment has no "=".
    def segment_value(segment)
      if eq = segment.index(?=)
        segment[(eq + 1)..-1].sub(/\A#{RE_WSP}+/, '').sub(/#{RE_WSP}+\z/, '')
      end
    end
  end

  def initialize(set_cookie)
    set_cookie = (String.try_convert(set_cookie) or
      raise TypeError, "#{set_cookie.class} is not a String")
    if set_cookie.match(/[\r\n]/)
      raise ArgumentError, 'Set-Cookie value must not contain CR or LF'
    end
    @segments = self.class.split_segments(set_cookie)

    first = @segments.first
    if eq = first.index(?=)
      @name = first[0...eq].sub(/\A#{RE_WSP}+/, '').sub(/#{RE_WSP}+\z/, '')
      @value = first[(eq + 1)..-1]
    else
      @name = first.sub(/\A#{RE_WSP}+/, '').sub(/#{RE_WSP}+\z/, '')
      @value = nil
    end
  end

  # Returns an array of [name, value] pairs for attributes this
  # library does not know, in their original order and spelling.
  # Flag attributes (without "=") have a nil value.
  def unknown_attributes
    [].tap { |pairs|
      each_attribute_segment { |segment|
        name = self.class.segment_name(segment)
        next if KNOWN_ATTRIBUTES.key?(name.downcase)
        pairs << [name, self.class.segment_value(segment)]
      }
    }
  end

  # The Expires attribute value as a Time object, or nil.  When the
  # attribute occurs multiple times, the last occurrence with a
  # parseable date wins.
  def expires
    time = nil
    each_attribute_value('expires') { |value|
      value && (parsed = HTTP::Cookie::Scanner.new(value).parse_cookie_date(value)) and
        time = parsed
    }
    time
  end

  # The Max-Age attribute value as an Integer, or nil.  The last
  # occurrence with a valid integer value wins.
  def max_age
    sec = nil
    each_attribute_value('max-age') { |value|
      sec = value.to_i if value && /\A-?\d+\z/.match?(value)
    }
    sec
  end

  # The Domain attribute value, or nil.  The last occurrence with a
  # non-empty value wins.
  def domain
    domain = nil
    each_attribute_value('domain') { |value|
      domain = value unless value.nil? || value.empty?
    }
    domain
  end

  # The Path attribute value, or nil.  The last occurrence with an
  # absolute path value wins.
  def path
    path = nil
    each_attribute_value('path') { |value|
      path = value if value && value.start_with?('/')
    }
    path
  end

  # Whether the Secure attribute is present.
  def secure?
    attribute_present?('secure')
  end

  # Whether the HttpOnly attribute is present.
  def httponly?
    attribute_present?('httponly')
  end

  # Sets the Expires attribute.  Accepts a Time, a DateTime, a string
  # that Time.parse understands, or nil to remove the attribute.
  def expires=(time)
    case time
    when nil
      rewrite_attribute('expires', nil)
    else
      time = Time.parse(time) if time.is_a?(String)
      time = time.to_time if time.is_a?(DateTime)
      rewrite_attribute('expires', "Expires=#{time.getutc.httpdate}")
    end
  end

  # Sets the Max-Age attribute to an Integer number of seconds, or
  # removes the attribute if nil is given.
  def max_age=(sec)
    case sec
    when nil
      rewrite_attribute('max-age', nil)
    else
      str = sec.to_s
      unless /\A-?\d+\z/.match?(str)
        raise ArgumentError, "invalid Max-Age value: #{sec.inspect}"
      end
      rewrite_attribute('max-age', "Max-Age=#{str}")
    end
  end

  # Sets the Domain attribute, or removes it if nil is given.
  def domain=(domain)
    case domain
    when nil
      rewrite_attribute('domain', nil)
    else
      domain = validate_attribute_value(domain)
      rewrite_attribute('domain', "Domain=#{domain}")
    end
  end

  # Sets the Path attribute, or removes it if nil is given.
  def path=(path)
    case path
    when nil
      rewrite_attribute('path', nil)
    else
      path = validate_attribute_value(path)
      rewrite_attribute('path', "Path=#{path}")
    end
  end

  # Adds or removes the Secure attribute.
  def secure=(flag)
    rewrite_attribute('secure', flag ? 'Secure' : nil)
  end

  # Adds or removes the HttpOnly attribute.
  def httponly=(flag)
    rewrite_attribute('httponly', flag ? 'HttpOnly' : nil)
  end

  # Returns the semantic view of this cookie definition as an
  # HTTP::Cookie object, parsed with the existing HTTP::Cookie.parse
  # machinery from the given origin URI/URL.  Unknown attributes and
  # original spelling are not reflected in the result; use this when
  # cookie semantics (expiration, domain/path matching, flags) are
  # needed.
  def to_cookie(origin)
    HTTP::Cookie.parse(to_s, origin).first
  end

  # Serializes back to a Set-Cookie header value.  Segments that were
  # not rewritten are emitted byte for byte as parsed.  The result
  # never contains CR or LF.
  def to_s
    @segments.join(';')
  end

  private

  def each_attribute_segment
    @segments.each_with_index { |segment, i|
      yield segment if i > 0
    }
  end

  def each_attribute_value(key)
    each_attribute_segment { |segment|
      yield self.class.segment_value(segment) if
        self.class.segment_name(segment).downcase == key
    }
  end

  def attribute_present?(key)
    each_attribute_segment { |segment|
      return true if self.class.segment_name(segment).downcase == key
    }
    false
  end

  def validate_attribute_value(value)
    value = (String.try_convert(value) or
      raise TypeError, "#{value.class} is not a String")
    if value.match(RE_FORBIDDEN_VALUE_CHAR)
      raise ArgumentError, 'attribute value contains a forbidden character'
    end
    value
  end

  # Rewrites the segments of the known attribute +key+ following the
  # duplicate attribute rule described in the class documentation:
  # the first occurrence is replaced in place (preserving its
  # position and leading whitespace) and later duplicates are
  # removed.  If +canonical+ is nil, all occurrences are removed.  If
  # the attribute is absent and +canonical+ is non-nil, a new segment
  # is appended.
  def rewrite_attribute(key, canonical)
    indices = []
    @segments.each_with_index { |segment, i|
      indices << i if i > 0 && self.class.segment_name(segment).downcase == key
    }
    if canonical.nil?
      indices.reverse_each { |i| @segments.delete_at(i) }
    elsif indices.empty?
      @segments << (' ' + canonical)
    else
      first, *rest = indices
      lead = @segments[first][/\A#{RE_WSP}*/]
      @segments[first] = lead + canonical
      rest.reverse_each { |i| @segments.delete_at(i) }
    end
    self
  end
end
