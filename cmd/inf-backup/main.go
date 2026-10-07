package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"inf-backup/internal/config"
	"inf-backup/internal/console"
	"inf-backup/internal/protocol"
)

func run(ctx context.Context, args []string, in io.Reader, out io.Writer) int {
	flags := flag.NewFlagSet("inf-backup", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", config.DefaultPath, "configuration YAML path")
	version := flags.Bool("version", false, "print wrapper version and exit")
	flags.Usage = func() {
		fmt.Fprintln(out, "Usage: inf-backup [--config PATH | PATH] [--version]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *version {
		fmt.Fprintln(out, protocol.WrapperVersion)
		return 0
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return 2
	}
	if flags.NArg() == 1 {
		*path = flags.Arg(0)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(out, "[!] 配置错误:", err)
		return 1
	}
	c, err := console.New(ctx, cfg, out, nil)
	if err != nil {
		fmt.Fprintln(out, "[!] 状态错误:", cfg.Redact(err.Error()))
		return 1
	}
	defer c.Shutdown()
	c.Banner()
	lines := make(chan string)
	readErrors := make(chan error, 1)
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-readCtx.Done():
				return
			}
		}
		readErrors <- scanner.Err()
	}()
	for {
		select {
		case <-ctx.Done():
			return 130
		case line, ok := <-lines:
			if !ok {
				if err := <-readErrors; err != nil {
					fmt.Fprintln(out, "[!] 无法读取控制台输入")
					return 1
				}
				return 0
			}
			if c.Handle(line) {
				return 0
			}
		}
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout)
	cancel()
	os.Exit(code)
}
