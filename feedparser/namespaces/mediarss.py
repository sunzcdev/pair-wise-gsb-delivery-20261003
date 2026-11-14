# Support for the Media RSS format
# Copyright 2010-2025 Kurt McKee <contactme@kurtmckee.org>
# Copyright 2002-2008 Mark Pilgrim
# All rights reserved.
#
# This file is a part of feedparser.
#
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions are met:
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

import copy

from ..util import FeedParserDict

# Metadata that may be declared directly inside a ``media:group`` and act as
# default values for the enclosed ``media:content`` elements.
_MEDIA_MULTI_VALUE_KEYS = frozenset(
    ("media_credit", "media_thumbnail", "tags")
)
_MEDIA_SCALAR_KEYS = (
    "media_license",
    "media_rating",
    "media_restriction",
    "summary",
    "title",
)


class Namespace:
    supported_namespaces = {
        # Canonical namespace
        "http://search.yahoo.com/mrss/": "media",
        # Old namespace (no trailing slash)
        "http://search.yahoo.com/mrss": "media",
    }

    def _media_ensure_state(self):
        """Initialize the per-parser Media RSS tracking state on first use."""

        if not hasattr(self, "_media_group_stack"):
            # Stack of the groups currently being parsed.  Media RSS does
            # not nest groups, but the stack makes the active group boundary
            # explicit and guarantees a content cannot inherit from a group
            # that has already ended.
            self._media_group_stack = []
            # Structured records of media:content elements that appear outside
            # of any media:group.  They never inherit group-level defaults.
            self._media_ungrouped_content = []

    def _media_active_group(self):
        """Return the active group record, or ``None`` outside a group."""

        self._media_ensure_state()
        if self._media_group_stack:
            return self._media_group_stack[-1]
        return None

    def _media_active_content(self):
        """Return the structured record of the active media:content."""

        group = self._media_active_group()
        if group is not None:
            contents = group["media_content"]
        else:
            self._media_ensure_state()
            contents = self._media_ungrouped_content
        if contents:
            return contents[-1]
        return None

    def _media_record(self, key, value):
        """Store structured metadata on a group or a media:content.

        Every value is deep-copied so the structured representation never
        shares a mutable container with the legacy flat fields.
        """

        group = self._media_active_group()
        if group is None:
            content = self._media_active_content()
            if content is None:
                return
            target = content
        elif self._media_active_content() is None:
            target = group["media_defaults"]
        else:
            target = self._media_active_content()

        value = copy.deepcopy(value)
        if key in _MEDIA_MULTI_VALUE_KEYS:
            target.setdefault(key, [])
            if isinstance(value, list):
                target[key].extend(value)
            else:
                target[key].append(value)
        else:
            target[key] = value

    @staticmethod
    def _media_effective_metadata(group_defaults, content_record):
        """Merge group defaults and a content record into effective metadata.

        An explicitly present content-level value, even an empty one, always
        wins over the group default; a missing content-level key inherits the
        default.  Multi-valued keys concatenate the group values followed by
        the content values, preserving source order.
        """

        effective = FeedParserDict()
        for key in (*_MEDIA_MULTI_VALUE_KEYS, *_MEDIA_SCALAR_KEYS):
            if key in _MEDIA_MULTI_VALUE_KEYS:
                values = [
                    *copy.deepcopy(group_defaults.get(key, [])),
                    *copy.deepcopy(content_record.get(key, [])),
                ]
                if values:
                    effective[key] = values
            elif key in content_record:
                effective[key] = copy.deepcopy(content_record[key])
            elif key in group_defaults:
                effective[key] = copy.deepcopy(group_defaults[key])
        return effective

    def _start_media_category(self, attrs_d):
        attrs_d.setdefault("scheme", "http://search.yahoo.com/mrss/category_schema")
        self._start_category(attrs_d)

    def _end_media_category(self):
        self._end_category()
        context = self._get_context()
        if context.get("tags"):
            self._media_record("tags", context["tags"][-1])

    def _end_media_keywords(self):
        context = self._get_context()
        before = len(context.get("tags", []))
        for term in self.pop("media_keywords").split(","):
            if term.strip():
                self._add_tag(term.strip(), None, None)
        for tag in context.get("tags", [])[before:]:
            self._media_record("tags", tag)

    def _start_media_title(self, attrs_d):
        self._start_title(attrs_d)

    def _end_media_title(self):
        title_depth = self.title_depth
        self._end_title()
        self.title_depth = title_depth
        value = self._get_context().get("title")
        if value:
            self._media_record("title", value)

    def _start_media_group(self, attrs_d):
        self._media_ensure_state()
        if not (self.inentry and not self.insource):
            return
        context = self._get_context()
        group = FeedParserDict(
            media_defaults=FeedParserDict(),
            media_content=[],
        )
        self._media_group_stack.append(group)
        context.setdefault("media_groups", [])
        context["media_groups"].append(group)

    def _end_media_group(self):
        self._media_ensure_state()
        if self._media_group_stack:
            self._media_group_stack.pop()

    def _start_media_rating(self, attrs_d):
        context = self._get_context()
        context.setdefault("media_rating", attrs_d)
        self.push("rating", 1)

    def _end_media_rating(self):
        rating = self.pop("rating")
        if rating is not None and rating.strip():
            context = self._get_context()
            context["media_rating"]["content"] = rating
            self._media_record("media_rating", context["media_rating"])

    def _start_media_credit(self, attrs_d):
        context = self._get_context()
        context.setdefault("media_credit", [])
        context["media_credit"].append(attrs_d)
        self.push("credit", 1)

    def _end_media_credit(self):
        credit = self.pop("credit")
        if credit is not None and credit.strip():
            context = self._get_context()
            context["media_credit"][-1]["content"] = credit
            self._media_record("media_credit", context["media_credit"][-1])

    def _start_media_description(self, attrs_d):
        self._start_description(attrs_d)

    def _end_media_description(self):
        # Pop the raw value before the base cleanup so an explicitly empty
        # description (``""``) can be distinguished from a missing one.
        value = self.pop(
            "content" if self._summaryKey == "content" else "description"
        )
        self.incontent -= 1
        self.contentparams.clear()
        self._summaryKey = None
        if value:
            if self.inentry and not self.insource:
                self.entries[-1].setdefault("summary", value)
            elif self.infeed or self.insource:
                self._get_context().setdefault("subtitle", value)
            self._media_record("summary", value)
        elif value is not None:
            # Explicitly empty: record it so it can override group defaults
            # instead of being treated as a missing value.
            self._media_record("summary", value)

    def _start_media_restriction(self, attrs_d):
        context = self._get_context()
        context.setdefault("media_restriction", attrs_d)
        self.push("restriction", 1)

    def _end_media_restriction(self):
        restriction = self.pop("restriction")
        if restriction is not None and restriction.strip():
            context = self._get_context()
            context["media_restriction"]["content"] = [
                cc.strip().lower() for cc in restriction.split(" ")
            ]
            self._media_record("media_restriction", context["media_restriction"])

    def _start_media_license(self, attrs_d):
        context = self._get_context()
        context.setdefault("media_license", attrs_d)
        self.push("license", 1)

    def _end_media_license(self):
        license_ = self.pop("license")
        if license_ is not None and license_.strip():
            context = self._get_context()
            context["media_license"]["content"] = license_
            self._media_record("media_license", context["media_license"])

    def _start_media_content(self, attrs_d):
        context = self._get_context()
        context.setdefault("media_content", [])
        context["media_content"].append(attrs_d)
        self._media_ensure_state()
        if not (self.inentry and not self.insource):
            return
        content_record = FeedParserDict(copy.deepcopy(attrs_d))
        group = self._media_active_group()
        if group is not None:
            group["media_content"].append(content_record)
        else:
            self._media_ungrouped_content.append(content_record)
            context.setdefault("media_ungrouped_content", [])
            context["media_ungrouped_content"].append(content_record)

    def _end_media_content(self):
        group = self._media_active_group()
        content = self._media_active_content()
        if content is None:
            return
        group_defaults = group["media_defaults"] if group is not None else {}
        content["media_effective"] = self._media_effective_metadata(
            group_defaults, content
        )

    def _start_media_thumbnail(self, attrs_d):
        context = self._get_context()
        context.setdefault("media_thumbnail", [])
        self.push("url", 1)  # new
        context["media_thumbnail"].append(attrs_d)

    def _end_media_thumbnail(self):
        url = self.pop("url")
        context = self._get_context()
        if url is not None and url.strip():
            if "url" not in context["media_thumbnail"][-1]:
                context["media_thumbnail"][-1]["url"] = url
        self._media_record("media_thumbnail", context["media_thumbnail"][-1])

    def _start_media_player(self, attrs_d):
        self.push("media_player", 0)
        self._get_context()["media_player"] = FeedParserDict(attrs_d)

    def _end_media_player(self):
        value = self.pop("media_player")
        context = self._get_context()
        context["media_player"]["content"] = value
        self._media_record("media_player", context["media_player"])
