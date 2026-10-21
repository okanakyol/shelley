// Command shelley-edge runs on the host and lets Shelley use subscriptions
// without their credentials ever reaching Shelley's machine. It plays the
// role of exe.dev's LLM integrations, one per account: point Shelley at it
// with
//
//	{"llm_integrations": [
//	  {"name": "chatgpt", "url": "http://127.0.0.1:8080/chatgpt"},
//	  {"name": "opencode", "url": "http://127.0.0.1:8080/opencode"}
//	]}
//
// in shelley.json.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

func main() {
	fs := flag.NewFlagSet("shelley-edge", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n"+
			"  shelley-edge login               sign in with ChatGPT\n"+
			"  shelley-edge set-key opencode    store an OpenCode API key (read from stdin)\n"+
			"  shelley-edge serve [flags]       serve the integrations\n\n")
		fs.PrintDefaults()
	}
	listen := fs.String("listen", "127.0.0.1:8080", "serve: address to listen on")
	opencodeURL := fs.String("opencode-url", defaultOpenCodeURL, "serve: OpenCode API base (https://opencode.ai/zen/v1 for Zen)")
	if len(os.Args) < 2 {
		fs.Usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	store := keychain{service: "shelley-edge"}
	config, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shelley-edge:", err)
		os.Exit(1)
	}
	chatgpt := newChatGPT(store, fileLock{path: filepath.Join(config, "shelley-edge", "chatgpt.lock")})

	switch {
	case cmd == "login":
		err = login(ctx, chatgpt)
	case cmd == "set-key" && fs.Arg(0) == "opencode":
		err = setKey(store, "opencode", "OpenCode API key")
	case cmd == "serve":
		var hosts map[string]bool
		if hosts, err = allowedHosts(*listen); err != nil {
			break
		}
		slog.Info("shelley-edge listening", "addr", *listen, "opencode", *opencodeURL)
		err = serve(ctx, *listen, &edge{
			chatgpt:  chatgpt,
			opencode: &opencode{apiURL: strings.TrimSuffix(*opencodeURL, "/"), httpc: http.DefaultClient, secrets: store},
			hosts:    hosts,
		})
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shelley-edge:", err)
		os.Exit(1)
	}
}

func login(ctx context.Context, c *chatgpt) error {
	// The redirect must be 127.0.0.1 (never localhost); 1455 is the port
	// OpenAI's examples use.
	ln, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		return fmt.Errorf("listen for the sign-in callback: %w", err)
	}
	err = c.login(ctx, ln, func(u string) {
		fmt.Printf("Open this URL in your browser and sign in with ChatGPT:\n\n%s\n\n", u)
	})
	if err == nil {
		fmt.Println("Signed in. Start the proxy with: shelley-edge serve")
	}
	return err
}

// setKey reads a key from stdin, without echo on a terminal.
func setKey(s secrets, account, label string) error {
	var key string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "%s: ", label)
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		key = string(raw)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read %s from stdin: %w", label, err)
		}
		key = line
	}
	if key = strings.TrimSpace(key); key == "" {
		return fmt.Errorf("empty %s", label)
	}
	if err := s.set(account, []byte(key)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Saved the %s in the keychain.\n", label)
	return nil
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
