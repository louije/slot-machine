package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	var assetURL, checksumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case wantName:
			assetURL = a.URL
		case "checksums.txt":
			checksumURL = a.URL
		}
	}
	if assetURL == "" {
		fmt.Fprintf(os.Stderr, "error: no asset %q in release %s\n", wantName, rel.TagName)
		os.Exit(1)
	}

	// Refuse to self-replace without a checksum to check against. This binary
	// supervises deploys; silently installing an unverified replacement is not a
	// tradeoff worth making for convenience.
	if checksumURL == "" {
		fmt.Fprintf(os.Stderr, "error: release %s has no checksums.txt, refusing to update\n", rel.TagName)
		fmt.Fprintln(os.Stderr, "download the binary manually if you are sure")
		os.Exit(1)
	}

	wantSum, err := fetchChecksum(checksumURL, wantName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
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
	// Hash while writing, so the bytes verified are exactly the bytes stored.
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hash), dlResp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error: download failed: %v\n", err)
		os.Exit(1)
	}
	f.Close()

	gotSum := hex.EncodeToString(hash.Sum(nil))
	if gotSum != wantSum {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error: checksum mismatch for %s\n", wantName)
		fmt.Fprintf(os.Stderr, "  expected %s\n  got      %s\n", wantSum, gotSum)
		fmt.Fprintln(os.Stderr, "the download was corrupted or tampered with; nothing was installed")
		os.Exit(1)
	}

	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error: cannot replace binary: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%s → %s\n", Version, rel.TagName)
}

// fetchChecksum downloads checksums.txt and returns the expected hash for name.
// The format is sha256sum's: "<hex>  <name>" per line.
func fetchChecksum(url, name string) (string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "slot-machine/"+Version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading checksums.txt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("downloading checksums.txt: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading checksums.txt: %w", err)
	}
	return parseChecksum(string(body), name)
}

func parseChecksum(body, name string) (string, error) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum prefixes binary-mode entries with "*".
		if strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}
