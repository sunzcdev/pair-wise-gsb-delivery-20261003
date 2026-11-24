# -*- coding: utf-8 -*-
#
# Copyright 2012-2015 Spotify AB
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""
Possible values for a Task's status in the Scheduler
"""

PENDING = "PENDING"
FAILED = "FAILED"
DONE = "DONE"
RUNNING = "RUNNING"
BATCH_RUNNING = "BATCH_RUNNING"
SUSPENDED = "SUSPENDED"  # Only kept for backward compatibility with old clients
UNKNOWN = "UNKNOWN"
DISABLED = "DISABLED"

#
# Blocking sub-states for external tasks (whose output is produced outside of
# Luigi). The task itself stays PENDING so that tasks depending on it keep
# waiting, while ``external_state`` records *why* it is blocked:
#
# EXTERNAL_MISSING:
#     The completion probe ran successfully and reported that the output does
#     not exist yet.
# EXTERNAL_PROBE_FAILED:
#     The completion probe itself raised (e.g. the metastore / storage was
#     temporarily unreachable). This is a transient condition and the probe is
#     retried with bounded backoff instead of being treated as a missing
#     output.
#
EXTERNAL_MISSING = "EXTERNAL_MISSING"
EXTERNAL_PROBE_FAILED = "EXTERNAL_PROBE_FAILED"
