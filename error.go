// Copyright 2016 Unknwon
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package ini

import (
	"fmt"
)

// ErrDelimiterNotFound indicates the error type of no delimiter is found which there should be one.
type ErrDelimiterNotFound struct {
	Line string
}

// IsErrDelimiterNotFound returns true if the given error is an instance of ErrDelimiterNotFound.
func IsErrDelimiterNotFound(err error) bool {
	_, ok := err.(ErrDelimiterNotFound)
	return ok
}

func (err ErrDelimiterNotFound) Error() string {
	return fmt.Sprintf("key-value delimiter not found: %s", err.Line)
}

// ErrEmptyKeyName indicates the error type of no key name is found which there should be one.
type ErrEmptyKeyName struct {
	Line string
}

// IsErrEmptyKeyName returns true if the given error is an instance of ErrEmptyKeyName.
func IsErrEmptyKeyName(err error) bool {
	_, ok := err.(ErrEmptyKeyName)
	return ok
}

func (err ErrEmptyKeyName) Error() string {
	return fmt.Sprintf("empty key name: %s", err.Line)
}

// ErrReloadSource indicates that a transactional reload failed because of a
// specific data source.
type ErrReloadSource struct {
	// Index is the position of the failed data source in the file's source
	// list, in the order sources were passed to Load, LoadSources or Append.
	Index int
	// Err is the underlying error: a read, parse, or reader-close error.
	Err error
}

// IsErrReloadSource returns true if the given error is an instance of ErrReloadSource.
func IsErrReloadSource(err error) bool {
	_, ok := err.(ErrReloadSource)
	return ok
}

func (err ErrReloadSource) Error() string {
	return fmt.Sprintf("error reloading data source %d: %v", err.Index, err.Err)
}

// Unwrap returns the underlying error that caused the reload to fail.
func (err ErrReloadSource) Unwrap() error {
	return err.Err
}
