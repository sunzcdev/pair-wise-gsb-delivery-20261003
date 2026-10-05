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

package advisor

import (
	"fmt"

	casbin "github.com/casbin/casbin/v3"
)

// ApplyOnCopy applies advice to a fresh, detached copy of base and returns
// that copy. The base enforcer is never modified. Deny policies and any
// rules not implied by advice are left untouched. The returned enforcer is
// meant for verification (for example re-running Enforce on the denied
// requests); persistence and watchers are disabled on the copy.
func ApplyOnCopy(base *casbin.Enforcer, advice Advice) (*casbin.Enforcer, error) {
	clone, err := cloneEnforcer(base)
	if err != nil {
		return nil, err
	}

	for _, role := range advice.Roles {
		if advice.Domain != "" {
			if _, err := clone.AddRoleForUserInDomain(advice.Subject, role, advice.Domain); err != nil {
				return nil, fmt.Errorf("apply advice: grant %q to %q in domain %q: %w",
					role, advice.Subject, advice.Domain, err)
			}
			continue
		}
		if _, err := clone.AddRoleForUser(advice.Subject, role); err != nil {
			return nil, fmt.Errorf("apply advice: grant %q to %q: %w",
				role, advice.Subject, err)
		}
	}

	return clone, nil
}
