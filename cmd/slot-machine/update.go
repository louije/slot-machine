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
	Name string `json:"name"`
	URL  string `json:"url"` // API URL — serves binary with Accept: application/octet-stream
}

func cmdUpdate() {
	req, _ := http.NewRequest("GET", releaseURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "slot-machine/"+Version)
	resp, err := http.DefaultClient.Do(req)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading response: %v\n", err)
		os.Exit(1)
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot parse release: %v\n", err)
		os.Exit(1)
	}

	if rel.TagName == Version {
		fmt.Printf("already up to date (%s)\n", Version)
		return
	}

	wantName := fmt.Sprintf("slot-machine-%s-%s", runtime.GOOS, runtime.GOARCH)
	var assetURL string
	for _, a := range rel.Assets {
		if a.Name == wantName {
			assetURL = a.URL
			break
		}
	}
	if assetURL == "" {
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

	dlReq, _ := http.NewRequest("GET", assetURL, nil)
	dlReq.Header.Set("Accept", "application/octet-stream")
	dlReq.Header.Set("User-Agent", "slot-machine/"+Version)
	dlResp, err := http.DefaultClient.Do(dlReq)
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
