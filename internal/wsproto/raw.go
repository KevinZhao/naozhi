package wsproto

import (
	"encoding/json"
	"fmt"
)

// Pre-marshaled hot-path frames: sent from read pumps and auth failure paths
// where allocating an encoder per send would be waste. Generated from the
// frame structs at init so the bytes cannot drift from the types (they used
// to be hand-written const strings in internal/server/wshub.go); converted
// to []byte at each send site so no shared buffer is appended to
// concurrently.
var (
	RawAuthOK          = mustMarshal(NewAuthOK())
	RawPong            = mustMarshal(NewPong())
	RawAuthFailInvalid = mustMarshal(NewAuthFail(AuthFail{Error: "invalid token"}))
	RawErrNotAuth      = mustMarshal(NewError(Error{Error: "not authenticated"}))
	RawErrRateLimited  = mustMarshal(NewError(Error{Error: "rate limited"}))
)

func mustMarshal(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("wsproto: marshal %T: %v", v, err))
	}
	return string(data)
}
