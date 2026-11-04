package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// CapFiles is the node tag for serving its own files to the primary: the
// files_list / file_read / files_exists / fetch_outputs RPCs (planner RFC
// §15.5 ④). A direct node serves them over its own HTTP file API.
const CapFiles = "files"

// MaxFileChunk caps one raw segment a primary reads from a node, so a raw
// read or a download goes in offset segments of at most this size; it also
// bounds a preview, whose cap is 1 MB.
const MaxFileChunk = 1 << 20

// ReverseFileChunk caps the bytes one file_read answer carries over a reverse
// conn, raw or preview. The node writes every frame under one lock with a
// 10 s deadline: a full MaxFileChunk (~1.4 MB as base64) would time out on a
// slow uplink and drop the conn, and on a fast one still hold up the event
// stream behind it.
const ReverseFileChunk = 256 << 10

// ErrFilesUnsupported is the error a file request to a node that cannot serve
// its files gets (a reverse node predating CapFiles); the dashboard answers 501.
var ErrFilesUnsupported = errors.New("node does not support remote files")

// FileRoot names the tree a file request reads: a session's working tree
// (Key) or a registered project (Project), both as the node knows them.
type FileRoot struct {
	Key     string `json:"key,omitempty"`
	Project string `json:"project,omitempty"`
}

// FileRefusal is a file request the node refused, as the HTTP status and
// message the dashboard answers with; a zero Status means it was served.
type FileRefusal struct {
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// FilesListParams is files_list's request: the children of Dir ("" is the
// root), dotfiles and noise directories only with ShowHidden.
type FilesListParams struct {
	FileRoot
	Dir        string `json:"dir,omitempty"`
	ShowHidden bool   `json:"show_hidden,omitempty"`
}

// FileEntry is one child in a directory listing. Absolute paths are never
// serialised; the dashboard joins the name onto its breadcrumb dir.
type FileEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtime_ms"`
	// Symlink children are surfaced but flagged so the UI renders them
	// non-navigable: the read and write paths refuse final-component symlinks.
	Symlink bool `json:"symlink,omitempty"`
}

// FileList is a directory listing, the body of GET /api/projects/files/list.
type FileList struct {
	Dir       string      `json:"dir"`
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
	// RootName names the tree a session root resolved to.
	RootName string `json:"root_name,omitempty"`
	FileRefusal
}

// FileReadParams is file_read's request: up to Limit bytes (at most
// ReverseFileChunk) from Offset of the text preview (Mode "preview", capped
// like the dashboard's) or of the file (Mode "raw").
type FileReadParams struct {
	FileRoot
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// FileRead is file_read's answer. Size and MtimeMs describe the whole file
// so a segmented read can tell it changed between segments.
type FileRead struct {
	Size    int64 `json:"size"`
	MtimeMs int64 `json:"mtime_ms"`
	// Mime, Truncated and Binary are the preview's.
	Mime      string `json:"mime,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	// Data is the preview's bytes (not yet UTF-8 repaired) or the raw
	// segment at Offset.
	Offset int64  `json:"offset,omitempty"`
	Data   []byte `json:"data,omitempty"`
	FileRefusal
}

// FilesExistsParams is files_exists' request: paths under a project.
type FilesExistsParams struct {
	Project string   `json:"project"`
	Paths   []string `json:"paths"`
}

// FileStat is one files_exists result.
type FileStat struct {
	Exists bool   `json:"exists"`
	Size   int64  `json:"size,omitempty"`
	Mime   string `json:"mime,omitempty"`
	IsDir  bool   `json:"is_dir,omitempty"`
}

// FilesExists is files_exists' answer, the body of POST
// /api/projects/files/exists.
type FilesExists struct {
	Results map[string]FileStat `json:"results"`
	FileRefusal
}

// FileBrowser is a node conn that can serve the node's files. Whether this
// node can is Meta().HasCap(CapFiles); asked anyway, a method answers
// ErrFilesUnsupported.
type FileBrowser interface {
	ListFiles(ctx context.Context, p FilesListParams) (FileList, error)
	ReadFile(ctx context.Context, p FileReadParams) (FileRead, error)
	FilesExist(ctx context.Context, p FilesExistsParams) (FilesExists, error)
	// FetchOutputs returns the node's GET /api/sessions/outputs?key= body.
	FetchOutputs(ctx context.Context, key string) (json.RawMessage, error)
}

// fileRPC runs one file RPC on a node advertising CapFiles; an older node's
// "unknown method" reads as ErrFilesUnsupported too.
func (c *ReverseConn) fileRPC(ctx context.Context, method string, params, out any) error {
	if !c.meta.HasCap(CapFiles) {
		return ErrFilesUnsupported
	}
	raw, err := c.rpc(ctx, method, params)
	if err != nil {
		if strings.Contains(err.Error(), "unknown method") {
			return ErrFilesUnsupported
		}
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *ReverseConn) ListFiles(ctx context.Context, p FilesListParams) (FileList, error) {
	var out FileList
	err := c.fileRPC(ctx, "files_list", p, &out)
	return out, err
}

// ReadFile reads a raw segment of at most ReverseFileChunk, or the whole
// preview in segments of that size; a short segment ends the preview.
func (c *ReverseConn) ReadFile(ctx context.Context, p FileReadParams) (FileRead, error) {
	if p.Mode != "preview" {
		if p.Limit <= 0 || p.Limit > ReverseFileChunk {
			p.Limit = ReverseFileChunk
		}
		var out FileRead
		err := c.fileRPC(ctx, "file_read", p, &out)
		return out, err
	}
	p.Offset, p.Limit = 0, ReverseFileChunk
	var out FileRead
	for {
		var seg FileRead
		if err := c.fileRPC(ctx, "file_read", p, &seg); err != nil {
			return FileRead{}, err
		}
		if p.Offset == 0 {
			out = seg
		} else if seg.Status != 0 || seg.Binary || seg.Size != out.Size || seg.MtimeMs != out.MtimeMs || seg.Offset != p.Offset {
			return FileRead{}, errors.New("file changed during preview")
		} else {
			out.Data = append(out.Data, seg.Data...)
		}
		if out.Status != 0 || out.Binary || len(seg.Data) < p.Limit || len(out.Data) >= MaxFileChunk {
			return out, nil
		}
		p.Offset += int64(len(seg.Data))
	}
}

func (c *ReverseConn) FilesExist(ctx context.Context, p FilesExistsParams) (FilesExists, error) {
	var out FilesExists
	err := c.fileRPC(ctx, "files_exists", p, &out)
	return out, err
}

func (c *ReverseConn) FetchOutputs(ctx context.Context, key string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.fileRPC(ctx, "fetch_outputs", map[string]string{"key": key}, &out)
	return out, err
}
