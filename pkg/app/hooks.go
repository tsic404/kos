package app

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/tsic404/kos/api/v1beta1"
)

// HookUpgrade is the hook a package declares for its upgrade step; a non-zero
// exit status fails the upgrade and rolls it back (C-32, C-34).
const HookUpgrade = "upgrade"

// DefaultHookTimeout bounds a hook that declares no timeout of its own (C-32).
const DefaultHookTimeout = 60 * time.Second

// RunHook runs one package hook and reports its outcome by exit status: zero
// succeeds and every other value fails (C-32). The hook runs under the round's
// context and its own timeout, so a hung hook cannot hold the reconciler.
func RunHook(ctx context.Context, hook v1beta1.Hook) error {
	if len(hook.Command) == 0 {
		return &InvalidPackage{Reason: fmt.Sprintf("hook %q carries no command", hook.Name)}
	}
	timeout := DefaultHookTimeout
	if hook.TimeoutSeconds > 0 {
		timeout = time.Duration(hook.TimeoutSeconds) * time.Second
	}
	hookContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(hookContext, hook.Command[0], hook.Command[1:]...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if detail := bytes.TrimSpace(stderr.Bytes()); len(detail) > 0 {
			return fmt.Errorf("hook %q failed: %w: %s", hook.Name, err, detail)
		}
		return fmt.Errorf("hook %q failed: %w", hook.Name, err)
	}
	return nil
}
