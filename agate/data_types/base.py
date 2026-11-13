from agate.exceptions import CastError, DataTypeError

#: Default values which will be automatically cast to :code:`None`
DEFAULT_NULL_VALUES = ('', 'na', 'n/a', 'none', 'null', '.')


class DataType:  # pragma: no cover
    """
    Specifies how values should be parsed when creating a :class:`.Table`.

    :param null_values: A sequence of values which should be cast to
        :code:`None` when encountered by this data type.
    """
    def __init__(self, null_values=DEFAULT_NULL_VALUES):
        self.null_values = [v.lower() for v in null_values]

    def test(self, d):
        """
        Test, for purposes of type inference, if a value could possibly be
        coerced to this data type.

        This is really just a thin wrapper around :meth:`DataType.cast`.
        """
        try:
            self.cast(d)
        except CastError:
            return False

        return True

    def semantic_config(self):
        """
        Return a mapping of the configuration settings which affect how this
        type parses or serializes values, or :code:`None` if they can not be
        determined.

        Custom data types may override this method to declare which of their
        settings are semantically significant. Only the settings included in
        the returned mapping are considered when checking compatibility, so
        cached or derived attributes should not be included. The default
        implementation returns :code:`None`, indicating that compatibility
        with other instances can not be determined.
        """
        return None

    def check_compatible(self, other):
        """
        Compare the semantic configuration of this instance with another
        instance of the same class.

        :param other:
            Another instance of this data type's class.
        :returns:
            A dict of conflicting settings, mapping each setting name to a
            :code:`(self_value, other_value)` tuple. An empty dict means the
            two instances are compatible. :code:`None` means compatibility
            can not be determined.
        """
        if type(self) is not type(other):
            raise DataTypeError('Can not compare %s with %s.' % (type(self).__name__, type(other).__name__))

        self_config = self.semantic_config()
        other_config = other.semantic_config()

        if self_config is None or other_config is None:
            return None

        conflicts = {}

        for name, value in self_config.items():
            if name in other_config and other_config[name] != value:
                conflicts[name] = (value, other_config[name])

        return conflicts

    def cast(self, d):
        """
        Coerce a given string value into this column's data type.
        """
        raise NotImplementedError

    def csvify(self, d):
        """
        Format a given native value for CSV serialization.
        """
        if d is None:
            return None

        return str(d)

    def jsonify(self, d):
        """
        Format a given native value for JSON serialization.
        """
        if d is None:
            return None

        return str(d)
