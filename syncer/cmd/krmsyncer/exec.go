// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Helper formatting functions.
func logInfo(format string, a ...any) {
	fmt.Printf("\033[1;34m[INFO]\033[0m "+format+"\n", a...)
}

func logSuccess(format string, a ...any) {
	fmt.Printf("\033[1;32m[SUCCESS]\033[0m "+format+"\n", a...)
}

func logWarn(format string, a ...any) {
	fmt.Printf("\033[1;33m[WARNING]\033[0m "+format+"\n", a...)
}

func logError(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;31m[ERROR]\033[0m "+format+"\n", a...)
}

func logHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// checkDeps verifies that the external tools the CLI shells out to are installed.
func checkDeps(deps ...string) error {
	for _, dep := range deps {
		if _, err := exec.LookPath(dep); err != nil {
			return fmt.Errorf("dependency %q is required but not found; please install it", dep)
		}
	}
	return nil
}

// run executes a command, streaming its stdout/stderr to the terminal.
func run(ctx context.Context, name string, args ...string) error {
	return runWithStdin(ctx, "", name, args...)
}

// runWithStdin executes a command with the given stdin, streaming its stdout/stderr to the terminal.
func runWithStdin(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// output executes a command and returns its trimmed stdout. Stderr is captured
// and included in the returned error.
func output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// quiet executes a command, discarding its output. Stderr is included in the returned error.
func quiet(ctx context.Context, name string, args ...string) error {
	_, err := output(ctx, name, args...)
	return err
}
