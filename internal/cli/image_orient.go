package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Auto-orientation for images with NO EXIF orientation flag: a small vision
// model is asked which EDGE holds the top of the text and RotateJPEG bakes the
// matching rotation into the pixels. The prompt deliberately asks for an edge,
// not degrees — small models flip the direction when asked for degrees
// (answered 90 where 270 was correct); the edge→degrees mapping lives in code.

// orientUserPrompt is the instruction sent with the image. Output is
// hard-constrained to a 4-value enum; anything else is treated as "unknown"
// and the image is left untouched (fail-safe).
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

// edgeToDegreesCW maps the model's edge answer to the clockwise rotation that
// makes the text upright (text-top pointing RIGHT needs 270° CW == 90° CCW).
var edgeToDegreesCW = map[string]int{
	"up":    0,
	"left":  90,
	"down":  180,
	"right": 270,
}

// BuildOrientMessage constructs the stream-json NDJSON line (inline base64
// image block + instruction text) piped to `claude -p --input-format
// stream-json`, using the same wire shape as a normal session message.
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

// ParseOrientStreamJSON extracts the final text answer from the stream-json
// stdout of a `claude -p --output-format stream-json --verbose` run and maps
// it to an OrientVerdict. Fail-safe: on ANY ambiguity (no result line,
// multi-word or unrecognised output) it returns ({up,0}, false) so the caller
// leaves the image untouched; ok=true only for a confident non-"up" edge.
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
			// Error subtypes (e.g. "error_max_turns") leave .result empty.
			if ev.Result != "" {
				answer = ev.Result
			}
		case "assistant":
			// Fallback when the result line is absent (older CLI builds).
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
// to degrees; split out so it can be unit-tested against adversarial strings.
func classifyOrientAnswer(answer string) (OrientVerdict, bool) {
	// Trim whitespace and chatty punctuation ("up." / "**left**"). Multi-word
	// answers fail safe rather than substring-matching "up" out of a sentence.
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
		return OrientVerdict{Edge: "up", DegreesCW: 0}, false
	}
	return OrientVerdict{Edge: norm, DegreesCW: deg}, true
}
