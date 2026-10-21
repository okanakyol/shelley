package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// secrets holds the edge's credentials, one value per account name.
type secrets interface {
	// get returns nil when nothing is saved under account.
	get(account string) ([]byte, error)
	set(account string, value []byte) error
}

// keychain keeps each secret in the macOS login keychain as a generic
// password item of one service.
type keychain struct{ service string }

func (k keychain) get(account string) ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", k.service, "-a", account, "-w").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 44 { // errSecItemNotFound
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read keychain item %s/%s: %w", k.service, account, err)
	}
	value, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, fmt.Errorf("decode keychain item %s/%s: %w", k.service, account, err)
	}
	return value, nil
}

func (k keychain) set(account string, value []byte) error {
	// security -i reads the command from stdin, so the secret never appears in
	// process arguments. Base64 needs no quoting.
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
		k.service, account, base64.StdEncoding.EncodeToString(value)))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil || out.Len() > 0 {
		return fmt.Errorf("write keychain item %s/%s: %v %s", k.service, account, err, strings.TrimSpace(out.String()))
	}
	return nil
}
