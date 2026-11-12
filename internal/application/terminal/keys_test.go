package terminal

import (
	"fmt"
	"sync/atomic"
)

var testKeys atomic.Int64

// tk is a fresh client_key for a test's payment / change (one per tap,
// contract 2.10s).
func tk() string { return fmt.Sprintf("tk-%08d", testKeys.Add(1)) }
