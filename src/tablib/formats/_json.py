""" Tablib - JSON Support
"""

__lazy_modules__ = {"decimal", "json", "uuid"}

import decimal
import json
from uuid import UUID

import tablib


def serialize_objects_handler(obj):
    if isinstance(obj, (decimal.Decimal, UUID)):
        return str(obj)
    elif hasattr(obj, 'isoformat'):
        return obj.isoformat()
    else:
        return obj


class JSONFormat:
    title = 'json'
    extensions = ('json', 'jsn')

    @classmethod
    def export_set(cls, dataset, **kwargs):
        """Returns JSON representation of Dataset.

        Keyword arguments are forwarded to :meth:`Dataset._package`: mapping
        output is strict by default (duplicated or blank headers raise
        ``AmbiguousHeadersError``); pass ``pairs=True`` to emit lists of
        ``[header, value]`` pairs preserving duplicate headers and order, or
        ``strict=False`` for the legacy lenient behavior.
        """
        return json.dumps(
            dataset._package(**kwargs), default=serialize_objects_handler,
            ensure_ascii=False,
        )

    @classmethod
    def export_book(cls, databook, **kwargs):
        """Returns JSON representation of Databook."""
        return json.dumps(
            databook._package(**kwargs), default=serialize_objects_handler,
            ensure_ascii=False,
        )

    @classmethod
    def import_set(cls, dset, in_stream, pairs=False):
        """Returns dataset from JSON stream.

        Pass ``pairs=True`` to import rows expressed as lists of
        ``[header, value]`` pairs, preserving duplicate headers and order.
        """

        dset.wipe()
        payload = json.load(in_stream)
        if pairs:
            dset._load_pairs(payload)
        else:
            dset.dict = payload

    @classmethod
    def import_book(cls, dbook, in_stream, pairs=False):
        """Returns databook from JSON stream."""

        dbook.wipe()
        for sheet in json.load(in_stream):
            data = tablib.Dataset()
            data.title = sheet['title']
            if pairs:
                data._load_pairs(sheet['data'])
            else:
                data.dict = sheet['data']
            dbook.add_sheet(data)

    @classmethod
    def detect(cls, stream):
        """Returns True if given stream is valid JSON."""
        try:
            json.load(stream)
            return True
        except (TypeError, ValueError):
            return False
