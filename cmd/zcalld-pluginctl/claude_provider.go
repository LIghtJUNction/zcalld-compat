package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/claudemaintenance"
	"github.com/LIghtJUNction/zcalld-compat/claudeprovider"
)

func runClaudeProviderWorker(root string) error {
	return claudeprovider.Serve(root, os.Stdin, os.Stdout)
}

func runClaudeProviderBinding(root, file, expected string, args []string) (any, error) {
	if len(args) == 1 && args[0] == "native-config" {
		if file != "" || expected != "" {
			return nil, errors.New("claude-provider native-config is read-only and takes no binding options")
		}
		return claudemaintenance.NativeProviderConfiguration(root)
	}
	if len(args) != 2 || args[0] != "bind" || (args[1] != "plan" && args[1] != "apply") || file == "" {
		return nil, errors.New("claude-provider expects bind plan|apply --file ABSOLUTE_JSON [--expect-plan HASH]")
	}
	if args[1] == "plan" && expected != "" {
		return nil, errors.New("claude-provider bind plan takes no --expect-plan")
	}
	if args[1] == "apply" && expected == "" {
		return nil, errors.New("claude-provider bind apply requires --expect-plan")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return claudemaintenance.NativeBindingPlan(ctx, root, file, expected, args[1] == "apply")
}
