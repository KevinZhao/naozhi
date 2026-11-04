package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// A direct node serves its files over the same HTTP file API the primary's
// dashboard uses; these proxy it with the bearer token. Bodies are bounded:
// a listing or an exists batch is small JSON, a preview carries at most the
// node's 1 MB of text (escaped), a raw segment at most MaxFileChunk.
const (
	maxFileListBody    = 4 << 20
	maxFilePreviewBody = 8 << 20
	maxFileExistsBody  = 1 << 20
	maxOutputsBody     = 4 << 20
)

// fileRootQuery is the file API's root parameter for r.
func fileRootQuery(r FileRoot) string {
	if r.Key != "" {
		return "root=" + url.QueryEscape("session:"+r.Key)
	}
	return "project=" + url.QueryEscape(r.Project)
}

// fileRefusalFrom reads a non-2xx file API answer as the refusal it carries.
func fileRefusalFrom(resp *http.Response) FileRefusal {
	var v struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(body, &v)
	return FileRefusal{Status: resp.StatusCode, Error: v.Error}
}

// getFileJSON GETs path and decodes a 200 into out; any other status comes
// back as the refusal.
func (n *HTTPClient) getFileJSON(ctx context.Context, path string, limit int64, out any) (FileRefusal, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return FileRefusal{}, fmt.Errorf("files from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fileRefusalFrom(resp), nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out); err != nil {
		return FileRefusal{}, fmt.Errorf("decode files from %s: %w", n.ID, err)
	}
	return FileRefusal{}, nil
}

func (n *HTTPClient) ListFiles(ctx context.Context, p FilesListParams) (FileList, error) {
	path := "/api/projects/files/list?" + fileRootQuery(p.FileRoot)
	if p.Dir != "" {
		path += "&dir=" + url.QueryEscape(p.Dir)
	}
	if p.ShowHidden {
		path += "&show_hidden=1"
	}
	var out FileList
	rf, err := n.getFileJSON(ctx, path, maxFileListBody, &out)
	if rf.Status != 0 {
		return FileList{FileRefusal: rf}, nil
	}
	return out, err
}

func (n *HTTPClient) ReadFile(ctx context.Context, p FileReadParams) (FileRead, error) {
	path := "/api/projects/file?" + fileRootQuery(p.FileRoot) + "&path=" + url.QueryEscape(p.Path)
	if p.Mode == "preview" {
		var v struct {
			Content   string `json:"content"`
			Size      int64  `json:"size"`
			Mime      string `json:"mime"`
			Truncated bool   `json:"truncated"`
			Binary    bool   `json:"binary"`
		}
		rf, err := n.getFileJSON(ctx, path+"&mode=preview", maxFilePreviewBody, &v)
		if err != nil || rf.Status != 0 {
			return FileRead{FileRefusal: rf}, err
		}
		return FileRead{Size: v.Size, Mime: v.Mime, Truncated: v.Truncated, Binary: v.Binary, Data: []byte(v.Content)}, nil
	}
	return n.readFileRange(ctx, path+"&mode=download", p.Offset, p.Limit)
}

// readFileRange reads one segment through the node's download mode, whose
// http.ServeContent honours Range; download still refuses sensitive names.
func (n *HTTPClient) readFileRange(ctx context.Context, path string, offset int64, limit int) (FileRead, error) {
	if limit <= 0 || limit > MaxFileChunk {
		limit = MaxFileChunk
	}
	hdr := http.Header{"Range": {"bytes=" + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+int64(limit)-1, 10)}}
	resp, err := n.doRequestHdr(ctx, http.MethodGet, path, nil, hdr)
	if err != nil {
		return FileRead{}, fmt.Errorf("file from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	var size int64
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != offset {
			return FileRead{}, fmt.Errorf("file from %s: bad Content-Range %q", n.ID, resp.Header.Get("Content-Range"))
		}
		size = total
	case http.StatusOK:
		// An empty file ignores the Range; anything else must have honoured it.
		if offset != 0 || resp.ContentLength < 0 || resp.ContentLength > int64(limit) {
			return FileRead{}, fmt.Errorf("file from %s: range not honoured", n.ID)
		}
		size = resp.ContentLength
	default:
		return FileRead{FileRefusal: fileRefusalFrom(resp)}, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
	if err != nil {
		return FileRead{}, fmt.Errorf("read file from %s: %w", n.ID, err)
	}
	var mtime int64
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		mtime = t.UnixMilli()
	}
	return FileRead{Size: size, MtimeMs: mtime, Offset: offset, Data: data}, nil
}

// parseContentRange reads "bytes start-end/total".
func parseContentRange(v string) (start, total int64, ok bool) {
	rest, found := strings.CutPrefix(v, "bytes ")
	if !found {
		return 0, 0, false
	}
	rng, tot, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	s, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(s, 10, 64)
	total, err2 := strconv.ParseInt(tot, 10, 64)
	return start, total, err1 == nil && err2 == nil && start >= 0 && total >= 0
}

func (n *HTTPClient) FilesExist(ctx context.Context, p FilesExistsParams) (FilesExists, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return FilesExists{}, err
	}
	resp, err := n.doRequest(ctx, http.MethodPost, "/api/projects/files/exists", bytes.NewReader(data))
	if err != nil {
		return FilesExists{}, fmt.Errorf("files exists on %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return FilesExists{FileRefusal: fileRefusalFrom(resp)}, nil
	}
	var out FilesExists
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFileExistsBody)).Decode(&out); err != nil {
		return FilesExists{}, fmt.Errorf("decode files exists from %s: %w", n.ID, err)
	}
	return out, nil
}

func (n *HTTPClient) FetchOutputs(ctx context.Context, key string) (json.RawMessage, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/sessions/outputs?key="+url.QueryEscape(key), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch outputs from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch outputs from %s: status %d", n.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOutputsBody))
	if err != nil {
		return nil, fmt.Errorf("read outputs from %s: %w", n.ID, err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("fetch outputs from %s: malformed JSON body", n.ID)
	}
	return raw, nil
}
