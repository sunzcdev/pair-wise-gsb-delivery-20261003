#=================================================================
class ArchiveLoadFailed(Exception):
    def __init__(self, reason):
        self.msg = str(reason)
        super(ArchiveLoadFailed, self).__init__(self.msg)


#=================================================================
class RevisitResolveError(Exception):
    """ Base class for errors raised while resolving a WARC ``revisit``
    record into readable content.

    The offending record and the revisit chain resolved so far (outermost
    revisit first) are attached for diagnostics; either may be ``None``.
    """

    def __init__(self, reason, record=None, chain=None):
        self.msg = str(reason)
        self.record = record
        self.chain = tuple(chain) if chain else ()
        super(RevisitResolveError, self).__init__(self.msg)


#=================================================================
class RevisitReferenceError(RevisitResolveError):
    """ A revisit could not be resolved because the record it refers to was
    not found, or because the revisit carries no reference at all.
    """

    def __init__(self, reason, record=None, chain=None, reference=None):
        super(RevisitReferenceError, self).__init__(reason,
                                                    record=record,
                                                    chain=chain)
        self.reference = reference


#=================================================================
class RevisitCycleError(RevisitResolveError):
    """ The revisit chain loops back to a record that is already in the
    chain.
    """


#=================================================================
class RevisitChainLengthError(RevisitResolveError):
    """ The revisit chain is longer than the configured limit. """


#=================================================================
class RevisitProfileError(RevisitResolveError):
    """ The revisit uses a ``WARC-Profile`` this resolver does not know how
    to interpret.
    """


#=================================================================
class RevisitTargetTypeError(RevisitResolveError):
    """ The referenced record exists, but its ``WARC-Type`` can not supply a
    payload (for example a ``request`` or ``warcinfo`` record).
    """

    def __init__(self, reason, record=None, chain=None, target_record=None):
        super(RevisitTargetTypeError, self).__init__(reason,
                                                     record=record,
                                                     chain=chain)
        self.target_record = target_record


#=================================================================
class RevisitPayloadError(RevisitResolveError):
    """ The referenced record was found, but its payload could not be read
    from the beginning, so it can not stand in for the revisit payload.
    """


#=================================================================
class RevisitDigestError(RevisitResolveError):
    """ The payload found for a revisit does not match the payload digest
    recorded for it, so it must not be returned as valid content.
    """

    def __init__(self, reason, record=None, chain=None,
                 expected=None, actual=None):
        super(RevisitDigestError, self).__init__(reason,
                                                 record=record,
                                                 chain=chain)
        self.expected = expected
        self.actual = actual
