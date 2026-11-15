import typing

EXCLUDE: typing.Final = "exclude"
INCLUDE: typing.Final = "include"
RAISE: typing.Final = "raise"


class _Missing:
    def __bool__(self):
        return False

    def __copy__(self):
        return self

    def __deepcopy__(self, _):
        return self

    def __repr__(self):
        return "<marshmallow.missing>"

    def __len__(self):
        return 0


missing: typing.Final = _Missing()


class _Delete:
    """Sentinel class for the `DELETE` marker.

    Instances of this class can never be confused with legitimate
    business data, which consists of plain Python/JSON-like values.
    """

    def __bool__(self):
        return False

    def __copy__(self):
        return self

    def __deepcopy__(self, _):
        return self

    def __repr__(self):
        return "<marshmallow.DELETE>"

    def __len__(self):
        return 0


#: Singleton marker used to request deletion of a field's value when
#: loading data in patch mode (`Schema.load_patch <marshmallow.Schema.load_patch>`).
#: Because it is a unique object rather than a string, number, or ``None``,
#: it cannot collide with any valid business value.
DELETE: typing.Final = _Delete()
