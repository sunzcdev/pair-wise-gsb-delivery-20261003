""" Tablib - YAML Support.
"""

__lazy_modules__ = {"yaml"}

import yaml

import tablib


class YAMLFormat:
    title = 'yaml'
    extensions = ('yaml', 'yml')

    @classmethod
    def export_set(cls, dataset, **kwargs):
        """Returns YAML representation of Dataset.

        Keyword arguments are forwarded to :meth:`Dataset._package`: mapping
        output is strict by default (duplicated or blank headers raise
        ``AmbiguousHeadersError``); pass ``pairs=True`` to emit lists of
        ``[header, value]`` pairs preserving duplicate headers and order, or
        ``strict=False`` for the legacy lenient behavior.
        """
        return yaml.safe_dump(
            dataset._package(**kwargs),
            default_flow_style=None,
            allow_unicode=True,
            sort_keys=False,
        )

    @classmethod
    def export_book(cls, databook, **kwargs):
        """Returns YAML representation of Databook."""
        return yaml.safe_dump(
            databook._package(**kwargs),
            default_flow_style=None,
            allow_unicode=True,
            sort_keys=False,
        )

    @classmethod
    def import_set(cls, dset, in_stream, pairs=False):
        """Returns dataset from YAML stream.

        Pass ``pairs=True`` to import rows expressed as lists of
        ``[header, value]`` pairs, preserving duplicate headers and order.
        """

        dset.wipe()
        payload = yaml.safe_load(in_stream)
        if pairs:
            dset._load_pairs(payload)
        else:
            dset.dict = payload

    @classmethod
    def import_book(cls, dbook, in_stream, pairs=False):
        """Returns databook from YAML stream."""

        dbook.wipe()

        for sheet in yaml.safe_load(in_stream):
            data = tablib.Dataset()
            data.title = sheet['title']
            if pairs:
                data._load_pairs(sheet['data'])
            else:
                data.dict = sheet['data']
            dbook.add_sheet(data)

    @classmethod
    def detect(cls, stream):
        """Returns True if given stream is valid YAML."""
        try:
            _yaml = yaml.safe_load(stream)
            if isinstance(_yaml, (list, tuple, dict)):
                return True
            else:
                return False
        except (yaml.parser.ParserError, yaml.reader.ReaderError,
                yaml.scanner.ScannerError):
            return False
