package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"

	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/provider"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		version := "dev"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		_, err := fmt.Fprintln(out, "ada", version)
		return err
	}
	if len(args) == 0 || args[0] != "chat" {
		return fmt.Errorf("usage: ada chat [-config FILE] | ada version")
	}
	flags := flag.NewFlagSet("chat", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", "", "settings file")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: ada chat [-config FILE]")
	}
	settings, err := config.Load(*path)
	if err != nil {
		return err
	}
	conversation, err := config.Build(settings, os.Getenv)
	if err != nil {
		return err
	}
	defer conversation.Tools.Close()

	var history []provider.Message
	input := bufio.NewScanner(in)
	input.Buffer(make([]byte, 64<<10), 4<<20)
	for input.Scan() {
		turn, stop := signal.NotifyContext(ctx, os.Interrupt)
		answer, next, err := conversation.Turn(turn, history, input.Text())
		interrupted := turn.Err() == context.Canceled
		stop()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if interrupted && errors.Is(err, context.Canceled) {
				fmt.Fprintln(diagnostics, "Turn canceled.")
			} else {
				fmt.Fprintln(diagnostics, err)
			}
			continue
		default:
			history = next
		}
		if _, err := fmt.Fprintln(out, answer); err != nil {
			return fmt.Errorf("print answer: %w", err)
		}
	}
	if err := input.Err(); err != nil {
		return fmt.Errorf("read chat input: %w", err)
	}
	return nil
}
