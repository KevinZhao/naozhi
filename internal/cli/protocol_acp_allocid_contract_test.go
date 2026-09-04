package cli

import "sync/atomic"

// nextID must stay atomic.Int64: allocID narrows it to int for RPCRequest.ID,
// which is lossless only on 64-bit builds. Compile-time pin so a type change
// forces a conscious re-review of that narrowing.
var _ *atomic.Int64 = &(&ACPProtocol{}).nextID
