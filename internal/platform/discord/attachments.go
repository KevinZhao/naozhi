package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// maxDiscordImageBytes caps one inbound image, matching the other adapters.
const maxDiscordImageBytes = 10 << 20

var errDiscordTooLarge = errors.New("discord attachment exceeds the size cap")

// pendingAttachment is one attachment of an inbound message: either a
// download still to run, or a refusal already decided from its metadata.
type pendingAttachment struct {
	name   string
	url    string
	size   int
	image  bool
	reject platform.FileReject
}

// planAttachments decides from metadata alone what each attachment costs.
// Images and files ClassifyFile may accept are downloaded, at most
// limits.MaxFileAttachmentsPerMessage of them; the rest, and any whose
// declared size is over its cap, are refused without a download.
func planAttachments(atts []*discordgo.MessageAttachment) []pendingAttachment {
	var plan []pendingAttachment
	downloads := 0
	for _, att := range atts {
		if att == nil {
			continue
		}
		p := pendingAttachment{name: att.Filename, url: att.URL, size: att.Size,
			image: isImageContentType(att.ContentType)}
		perFile := limits.MaxFileAttachmentBytes
		if p.image {
			perFile = maxDiscordImageBytes
		}
		switch {
		case !p.image && !attachment.MaybeSupported(att.Filename, att.ContentType):
			p.reject = platform.FileRejectUnsupported
		case downloads >= limits.MaxFileAttachmentsPerMessage:
			p.reject = platform.FileRejectTooMany
		case p.size > perFile:
			p.reject = platform.FileRejectTooLarge
		default:
			downloads++
		}
		plan = append(plan, p)
	}
	return plan
}

// needsDownload reports whether any planned attachment is still to fetch.
func needsDownload(plan []pendingAttachment) bool {
	for _, p := range plan {
		if p.reject == platform.FileRejectNone {
			return true
		}
	}
	return false
}

// attachFiles downloads the planned attachments under the per-message
// aggregate byte cap shared by images and files. Images go to msg.Images
// once their bytes sniff as an image; files go to msg.Files for dispatch to
// classify. Every attachment not delivered becomes a File carrying its
// Reject reason, so the user is told. It reports false when ctx ended
// mid-way, and the message must then not be forwarded.
func (d *Discord) attachFiles(ctx context.Context, msg *platform.IncomingMessage, plan []pendingAttachment) bool {
	total := 0
	for _, p := range plan {
		reject := func(r platform.FileReject) {
			msg.Files = append(msg.Files, platform.File{Name: p.name, Reject: r})
		}
		if p.reject != platform.FileRejectNone {
			reject(p.reject)
			continue
		}
		perFile := limits.MaxFileAttachmentBytes
		if p.image {
			perFile = maxDiscordImageBytes
		}
		remaining := limits.MaxFileAttachmentBytes - total
		if p.size > remaining {
			reject(platform.FileRejectTotalTooLarge)
			continue
		}
		data, err := downloadURL(ctx, d.cdnHTTP, p.url, min(perFile, remaining))
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			slog.Warn("discord download attachment failed",
				"err", err, "url", osutil.SanitizeForLog(p.url, 256), "channel", msg.ChatID)
			switch {
			case !errors.Is(err, errDiscordTooLarge):
				reject(platform.FileRejectDownloadFailed)
			case perFile <= remaining:
				reject(platform.FileRejectTooLarge)
			default:
				reject(platform.FileRejectTotalTooLarge)
			}
			continue
		}
		if !p.image {
			total += len(data)
			msg.Files = append(msg.Files, platform.File{Name: p.name, Data: data})
			continue
		}
		mime, err := resolveImageContentType(data)
		if err != nil {
			slog.Warn("discord attachment is not an image", "err", err, "channel", msg.ChatID)
			reject(platform.FileRejectUnsupported)
			continue
		}
		total += len(data)
		msg.Images = append(msg.Images, platform.Image{Data: data, MimeType: mime})
	}
	return true
}

// downloadURL fetches a CDN attachment, reading at most maxBytes; a larger
// body is errDiscordTooLarge rather than a silently truncated file.
func downloadURL(ctx context.Context, client *http.Client, rawURL string, maxBytes int) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid attachment URL: %w", err)
	}
	// CDN URLs are always https; plaintext would let a MITM substitute bytes
	// that are then forwarded as a trusted attachment.
	if u.Scheme != "https" {
		return nil, fmt.Errorf("attachment URL must be https, got %q", u.Scheme)
	}
	if !discordCDNHosts[u.Hostname()] {
		return nil, fmt.Errorf("attachment URL host not in whitelist: %s", u.Hostname())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, errDiscordTooLarge
	}
	return data, nil
}
