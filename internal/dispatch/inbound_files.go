package dispatch

import (
	"errors"
	"strconv"
	"strings"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

// Reasons shown to the user for a file that did not reach the model.
const (
	fileReasonUnsupported = "暂不支持该类型（支持 PDF 和 UTF-8 文本：txt/md/csv/json/log/yaml）"
	fileReasonEmpty       = "文件为空"
	fileReasonDownload    = "下载失败，请重新发送"
)

var (
	fileReasonTooMany  = "超过单条消息 " + strconv.Itoa(limits.MaxFileAttachmentsPerMessage) + " 个文件的上限"
	fileReasonTooLarge = "文件过大（上限 " + strconv.Itoa(limits.MaxFileAttachmentBytes>>20) + " MB）"
	fileReasonTotal    = "单条消息的文件合计超过 " + strconv.Itoa(limits.MaxFileAttachmentBytes>>20) + " MB"
)

// fileAttachments turns msg.Files into file_ref attachments whose bytes
// turnSender writes into the session workspace at send time (WorkspacePath
// stays empty until then). A file that is rejected by its adapter, by
// attachment.ClassifyFile, or by the per-message count and byte caps is left
// out and named in notice, a reply for the user; notice is "" when every
// file was accepted.
func fileAttachments(files []platform.File) (accepted []clievent.Attachment, notice string) {
	var rejected []string
	reject := func(name, reason string) {
		if name == "" {
			name = "未命名文件"
		}
		rejected = append(rejected, "- "+name+"："+reason)
	}
	total := 0
	for _, f := range files {
		name := attachment.SanitizeOrigName(f.Name)
		if reason := adapterRejectReason(f.Reject); reason != "" {
			reject(name, reason)
			continue
		}
		if len(accepted) >= limits.MaxFileAttachmentsPerMessage {
			reject(name, fileReasonTooMany)
			continue
		}
		mime, err := attachment.ClassifyFile(f.Name, f.Data)
		if err != nil {
			reject(name, classifyRejectReason(err))
			continue
		}
		if total+len(f.Data) > limits.MaxFileAttachmentBytes {
			reject(name, fileReasonTotal)
			continue
		}
		total += len(f.Data)
		accepted = append(accepted, clievent.Attachment{
			Kind:     clievent.KindFileRef,
			Data:     f.Data,
			MimeType: mime,
			OrigName: name,
			Size:     int64(len(f.Data)),
		})
	}
	if len(rejected) == 0 {
		return accepted, ""
	}
	// Adapters cap the files they forward; this bounds the reply regardless.
	const maxListed = limits.MaxFileAttachmentsPerMessage
	if extra := len(rejected) - maxListed; extra > 0 {
		rejected = append(rejected[:maxListed], "- 另有 "+strconv.Itoa(extra)+" 个文件未处理")
	}
	return accepted, "以下文件未处理：\n" + strings.Join(rejected, "\n")
}

// adapterRejectReason maps an adapter's FileReject to its user-facing reason;
// "" for FileRejectNone.
func adapterRejectReason(r platform.FileReject) string {
	switch r {
	case platform.FileRejectNone:
		return ""
	case platform.FileRejectTooLarge:
		return fileReasonTooLarge
	case platform.FileRejectDownloadFailed:
		return fileReasonDownload
	case platform.FileRejectTooMany:
		return fileReasonTooMany
	case platform.FileRejectTotalTooLarge:
		return fileReasonTotal
	default:
		return fileReasonUnsupported
	}
}

// classifyRejectReason maps an attachment.ClassifyFile error to its
// user-facing reason.
func classifyRejectReason(err error) string {
	switch {
	case errors.Is(err, attachment.ErrTooLarge):
		return fileReasonTooLarge
	case errors.Is(err, attachment.ErrEmptyData):
		return fileReasonEmpty
	default:
		return fileReasonUnsupported
	}
}
