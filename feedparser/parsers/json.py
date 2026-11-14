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

from ..datetimes import _parse_date
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

    def __init__(self, baseuri=None, baselang=None, encoding=None):
        self.baseuri = baseuri or ""
        self.lang = baselang or None
        self.encoding = encoding or "utf-8"  # character encoding

        self.version = None
        self.feeddata = FeedParserDict()
        self.namespacesInUse = []
        self.entries = []
        self.errors = []

    def feed(self, file):
        data = json.load(file)

        v = data.get("version", "")
        try:
            self.version = self.VERSIONS[v]
        except KeyError:
            raise ValueError("Unrecognized JSONFeed version '%s'" % v)

        for src, dst in self.FEED_FIELDS:
            if src in data:
                self.feeddata[dst] = data[src]
        self.parse_authors(data, self.feeddata, "feed")
        # TODO: hubs; expired has no RSS equivalent

        self.entries = [self.parse_entry(e, i) for i, e in enumerate(data["items"])]

        if self.errors:
            raise ValueError("; ".join(self.errors))

    def parse_entry(self, e, index=None):
        entry = FeedParserDict()
        for src, dst in self.ITEM_FIELDS:
            if src in e:
                entry[dst] = e[src]

        if "content_text" in e:
            entry["content"] = c = FeedParserDict()
            c["value"] = e["content_text"]
            c["type"] = "text"
        elif "content_html" in e:
            entry["content"] = c = FeedParserDict()
            c["value"] = sanitize_html(
                e["content_html"], self.encoding, "application/json"
            )
            c["type"] = "html"

        if "date_published" in e:
            entry["published"] = e["date_published"]
            entry["published_parsed"] = _parse_date(e["date_published"])
        if "date_updated" in e:
            entry["updated"] = e["date_modified"]
            entry["updated_parsed"] = _parse_date(e["date_modified"])

        if "tags" in e:
            entry["category"] = e["tags"]

        if "authors" in e or "author" in e:
            self.parse_authors(e, entry, "items[%s]" % index)
        else:
            self.inherit_authors(entry)

        if "attachments" in e:
            entry["enclosures"] = [self.parse_attachment(a) for a in e["attachments"]]

        return entry

    def parse_authors(self, parent, dest, source):
        if "authors" in parent:
            authors = parent["authors"]
            if not isinstance(authors, list):
                self.errors.append("%s.authors must be a list" % source)
                self._set_authors(dest, [])
                return

            details = []
            for index, author in enumerate(authors):
                try:
                    details.append(self.parse_author(author))
                except ValueError as error:
                    self.errors.append(
                        "%s.authors[%d]: %s" % (source, index, error)
                    )
            self._set_authors(dest, details)
            return

        if "author" in parent:
            try:
                details = [self.parse_author(parent["author"])]
            except ValueError as error:
                self.errors.append("%s.author: %s" % (source, error))
                details = []
            self._set_authors(dest, details)

    def inherit_authors(self, dest):
        if "authors" not in self.feeddata:
            return

        authors = [FeedParserDict(author) for author in self.feeddata["authors"]]
        self._set_authors(dest, authors)

    @staticmethod
    def parse_author(parent, dest=None):
        if not isinstance(parent, dict):
            raise ValueError("author must be an object")

        detail = FeedParserDict()
        has_detail = False

        if "name" in parent:
            if not isinstance(parent["name"], str):
                raise ValueError("author name must be a string")
            detail["name"] = parent["name"]
            has_detail = True

        if "url" in parent:
            if not isinstance(parent["url"], str):
                raise ValueError("author url must be a string")
            if parent["url"].startswith("mailto:"):
                detail["email"] = parent["url"][7:]
            else:
                detail["href"] = parent["url"]
            has_detail = True

        if "avatar" in parent:
            if not isinstance(parent["avatar"], str):
                raise ValueError("author avatar must be a string")
            detail["avatar"] = parent["avatar"]
            has_detail = True

        if not has_detail:
            raise ValueError("author has no name, url, or avatar")

        if dest is not None:
            dest["author_detail"] = detail
            if "name" in detail:
                dest["author"] = detail["name"]
            return None

        return detail

    @staticmethod
    def _set_authors(dest, authors):
        dest["authors"] = authors
        if not authors:
            return

        detail = authors[-1]
        dest["author_detail"] = detail
        if "name" in detail:
            dest["author"] = detail["name"]

    @staticmethod
    def parse_attachment(attachment):
        enc = FeedParserDict()
        enc["href"] = attachment["url"]
        enc["type"] = attachment["mime_type"]
        if "size_in_bytes" in attachment:
            enc["length"] = attachment["size_in_bytes"]
        return enc
