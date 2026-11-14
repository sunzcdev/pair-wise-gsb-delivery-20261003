# The JSON feed parser
# Copyright 2017 Beat Bolli
# All rights reserved.
#
# This file is a part of feedparser.
#
# Redistribution and use in source and binary forms, with or without modification,
# are permitted provided that the following conditions are met:
#
# * Redistributions of source code must retain the above copyright notice,
#   this list of conditions and the following disclaimer.
# * Redistributions in binary form must reproduce the above copyright notice,
#   this list of conditions and the following disclaimer in the documentation
#   and/or other materials provided with the distribution.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS 'AS IS'
# AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
# IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
# ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
# LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
# CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
# SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
# INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
# CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
# ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
# POSSIBILITY OF SUCH DAMAGE.

import json
from typing import Any

from ..datetimes import _parse_date
from ..exceptions import JSONFeedValidationError
from ..sanitizer import sanitize_html
from ..util import FeedParserDict


class JSONParser:
    VERSIONS = {
        "https://jsonfeed.org/version/1": "json1",
        "https://jsonfeed.org/version/1.1": "json11",
    }
    FEED_FIELDS = (
        ("title", "title"),
        ("icon", "image"),
        ("home_page_url", "link"),
        ("description", "description"),
    )
    ITEM_FIELDS = (
        ("title", "title"),
        ("id", "guid"),
        ("url", "link"),
        ("summary", "summary"),
        ("external_url", "source"),
    )

    def __init__(
        self,
        baseuri=None,
        baselang=None,
        encoding=None,
        tolerant_entries: bool = False,
    ):
        self.baseuri = baseuri or ""
        self.lang = baselang or None
        self.encoding = encoding or "utf-8"  # character encoding
        self.tolerant_entries = tolerant_entries

        self.version = None
        self.feeddata = FeedParserDict()
        self.namespacesInUse = []
        self.entries = []
        # Diagnostics for entries that were skipped in tolerant mode.
        # Each item is {"index": int, "path": str, "reason": str}.
        self.skipped_entries: list[dict[str, Any]] = []

    def _error(self, path: str, reason: str) -> None:
        """Raise a validation error; only called in tolerant-entry mode.

        Raising a dedicated exception (instead of catching a broad
        ``Exception``) keeps programming errors such as ``TypeError`` from
        being mistaken for malformed feed data.
        """

        raise JSONFeedValidationError(path, reason)

    @staticmethod
    def _is_int(value: Any) -> bool:
        # ``bool`` is a subclass of ``int``; a boolean is never a valid
        # numeric size, so reject it explicitly.
        return isinstance(value, int) and not isinstance(value, bool)

    def feed(self, file):
        data = json.load(file)

        if self.tolerant_entries and not isinstance(data, dict):
            self._error("", "the top-level JSON value must be an object")

        v = data.get("version", "")
        try:
            self.version = self.VERSIONS[v]
        except KeyError:
            raise ValueError("Unrecognized JSONFeed version '%s'" % v)

        if self.tolerant_entries and "items" not in data:
            self._error("items", "missing required feed field 'items'")
        items = data["items"]
        if self.tolerant_entries and not isinstance(items, list):
            self._error("items", "the 'items' field must be an array")

        for src, dst in self.FEED_FIELDS:
            if src in data and (
                not self.tolerant_entries or isinstance(data[src], str)
            ):
                self.feeddata[dst] = data[src]
            elif self.tolerant_entries and src in data:
                self._error(
                    src,
                    "the '%s' field must be a string" % src,
                )
        if "author" in data or "authors" in data:
            authors = data.get("authors")
            if authors is None:
                authors = [data["author"]]
            self._parse_authors(
                authors,
                self.feeddata,
                "authors" if "authors" in data else "author",
            )
        # TODO: hubs; expired has no RSS equivalent

        self.entries = []
        for index, item in enumerate(items):
            if not self.tolerant_entries:
                self.entries.append(self.parse_entry(item))
                continue
            try:
                entry = self.parse_entry(item, index)
            except JSONFeedValidationError as error:
                self.skipped_entries.append(
                    {
                        "index": index,
                        "path": error.path,
                        "reason": error.reason,
                    }
                )
            else:
                self.entries.append(entry)

    def parse_entry(self, e, index=None):
        path_prefix = "" if index is None else "items/%d/" % index

        if self.tolerant_entries and not isinstance(e, dict):
            self._error("items/%d" % index, "a feed item must be an object")

        entry = FeedParserDict()
        for src, dst in self.ITEM_FIELDS:
            if src in e and (
                not self.tolerant_entries or isinstance(e[src], str)
            ):
                entry[dst] = e[src]
            elif self.tolerant_entries and src in e:
                self._error(
                    path_prefix + src,
                    "the '%s' field must be a string" % src,
                )

        if "content_text" in e:
            if self.tolerant_entries and not isinstance(
                e["content_text"], str
            ):
                self._error(
                    path_prefix + "content_text",
                    "the 'content_text' field must be a string",
                )
            entry["content"] = c = FeedParserDict()
            c["value"] = e["content_text"]
            c["type"] = "text"
        elif "content_html" in e:
            if self.tolerant_entries and not isinstance(
                e["content_html"], str
            ):
                self._error(
                    path_prefix + "content_html",
                    "the 'content_html' field must be a string",
                )
            entry["content"] = c = FeedParserDict()
            c["value"] = sanitize_html(
                e["content_html"], self.encoding, "application/json"
            )
            c["type"] = "html"

        if "date_published" in e:
            if self.tolerant_entries and not isinstance(
                e["date_published"], str
            ):
                self._error(
                    path_prefix + "date_published",
                    "the 'date_published' field must be a string",
                )
            entry["published"] = e["date_published"]
            entry["published_parsed"] = _parse_date(e["date_published"])
        if "date_updated" in e:
            if self.tolerant_entries and not isinstance(
                e["date_updated"], str
            ):
                self._error(
                    path_prefix + "date_updated",
                    "the 'date_updated' field must be a string",
                )
            entry["updated"] = e["date_updated"]
            entry["updated_parsed"] = _parse_date(e["date_updated"])

        if "tags" in e:
            if self.tolerant_entries and not isinstance(e["tags"], list):
                self._error(
                    path_prefix + "tags",
                    "the 'tags' field must be an array",
                )
            entry["category"] = e["tags"]

        if "author" in e or "authors" in e:
            authors = e.get("authors")
            if authors is None:
                authors = [e["author"]]
            self._parse_authors(
                authors,
                entry,
                path_prefix
                + ("authors" if "authors" in e else "author"),
            )

        if "attachments" in e:
            attachments = e["attachments"]
            if self.tolerant_entries and not isinstance(attachments, list):
                self._error(
                    path_prefix + "attachments",
                    "the 'attachments' field must be an array",
                )
            entry["enclosures"] = [
                self._parse_attachment(
                    a, "%sattachments/%d" % (path_prefix, position)
                )
                for position, a in enumerate(attachments)
            ]

        return entry

    def _parse_authors(self, authors, dest, path):
        """Populate ``dest`` with one or more JSON Feed author objects.

        Malformed author members are isolated at the same level as malformed
        attachment members: a validation error aborts the item currently
        being parsed, which the tolerant loop skips while recording its
        source index, path and reason.
        """

        if self.tolerant_entries and not isinstance(authors, list):
            self._error(path, "an author list must be an array")

        parsed = []
        for position, parent in enumerate(authors):
            member_path = "%s/%d" % (path, position)
            if self.tolerant_entries and not isinstance(parent, dict):
                self._error(member_path, "an author must be an object")
            author = FeedParserDict()
            if "name" in parent:
                if self.tolerant_entries and not isinstance(
                    parent["name"], str
                ):
                    self._error(
                        member_path + "/name",
                        "the author 'name' field must be a string",
                    )
                author["name"] = parent["name"]
            if "url" in parent:
                if self.tolerant_entries and not isinstance(
                    parent["url"], str
                ):
                    self._error(
                        member_path + "/url",
                        "the author 'url' field must be a string",
                    )
                if parent["url"].startswith("mailto:"):
                    author["email"] = parent["url"][7:]
                else:
                    author["href"] = parent["url"]
            parsed.append(author)

        if parsed:
            dest["authors"] = parsed
            dest["author_detail"] = parsed[0]
            if "name" in parsed[0]:
                dest["author"] = parsed[0]["name"]

    def _parse_attachment(self, attachment, path):
        if self.tolerant_entries and not isinstance(attachment, dict):
            self._error(path, "an attachment must be an object")
        if self.tolerant_entries and not isinstance(
            attachment.get("url"), str
        ):
            self._error(
                path + "/url",
                "the attachment 'url' field must be a string",
            )
        if self.tolerant_entries and not isinstance(
            attachment.get("mime_type"), str
        ):
            self._error(
                path + "/mime_type",
                "the attachment 'mime_type' field must be a string",
            )
        if (
            self.tolerant_entries
            and "size_in_bytes" in attachment
            and not self._is_int(attachment["size_in_bytes"])
        ):
            self._error(
                path + "/size_in_bytes",
                "the 'size_in_bytes' field must be an integer",
            )

        enc = FeedParserDict()
        enc["href"] = attachment["url"]
        enc["type"] = attachment["mime_type"]
        if "size_in_bytes" in attachment:
            enc["length"] = attachment["size_in_bytes"]
        return enc
