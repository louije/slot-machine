package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

const releaseURL = "https://api.github.com/repos/louije/slot-machine/releases/latest"

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func cmdUpdate() {
	resp, err := http.Get(releaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot reach GitHub: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		fmt.Fprintln(os.Stderr, "error: no releases found")
		os.Exit(1)
	}
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "error: GitHub API returned %d\n", resp.StatusCode)
		os.Exit(1)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot parse release: %v\n", err)
		os.Exit(1)
	}

	if rel.TagName == Version {
		fmt.Printf("already up to date (%s)\n", Version)
		return
	}

	wantName := fmt.Sprintf("slot-machine-%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, a := range rel.Assets {
		if a.Name == wantName {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		fmt.Fprintf(os.Stderr, "error: no asset %q in release %s\n", wantName, rel.TagName)
		os.Exit(1)
	}

	// Download to temp file next to current binary.
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine own path: %v\n", err)
		os.Exit(1)
	}
	self, _ = filepath.EvalSymlinks(self)

	dlResp, err := http.Get(downloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: download failed: %v\n", err)
		os.Exit(1)
	}
	defer dlResp.Body.Close()

	tmp := self + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot write %s: %v\n", tmp, err)
		os.Exit(1)
	}
	if _, err := io.Copy(f, dlResp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error: download failed: %v\n", err)
		os.Exit(1)
	}
	f.Close()

	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error: cannot replace binary: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%s → %s\n", Version, rel.TagName)
}
