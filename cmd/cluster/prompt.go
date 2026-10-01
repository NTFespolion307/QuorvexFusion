package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// confirm asks a yes/no question on the terminal (default no).
func confirm(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// readPassword prompts without echo. With twice, it asks for confirmation.
func readPassword(prompt string, twice bool) (string, error) {
	if !stdinIsTerminal() {
		return "", errors.New("no terminal to prompt for a password; use --password-stdin")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if twice {
		fmt.Fprint(os.Stderr, "Repeat password: ")
		b2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(b) != string(b2) {
			return "", errors.New("passwords do not match")
		}
	}
	return string(b), nil
}

// readStdinLine reads a single line (e.g. a piped password).
func readStdinLine() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("nothing on stdin")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// fingerprintConfirmer returns the callback the worker uses to approve an
// unknown controller CA: auto-accept with --yes, ask on a terminal, or nil
// (refuse) when neither is possible.
func fingerprintConfirmer(yes bool) func(string) bool {
	if yes {
		return func(fp string) bool {
			fmt.Fprintf(os.Stderr, "WARNING: trusting controller CA %s without verification (--yes).\n"+
				"Pass --ca-fingerprint to protect against man-in-the-middle attacks.\n", fp)
			return true
		}
	}
	if !stdinIsTerminal() {
		return nil
	}
	return func(fp string) bool {
		fmt.Fprintf(os.Stderr, "\nController CA fingerprint:\n  %s\n"+
			"Compare it with the fingerprint shown by the controller (web UI > Join tokens,\n"+
			"or `cluster controller init` output).\n", fp)
		return confirm("Is this your controller?")
	}
}
