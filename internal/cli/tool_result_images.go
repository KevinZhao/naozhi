package cli

// tool_result_images.go — the images a tool returns (a screenshot, a Read of
// an image file) reach the event stream: ClaudeProtocol pulls them out of the
// user frame, the process stores each original under the state root, and
// EventEntriesFromEventAt turns them into a thumbnail tool_result entry
// (docs/rfc/computer-use.md §4.2).

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/datadir"
)

// toolImageGate is what a user frame carrying an image block contains; every
// other user frame skips the second decode. The CLI writes compact JSON.
const toolImageGate = `"type":"image"`

// wireToolResultFrame is the part of a user frame parseToolResultImages
// reads. A tool_result's content stays raw: it is a plain string for a
// text-only result.
type wireToolResultFrame struct {
	Message struct {
		Content []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

type wireToolResultPart struct {
	Type   string                `json:"type"`
	Text   string                `json:"text"`
	Source *clievent.ImageSource `json:"source"`
}

// parseToolResultImages returns the tool_result blocks of a user frame that
// carry at least one base64 image, nil when none does.
func parseToolResultImages(line string) []clievent.ToolResultImages {
	var f wireToolResultFrame
	if json.Unmarshal(stringToBytesUnsafe(line), &f) != nil {
		return nil
	}
	var out []clievent.ToolResultImages
	for _, b := range f.Message.Content {
		if b.Type != "tool_result" {
			continue
		}
		var parts []wireToolResultPart
		if json.Unmarshal(b.Content, &parts) != nil {
			continue
		}
		r := clievent.ToolResultImages{ToolUseID: b.ToolUseID}
		var texts []string
		for _, p := range parts {
			switch {
			case p.Type == "text" && p.Text != "":
				texts = append(texts, p.Text)
			case p.Type == "image" && p.Source != nil && p.Source.Type == "base64":
				data, err := base64.StdEncoding.DecodeString(p.Source.Data)
				if err != nil || len(data) == 0 {
					continue
				}
				r.Images = append(r.Images, clievent.ToolImage{MimeType: p.Source.MediaType, Data: data})
			}
		}
		if len(r.Images) == 0 {
			continue
		}
		r.Text = strings.Join(texts, "\n")
		out = append(out, r)
	}
	return out
}

// storeToolImages writes each image's original under the state root's
// planner/attachments and records its ImagePaths value. Best-effort, like the
// upload path's inline-image persist: a failure only costs the lightbox its
// full-size view, the thumbnail still shows.
func (p *Process) storeToolImages(results []clievent.ToolResultImages) {
	if len(results) == 0 {
		return
	}
	dir := datadir.FromRoot(p.stateDir).PlannerAttachmentsRoot()
	if dir == "" {
		return
	}
	for i := range results {
		for j := range results[i].Images {
			img := &results[i].Images[j]
			ext := toolImageExt(img.MimeType)
			if ext == "" {
				continue
			}
			stored, err := attachment.PersistIn(dir, img.Data, ext, attachment.Meta{MimeType: img.MimeType})
			if err != nil {
				p.slogger().Debug("tool image persist failed", "err", err)
				continue
			}
			img.Path = datadir.PlannerAttachmentsRel + "/" + stored.RelPath
		}
	}
}

// toolImageExt is the stored file's extension for an image MIME type, ""
// for one the attachment handler would not serve.
func toolImageExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}
