package session

import "github.com/naozhi/naozhi/internal/session/sessionview"

// MaxUserLabelBytes caps the operator-set session label; see
// sessionview.MaxUserLabelBytes.
const MaxUserLabelBytes = sessionview.MaxUserLabelBytes

// ValidateUserLabel trims and validates an operator-set label; see
// sessionview.ValidateUserLabel.
func ValidateUserLabel(s string) (string, error) { return sessionview.ValidateUserLabel(s) }
