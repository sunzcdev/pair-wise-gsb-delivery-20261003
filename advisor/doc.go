// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package advisor suggests minimum-cost role grants that make denied
// requests pass. It is strictly read-only with respect to the enforcer
// passed in, never removes deny policies, and never grants a role that is
// not explicitly offered as a candidate.
//
// Use [Suggest] to compute recommendations and [ApplyOnCopy] to verify a
// recommendation on a detached enforcer:
//
//	result, err := advisor.Suggest(e, advisor.Options{
//	    Subject:        "alice",
//	    CandidateRoles: []string{"reader", "writer"},
//	    RoleCost:       map[string]int{"reader": 1, "writer": 3},
//	    Requests: [][]interface{}{
//	        {"doc1", "read"},
//	        {"doc1", "write"},
//	    },
//	})
//	switch result.Status {
//	case advisor.StatusFound:
//	    for _, advice := range result.Advice {
//	        verified, err := advisor.ApplyOnCopy(e, advice)
//	        // verified is an independent enforcer with the advice applied.
//	    }
//	case advisor.StatusUnsatisfiable:
//	    // No candidate combination works (for example an explicit deny).
//	case advisor.StatusBudgetExhausted:
//	    // Search budget ran out; result is not proven minimal.
//	}
package advisor
