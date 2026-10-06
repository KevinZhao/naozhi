package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"

	"github.com/slack-go/slack"
)

// subtypeFileShare is the message subtype Slack gives an upload, with or
// without a caption.
const subtypeFileShare = "file_share"

// slackFileHost is the only host a file download may target: the request
// carries the bot token, so a url_private_download pointing anywhere else is
// refused rather than fetched.
const slackFileHost = "files.slack.com"

// maxSlackImageBytes caps one inbound image, matching the other adapters.
const maxSlackImageBytes = 10 << 20

// slackFileDownloadTimeout bounds one download, body included: a file at the
// 32 MiB cap needs about 190 KB/s. Stop still cancels it through s.ctx.
const slackFileDownloadTimeout = 3 * time.Minute

// slackFileHTTPClient downloads uploads. It is separate from slackHTTPClient
// because that client's 10s Timeout also covers reading the body; it keeps
// the same CheckRedirect block so the bearer token cannot be redirected.
var slackFileHTTPClient = &http.Client{
	Timeout: slackFileDownloadTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var (
	errSlackFileTooLarge = errors.New("slack file exceeds the size cap")
	// errSlackFileHTMLPage is a 200 text/html body: Slack serves its sign-in
	// page instead of the file when the token lacks the files:read scope.
	errSlackFileHTMLPage = errors.New("slack served an HTML page instead of the file (missing files:read scope?)")
)

// isSlackImage reports a MIME type the model accepts as an inline image.
func isSlackImage(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// slackFileName is the upload's file name, or its title when Slack sent none.
func slackFileName(f slack.File) string {
	if f.Name != "" {
		return f.Name
	}
	return f.Title
}

// attachFiles downloads a file_share message's uploads under the per-message
// count and aggregate byte caps. Supported images go to msg.Images; other
// files go to msg.Files for dispatch to classify. Every upload that is not
// delivered becomes a File carrying its Reject reason, so the user is told.
func (s *Slack) attachFiles(ctx context.Context, msg *platform.IncomingMessage, files []slack.File) {
	total := 0
	for i, f := range files {
		name := slackFileName(f)
		reject := func(r platform.FileReject) {
			msg.Files = append(msg.Files, platform.File{Name: name, Reject: r})
		}
		if i >= limits.MaxFileAttachmentsPerMessage {
			reject(platform.FileRejectTooMany)
			continue
		}
		isImage := isSlackImage(f.Mimetype)
		perFile := limits.MaxFileAttachmentBytes
		if isImage {
			perFile = maxSlackImageBytes
		}
		remaining := limits.MaxFileAttachmentBytes - total
		if f.Size > perFile {
			reject(platform.FileRejectTooLarge)
			continue
		}
		if f.Size > remaining {
			reject(platform.FileRejectTotalTooLarge)
			continue
		}
		data, err := s.downloadFile(ctx, f, min(perFile, remaining))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("slack file download failed", "err", err, "file", f.ID, "channel", msg.ChatID)
			switch {
			case !errors.Is(err, errSlackFileTooLarge):
				reject(platform.FileRejectDownloadFailed)
			case perFile <= remaining:
				reject(platform.FileRejectTooLarge)
			default:
				reject(platform.FileRejectTotalTooLarge)
			}
			continue
		}
		total += len(data)
		if !isImage {
			msg.Files = append(msg.Files, platform.File{Name: name, Data: data})
			continue
		}
		// Slack's mimetype is the uploader's claim; the bytes decide.
		mime, _, _ := strings.Cut(http.DetectContentType(data), ";")
		if !isSlackImage(mime) {
			total -= len(data)
			reject(platform.FileRejectUnsupported)
			continue
		}
		msg.Images = append(msg.Images, platform.Image{Data: data, MimeType: mime})
	}
}

// downloadFile fetches f's private download URL with the bot token, reading
// at most maxBytes; a larger body is errSlackFileTooLarge.
func (s *Slack) downloadFile(ctx context.Context, f slack.File, maxBytes int) ([]byte, error) {
	raw := f.URLPrivateDownload
	if raw == "" {
		raw = f.URLPrivate
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse download URL: %w", err)
	}
	if u.Scheme != "https" || u.Host != slackFileHost || u.User != nil {
		return nil, fmt.Errorf("download URL not on https://%s: %s", slackFileHost,
			osutil.SanitizeForLog(u.Redacted(), 256))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.BotToken)
	resp, err := s.fileHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	if ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";"); strings.EqualFold(strings.TrimSpace(ct), "text/html") {
		return nil, errSlackFileHTMLPage
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, errSlackFileTooLarge
	}
	return data, nil
}
