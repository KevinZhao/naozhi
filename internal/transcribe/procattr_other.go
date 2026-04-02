//go:build !linux

package transcribe

import "os/exec"

func setSysProcAttr(_ *exec.Cmd) {}
