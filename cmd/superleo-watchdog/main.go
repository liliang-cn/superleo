// Command superleo-watchdog watches a running SuperLeo from outside.
//
//	superleo-watchdog run        tick until stopped (what the LaunchAgent runs)
//	superleo-watchdog once       one tick, then exit
//	superleo-watchdog install    write ~/Library/LaunchAgents/cn.superleo.watchdog.plist and load it
//	superleo-watchdog uninstall  unload and remove it
//
// It reads ~/.superleo/watchdog.json (optional) and talks to the app only
// through its local API. Create ~/.superleo/watchdog.pause to stop it from
// reopening the app.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/liliang-cn/superleo/internal/watchdog"
)

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "run", "once":
		cfg, err := watchdog.LoadConfig(watchdog.DataDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		w, err := watchdog.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if cmd == "once" {
			w.Tick(ctx)
			return
		}
		w.Run(ctx)
	case "install", "uninstall":
		bin, err := os.Executable()
		if err == nil {
			bin, err = filepath.EvalSymlinks(bin)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		in, err := watchdog.NewInstaller(bin)
		if err == nil && cmd == "install" {
			err = in.Install()
		} else if err == nil {
			err = in.Uninstall()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(cmd, "ok")
	default:
		fmt.Fprintln(os.Stderr, "usage: superleo-watchdog run|once|install|uninstall")
		os.Exit(2)
	}
}
