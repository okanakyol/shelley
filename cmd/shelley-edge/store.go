package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// credential is the saved Sign in with ChatGPT state. HostID identifies this
// machine to OpenAI and outlives sign-outs; the rest comes from the last
// successful sign-in or refresh.
type credential struct {
	HostID   string    `json:"host_id"`
	ClientID string    `json:"client_id,omitempty"`
	Access   string    `json:"access_token,omitempty"`
	Refresh  string    `json:"refresh_token,omitempty"`
	Expires  time.Time `json:"expires,omitzero"`
}

func (c credential) signedIn() bool { return c.ClientID != "" && c.Refresh != "" }

type store interface {
	// load returns the zero credential when nothing is saved yet.
	load() (credential, error)
	save(credential) error
}

// keychain keeps the credential in the macOS login keychain as one generic
// password item.
type keychain struct{ service, account string }

func (k keychain) load() (credential, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", k.service, "-a", k.account, "-w").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 44 { // errSecItemNotFound
		return credential{}, nil
	}
	if err != nil {
		return credential{}, fmt.Errorf("read keychain item %s: %w", k.service, err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return credential{}, fmt.Errorf("decode keychain item %s: %w", k.service, err)
	}
	var c credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return credential{}, fmt.Errorf("decode keychain item %s: %w", k.service, err)
	}
	return c, nil
}

func (k keychain) save(c credential) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// security -i reads the command from stdin, so the secret never appears in
	// process arguments. Base64 needs no quoting.
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
		k.service, k.account, base64.StdEncoding.EncodeToString(raw)))
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Run(); err != nil || stderr.Len() > 0 {
		return fmt.Errorf("write keychain item %s: %v %s", k.service, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
