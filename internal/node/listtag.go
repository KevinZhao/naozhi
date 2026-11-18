package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// The reverse link's list fetches (fetch_sessions / fetch_projects /
// fetch_discovered) are polled every 10 s and mostly answer what they answered
// last time. The primary sends the tag of the body it holds as params
// {"etag": …}; a node whose body hashes the same answers listUnchanged
// instead, as an HTTP node answers 304. An older node ignores the params and
// answers in full; an older primary sends none and always gets the body.

// listUnchanged is the result standing in for an unchanged list. A list
// result is always a JSON array, so an object cannot be mistaken for one.
var listUnchanged = json.RawMessage(`{"not_modified":true}`)

// ListTag hashes one list body as both sides see it on the wire.
func ListTag(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// AnswerListFetch is the node's reply to a list fetch whose body is body:
// listUnchanged when params carry body's tag, else body. Params that do not
// parse are ignored, as they were before the tag existed.
func AnswerListFetch(params, body json.RawMessage) json.RawMessage {
	if len(params) == 0 {
		return body
	}
	var p struct {
		ETag string `json:"etag"`
	}
	if json.Unmarshal(params, &p) != nil || p.ETag == "" || p.ETag != ListTag(body) {
		return body
	}
	return listUnchanged
}

// listCache is the last list body one fetch method decoded, under its tag.
type listCache struct {
	mu   sync.Mutex
	tag  string
	rows []map[string]any
}

func (lc *listCache) load() (string, []map[string]any) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.tag, lc.rows
}

func (lc *listCache) store(tag string, rows []map[string]any) {
	lc.mu.Lock()
	lc.tag, lc.rows = tag, rows
	lc.mu.Unlock()
}

// fetchList runs one list fetch, asking with the tag of the body it holds and
// answering an unchanged reply from that body. Any failure drops the cache so
// the next poll fetches in full. The returned maps are fresh at the top level
// (the cache manager tags them with the node); nested values are shared with
// the cache and must not be mutated.
func (c *ReverseConn) fetchList(ctx context.Context, method string, lc *listCache) ([]map[string]any, error) {
	tag, cached := lc.load()
	var params any
	if tag != "" {
		params = map[string]string{"etag": tag}
	}
	raw, err := c.rpc(ctx, method, params)
	if err != nil {
		lc.store("", nil)
		return nil, err
	}
	if tag != "" && bytes.Equal(bytes.TrimSpace(raw), listUnchanged) {
		return cloneSessions(cached), nil
	}
	var result []map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		lc.store("", nil)
		return result, err
	}
	lc.store(ListTag(raw), result)
	return cloneSessions(result), nil
}
