package lz4_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestDebugCostPerf(t *testing.T) {
	src := bytes.Repeat([]byte("offline logs are repetitive enough to benefit from cost parsing; "), 4096)
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	var c lz4.CompressorCost
	n, _ := c.CompressBlock(src, dst)
	t.Log(n)
	fmt.Println(len(src), n)
}
