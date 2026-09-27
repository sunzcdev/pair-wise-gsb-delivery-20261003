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

package errors

import (
	"errors"
	"fmt"
)

// Global errors for explicit authorization sessions defined here.
var (
	ErrSessionNotFound = errors.New("authorization session not found")
	ErrRoleNotGranted  = errors.New("role is not granted to the session user")
)

// DynamicMutexViolationError represents a dynamic separation of duties
// violation raised when activating a role in an authorization session.
type DynamicMutexViolationError struct {
	Group     string
	Role      string
	Conflicts []string
}

func (e *DynamicMutexViolationError) Error() string {
	return fmt.Sprintf("dynamic mutex violation [%s]: activating role '%s' conflicts with active roles %v",
		e.Group, e.Role, e.Conflicts)
}

// NewDynamicMutexViolationError creates a new dynamic mutex violation error.
func NewDynamicMutexViolationError(group, role string, conflicts []string) error {
	return &DynamicMutexViolationError{
		Group:     group,
		Role:      role,
		Conflicts: conflicts,
	}
}
