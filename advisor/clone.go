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

// cloneEnforcer builds a fully detached enforcer that shares nothing
// mutable with src: the model (including every policy rule) is deep copied
// and fresh role managers are populated from the copied grouping policy.
// The clone has no adapter, so trial grants are never persisted and no
// watcher is notified.
func cloneEnforcer(src *casbin.Enforcer) (*casbin.Enforcer, error) {
	clone, err := casbin.NewEnforcer(src.GetModel().Copy())
	if err != nil {
		return nil, fmt.Errorf("clone enforcer: %w", err)
	}
	clone.EnableAutoSave(false)
	clone.EnableAutoNotifyWatcher(false)
	if err := clone.BuildRoleLinks(); err != nil {
		return nil, fmt.Errorf("build role links for clone: %w", err)
	}
	return clone, nil
}
