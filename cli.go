package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"bookhop/device"
)

func cmdApps() error {
	d, err := device.First()
	if err != nil {
		return err
	}
	apps, err := device.ListApps(d)
	if err != nil {
		return err
	}
	fmt.Printf("%-40s %-50s %-10s %s\n", "NAME", "BUNDLE ID", "VERSION", "FILE SHARING")
	for _, a := range apps {
		share := ""
		if a.FileSharing {
			share = "yes"
		}
		fmt.Printf("%-40s %-50s %-10s %s\n", a.Name, a.BundleID, a.Version, share)
	}
	return nil
}

func openBookPlayer() (*device.Session, error) {
	d, err := device.First()
	if err != nil {
		return nil, err
	}
	bundle, err := device.FindBookPlayer(d)
	if err != nil {
		return nil, err
	}
	fmt.Println("BookPlayer bundle ID:", bundle)
	return device.Open(d, bundle)
}

func cmdLs(rel string) error {
	s, err := openBookPlayer()
	if err != nil {
		return err
	}
	defer s.Close()
	names, err := s.List(rel)
	if err != nil {
		return err
	}
	for _, n := range names {
		if n != "." && n != ".." {
			fmt.Println(n)
		}
	}
	return nil
}

// sendItem is one local file and where it goes under Documents.
type sendItem struct {
	local, rel string
	size       int64
}

// collect expands files and folders into the list of files to send. Folders
// keep their structure, rooted at the folder's own name.
func collect(args []string) ([]sendItem, error) {
	var items []sendItem
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if device.Allowed(arg) {
				items = append(items, sendItem{arg, filepath.Base(arg), info.Size()})
			}
			continue
		}
		root := filepath.Clean(arg)
		parent := filepath.Dir(root)
		err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !device.Allowed(p) {
				return nil
			}
			fi, err := e.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(parent, p)
			items = append(items, sendItem{p, filepath.ToSlash(rel), fi.Size()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func cmdSend(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bookhop send FILE_OR_FOLDER...")
	}
	items, err := collect(args)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("no audiobook files found (.mp3, .m4a, .m4b, .zip)")
	}
	s, err := openBookPlayer()
	if err != nil {
		return err
	}
	defer s.Close()

	for i, it := range items {
		start := time.Now()
		fmt.Printf("[%d/%d] %s (%.1f MB) ... ", i+1, len(items), it.rel, float64(it.size)/1e6)
		f, err := os.Open(it.local)
		if err != nil {
			return err
		}
		err = s.Write(it.rel, f)
		f.Close()
		if err != nil {
			fmt.Println("failed")
			return err
		}
		secs := time.Since(start).Seconds()
		fmt.Printf("done in %.1fs (%.1f MB/s)\n", secs, float64(it.size)/1e6/max(secs, 0.001))
	}
	fmt.Println("Done! Open BookPlayer on your iPhone.")
	return nil
}

