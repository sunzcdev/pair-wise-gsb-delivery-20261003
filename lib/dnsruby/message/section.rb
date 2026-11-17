module Dnsruby
class Section < Array

  def initialize(msg = nil)
    @msg = msg
    super(0)
  end

  #  Return the rrset of the specified type in this section
  def rrset(name, type=Types.A, klass=Classes::IN)
    rrs = select{|rr|
      type_ok = (rr.type==type)
      if rr.type == Types::RRSIG
        type_ok = (rr.type_covered == type)
      end
      unless /\.\z/ =~ name.to_s
        name = name.to_s + '.'
      end
      type_ok && (rr.klass == klass) && (rr.name.to_s(true).downcase == name.to_s().downcase)
    }
    RRSet.new(rrs)
  end

  #  Return an array of all the rrsets in the section
  def rrsets(type = nil, include_opt = false)
    if type && !(Types === type)
      type = Types.new(type)
    end
    groups = {}
    budget = ValidationBudget.current if defined?(ValidationBudget)
    each do |rr|
      budget&.check!(1)
      next if !include_opt && rr.type == Types::OPT
      covered_type = rr.type == Types::RRSIG ? rr.type_covered : rr.type
      next if type && rr.type != type && covered_type != type

      # Hash insertion order preserves the section's first occurrence of each
      # set. A signature joins the set it covers even when it appears first.
      key = [rr.name.canonical, rr.klass.code, covered_type.code]
      (groups[key] ||= []) << rr
    end
    groups.values.map { |records| RRSet.new(records) }
  end

  def ==(other)
    return false unless self.class == other.class
    return false if other.rrsets(nil, true).length != self.rrsets(nil, true).length

    otherrrsets = other.rrsets(nil)
    self.rrsets(nil).each {|rrset|
      return false unless otherrrsets.include?(rrset)
    }

    true
  end

  def remove_rrset(name, type)
    #  Remove all RRs with the name and type from the section.
    #  Need to worry about header counts here - can we get Message to
    #  update the counts itself, rather than the section worrying about it?
    rrs_to_delete = []
    each do |rr|
      next if rr.rr_type == Types::OPT
      if (rr.name.to_s.downcase == name.to_s.downcase) &&
          ((rr.type == type) ||
              ((rr.type == Types::RRSIG) && (rr.type_covered == type)))
        rrs_to_delete.push(rr)
      end
    end
    rrs_to_delete.each { |rr| delete(rr) }
    @msg.update_counts if @msg
  end
end
end
