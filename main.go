// bookhop ("Send Books to iPhone") copies audiobooks into the BookPlayer iOS
// app over USB.
//
//	bookhop                 start the web UI (default)
//	bookhop apps            list apps installed on the connected iPhone
//	bookhop send FILE...    send files/folders to BookPlayer
//	bookhop ls [FOLDER]     list BookPlayer's Documents folder
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/danielpaulus/go-ios/ios/golog"

	"bookhop/device"
)

func main() {
	flag.StringVar(&device.BookPlayerBundleID, "bundle", device.BookPlayerBundleID, "BookPlayer bundle ID (default: find the app named BookPlayer)")
	port := flag.Int("port", 0, "port for the web UI (default: random)")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser")
	verbose := flag.Bool("v", false, "verbose go-ios logging")
	flag.Parse()

	level := slog.LevelError + 1 // silence go-ios unless -v
	if *verbose {
		level = slog.LevelDebug
	}
	golog.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	var err error
	switch flag.Arg(0) {
	case "", "ui":
		err = runUI(*port, !*noBrowser)
	case "apps":
		err = cmdApps()
	case "send":
		err = cmdSend(flag.Args()[1:])
	case "ls":
		err = cmdLs(flag.Arg(1))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
