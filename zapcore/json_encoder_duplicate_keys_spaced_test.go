// Copyright (c) 2026 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zapcore_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.uber.org/zap"
	. "go.uber.org/zap/zapcore"
)

func TestJSONDuplicateKeyPolicySpacedKeepLast(t *testing.T) {
	cfg := duplicateEncoderConfig()
	cfg.DuplicateKeyPolicy = DuplicateKeyKeepLast

	var output bytes.Buffer
	core := NewCore(NewConsoleEncoder(cfg), AddSync(&output), DebugLevel)
	logger := zap.New(core).With(
		zap.String("first", "v"),
		zap.String("request_id", "from-with"),
	)
	logger.Info("message", zap.String("request_id", "from-call"))

	assert.Contains(t, output.String(), `{"first": "v", "request_id": "from-call"}`)
}
