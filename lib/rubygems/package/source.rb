# frozen_string_literal: true

##
# A source of .gem bytes for Gem::Package.
#
# Sources are read in two stages:
#
# * +with_metadata_io+ - the metadata stage.  It yields an IO positioned at
#   the start of the package that is only read far enough to obtain the
#   specification (+metadata+ or +metadata.gz+).  Callers use it to inspect
#   a package before deciding whether to unpack it.
# * +with_read_io+ - the data stage.  It yields an IO positioned at the start
#   of the package for verification and for reading +data.tar.gz+.
#
# Both stages must see the same package bytes.  A source backed by a
# forward-only IO may therefore buffer the input once (see
# Gem::Package::IOSource) and replay it, but it must never ask the underlying
# input for a second copy.

class Gem::Package::Source # :nodoc:
  ##
  # Metadata stage.  By default this is the same full read as
  # +with_read_io+; a seekable source needs no separate bookkeeping.

  def with_metadata_io(&block)
    with_read_io(&block)
  end
end
