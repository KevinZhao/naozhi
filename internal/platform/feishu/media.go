package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/naozhi/naozhi/internal/platform"
)

// media.go — images and audio: sending an image, downloading an inbound
// resource, sniffing audio magic bytes, and uploading. Extracted from feishu.go
// (J10 of #2548).

func (f *Feishu) sendImage(ctx context.Context, chatID string, img platform.Image) (string, error) {
	imageKey, err := f.uploadImage(ctx, img.Data, img.MimeType)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}

	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	// `content` must be stringified JSON, not a nested object.
	content, err := json.Marshal(struct {
		ImageKey string `json:"image_key"`
	}{ImageKey: imageKey})
	if err != nil {
		return "", fmt.Errorf("marshal content: %w", err)
	}
	reqBody, err := json.Marshal(struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}{ReceiveID: chatID, MsgType: "image", Content: string(content)})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	return f.postMessage(ctx, token, reqBody)
}

// DownloadImage downloads an image from a message via Feishu API.
func (f *Feishu) DownloadImage(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
	return f.downloadResource(ctx, messageID, fileKey, "image", maxImageDownloadBytes, "image/png")
}

// DownloadAudio downloads an audio file from a message via Feishu API.
func (f *Feishu) DownloadAudio(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
	return f.downloadResource(ctx, messageID, fileKey, "audio", maxAudioDownloadBytes, "audio/ogg")
}

// downloadResource downloads a message resource (image/audio) from the Feishu API.
func (f *Feishu) downloadResource(ctx context.Context, messageID, fileKey, resType string, maxBytes int64, defaultMIME string) ([]byte, string, error) {
	// math.MaxInt64 would overflow maxBytes+1 and degrade LimitReader to 0 bytes.
	if maxBytes <= 0 || maxBytes >= (1<<62) {
		return nil, "", fmt.Errorf("download %s: invalid maxBytes %d", resType, maxBytes)
	}
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/resources/"+url.PathEscape(fileKey)+"?type="+url.QueryEscape(resType), nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download %s: %w", resType, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// %q: upstream body reaches slog attrs; escape bidi/C1/newline.
		return nil, "", fmt.Errorf("download %s: status %d, body: %q", resType, resp.StatusCode, body)
	}

	// maxBytes+1 distinguishes "exactly at limit" from "silently truncated";
	// reject rather than deliver a truncated file.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read %s body: %w", resType, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("download %s: payload exceeds %d-byte limit", resType, maxBytes)
	}

	contentType := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = defaultMIME
	}

	// The Content-Type header is not authoritative; sniff the bytes. Go's
	// WHATWG sniffer reports OGG as `application/ogg`, hence the explicit
	// accept. Audio additionally passes the audioMagicOK allowlist so a
	// crafted payload cannot widen the ffmpeg/Whisper attack surface just by
	// looking audio/* to the sniffer.
	if len(data) > 0 {
		sniffed := http.DetectContentType(data)
		ok := true
		switch resType {
		case "image":
			ok = strings.HasPrefix(sniffed, "image/")
		case "audio":
			ok = (strings.HasPrefix(sniffed, "audio/") || sniffed == "application/ogg") && audioMagicOK(data)
		}
		if !ok {
			return nil, "", fmt.Errorf("download %s: mime mismatch (header=%s sniffed=%s)", resType, contentType, sniffed)
		}
	}
	return data, contentType, nil
}

// audioMagicOK reports whether data starts with a magic number of a format the
// transcribe pipeline handles: OGG, MP3 (ID3v2 or frame sync), WAV, MP4/M4A
// (allowlisted ftyp brands) or FLAC. AMR and raw AAC-ADTS are intentionally
// absent — Feishu voice is OGG/Opus or M4A; add new formats here explicitly
// rather than widening the sniffer fallback. Pure so tests can table-drive it.
func audioMagicOK(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	// OGG.
	if bytes.HasPrefix(data, []byte("OggS")) {
		return true
	}
	// ID3v2: require a known major version so an ASCII "ID3…" string cannot pass.
	if len(data) >= 5 && bytes.HasPrefix(data, []byte("ID3")) && data[3] >= 2 && data[3] <= 4 {
		return true
	}
	// Raw MP3 frame sync; the bare 0xFFE* variants are not used by Feishu voice.
	if data[0] == 0xFF {
		switch data[1] {
		case 0xF2, 0xF3, 0xFA, 0xFB:
			return true
		}
	}
	// WAV: RIFF + 4-byte size + "WAVE".
	if len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")) {
		return true
	}
	// ftyp box: 4-byte size | "ftyp" | brand. QuickTime / Flash brands are
	// rejected — spottier Whisper/ffmpeg compatibility, not on the Feishu surface.
	if len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) {
		switch string(data[8:12]) {
		case "M4A ", "mp4a", "isom", "mp42", "dash":
			return true
		}
	}
	// FLAC.
	if bytes.HasPrefix(data, []byte("fLaC")) {
		return true
	}
	return false
}

// replyError sends an error notice directly to the user on a short-lived ctx
// derived from stopCtx, because the caller's ctx is often already cancelled.
func (f *Feishu) uploadImage(ctx context.Context, data []byte, mimeType string) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	filename := "image" + platform.ImageExt(mimeType)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("image_type", "message"); err != nil {
		return "", fmt.Errorf("write image_type field: %w", err)
	}
	part, err := w.CreateFormFile("image", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write image data: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/images", &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}
	if result.Code != 0 {
		return "", &APIError{Code: result.Code, Msg: result.Msg, Op: "upload_image"}
	}
	return result.Data.ImageKey, nil
}

// EditMessage updates an existing card message via PATCH (all messages are cards).
