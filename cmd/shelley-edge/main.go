// Command shelley-edge runs on the host and lets Shelley use a ChatGPT
// subscription without the credential ever reaching Shelley's machine. It
// plays the role of exe.dev's LLM integration: point Shelley at it with
//
//	{"llm_integrations": [{"name": "edge", "url": "http://127.0.0.1:8080"}]}
//
// in shelley.json.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
)

func main() {
	fs := flag.NewFlagSet("shelley-edge", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  shelley-edge login           sign in with ChatGPT\n  shelley-edge serve [-listen addr]\n\n")
		fs.PrintDefaults()
	}
	listen := fs.String("listen", "127.0.0.1:8080", "serve: address to listen on")
	if len(os.Args) < 2 {
		fs.Usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	chatgpt := newChatGPT(keychain{service: "shelley-edge", account: "chatgpt"})

	var err error
	switch cmd {
	case "login":
		err = login(ctx, chatgpt)
	case "serve":
		slog.Info("shelley-edge listening", "addr", *listen)
		err = serve(ctx, *listen, &edge{chatgpt: chatgpt})
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
