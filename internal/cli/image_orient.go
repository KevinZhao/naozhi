package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Auto-orientation for images that carry NO EXIF orientation flag.
//
// A sideways document photo whose pixels are physically rotated (with no
// metadata to signal it) can't be fixed by the browser's
// createImageBitmap(from-image) path. Instead we ask a small vision model
// (Haiku) which edge of the image holds the TOP of the text, then bake the
// matching clockwise rotation into the pixels with RotateJPEG.
//
// IMPORTANT prompt-design note (validated empirically): asking the model
// "how many degrees clockwise to rotate" makes a small model flip the
// direction (it answered 90 where 270 was correct). Asking instead which
// EDGE the top of the text sits on ("up"/"down"/"left"/"right") is stable
// and correct across repeated runs. We do the edge→degrees mapping here in
// code, never in the model.

// orientSystemReminder is appended to the user text. The model output is
// hard-constrained to a 4-value enum; anything else is treated as "unknown"
// and the image is left untouched (fail-safe). The instruction is phrased
// as physical edges, NOT rotation degrees — see the note above.
const orientUserPrompt = `The attached image may be a photo or scan of a document/text page that was captured sideways or upside down. Look ONLY at the orientation of the text/printed lines.

Answer with EXACTLY ONE lowercase word and nothing else — no punctuation, no explanation:
- "up"    if the text is already upright and reads normally
- "left"  if the top of the text points to the LEFT edge (page rotated, you'd turn your head left to read)
- "right" if the top of the text points to the RIGHT edge
- "down"  if the text is upside down

If you are not confident or the image has no readable text, answer "up".`

// OrientVerdict is the parsed, validated result of an orientation query.
type OrientVerdict struct {
	// Edge is one of "up","left","right","down". Always set on a successful
	// parse; the zero value "" means the model output didn't conform.
	Edge string
	// DegreesCW is the clockwise rotation to apply to make the text upright:
	// up->0, left->90, down->180, right->270. (If the top of the text points
	// LEFT, rotating the image 90° clockwise brings that top to the top.)
	DegreesCW int
}

// edgeToDegreesCW maps the model's edge answer to the clockwise rotation
// that makes the text upright. Derived geometrically and confirmed by
// rotating a real sideways scan: a page whose text-top points RIGHT needs a
// 270° CW rotation (== 90° CCW) to stand upright.
var edgeToDegreesCW = map[string]int{
	"up":    0,
	"left":  90,
	"down":  180,
	"right": 270,
}

// BuildOrientMessage constructs the stream-json NDJSON line (one user
// message with an inline base64 image block + the instruction text) that is
// piped to `claude -p --input-format stream-json`. Reuses the same
// inputImageBlock/imageSource wire shape as a normal session message so the
// CLI sees a familiar multimodal turn.
func BuildOrientMessage(jpeg []byte, mimeType string) ([]byte, error) {
	if len(jpeg) == 0 {
		return nil, fmt.Errorf("orient: empty image")
	}
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	content := []any{
		inputImageBlock{
			Type: "image",
			Source: imageSource{
				Type:      "base64",
				MediaType: mimeType,
				Data:      base64.StdEncoding.EncodeToString(jpeg),
			},
		},
		map[string]any{"type": "text", "text": orientUserPrompt},
	}
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("orient: marshal message: %w", err)
	}
	return append(line, '\n'), nil
}

// ParseOrientStreamJSON scans the stream-json (NDJSON) stdout of a
// `claude -p --output-format stream-json --verbose` run and extracts the
// final text answer, then validates + maps it to an OrientVerdict.
//
// Fail-safe contract: on ANY ambiguity — no result line, multi-word output,
// an unrecognised word, mixed content — it returns ("up"→0°-equivalent)
// with ok=false so the caller leaves the image untouched. ok=true is
// returned ONLY when the model emitted exactly one of the four enum words
// AND it isn't "up" (a confident, actionable rotation). "up" returns
// (verdict{up,0}, false) because there's nothing to do — callers treat
// ok=false uniformly as "don't rotate".
func ParseOrientStreamJSON(stdout []byte) (OrientVerdict, bool) {
	var answer string
	for _, raw := range bytes.Split(stdout, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Result  string `json:"result"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "result":
			// The terminal result event carries the final text in .result.
			// An error subtype (e.g. "error_max_turns") leaves it empty.
			if ev.Result != "" {
				answer = ev.Result
			}
		case "assistant":
			// Fallback: pull text from the last assistant message in case
			// the result line is absent (older CLI builds).
			for _, b := range ev.Message.Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					answer = b.Text
				}
			}
		}
	}
	return classifyOrientAnswer(answer)
}

// classifyOrientAnswer normalises a raw model answer to the enum and maps it
// to degrees. Split out from the NDJSON scan so it can be unit-tested
// directly against adversarial strings.
func classifyOrientAnswer(answer string) (OrientVerdict, bool) {
	// Normalise: lowercase, trim surrounding whitespace and the punctuation a
	// chatty model might add ("up." / "**left**" / `"right"`). We do NOT
	// accept multi-word answers — a sentence means the model ignored the
	// format, so we fail safe rather than substring-match "up" out of
	// "I think it's up but...".
	norm := strings.ToLower(strings.TrimSpace(answer))
	norm = strings.Trim(norm, ".,;:!?\"'*`()[]{} \t\r\n")
	if norm == "" || strings.ContainsAny(norm, " \t\r\n") {
		return OrientVerdict{Edge: "up", DegreesCW: 0}, false
	}
	deg, known := edgeToDegreesCW[norm]
	if !known {
		return OrientVerdict{Edge: "up", DegreesCW: 0}, false
	}
	if deg == 0 {
		// "up": valid but no rotation needed — report not-actionable.
		return OrientVerdict{Edge: "up", DegreesCW: 0}, false
	}
	return OrientVerdict{Edge: norm, DegreesCW: deg}, true
}
