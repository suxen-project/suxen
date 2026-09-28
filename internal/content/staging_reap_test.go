package content

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
)

func TestStagingReaperHonorsAnotherProcess(t *testing.T) {
	const helperEnv = "SUXEN_STAGING_LOCK_TEST_HELPER"
	if directory := os.Getenv(helperEnv); directory != "" {
		rt := New(Options{Config: config.Config{DataDir: directory, MaxUploadBytes: 1024}})
		staged, err := rt.StageUpload(nil, strings.NewReader("live in another process"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(staged.Path, old, old); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(staged.Path)
		_, _ = os.Stdin.Read(make([]byte, 1))
		// Simulate process loss: the request's deferred Remove never runs.
		os.Exit(0)
	}
	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestStagingReaperHonorsAnotherProcess$")
	command.Env = append(os.Environ(), helperEnv+"="+directory)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() {
		t.Fatalf("helper did not report staged file: %v", scanner.Err())
	}
	path := scanner.Text()
	rt := New(Options{Config: config.Config{DataDir: directory}})
	stale, deleted, err := rt.ReapStaleStaging(false, time.Now())
	if err != nil || stale != 0 || deleted != 0 {
		t.Fatalf("live cross-process sweep = %d, %d, %v", stale, deleted, err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	stale, deleted, err = rt.ReapStaleStaging(false, time.Now())
	if err != nil || stale != 1 || deleted != 1 {
		t.Fatalf("crashed-process sweep = %d, %d, %v", stale, deleted, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("staged file after process exit and sweep: %v", err)
	}
}

func TestStagingReaperProtectsLiveUploadAndReclaimsAbandonedFile(t *testing.T) {
	rt := New(Options{Config: config.Config{DataDir: t.TempDir(), MaxUploadBytes: 1024}})
	staged, err := rt.StageUpload(nil, strings.NewReader("staged bytes"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(staged.Path, old, old); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(staged.Path); err != nil || string(data) != "staged bytes" {
		t.Fatalf("read locked staging file = %q, %v", data, err)
	}
	stale, deleted, err := rt.ReapStaleStaging(false, time.Now())
	if err != nil || stale != 0 || deleted != 0 {
		t.Fatalf("live staging sweep = %d, %d, %v", stale, deleted, err)
	}
	rt.CloseStaging()
	stale, deleted, err = rt.ReapStaleStaging(true, time.Now())
	if err != nil || stale != 1 || deleted != 0 {
		t.Fatalf("abandoned staging preview = %d, %d, %v", stale, deleted, err)
	}
	stale, deleted, err = rt.ReapStaleStaging(false, time.Now())
	if err != nil || stale != 1 || deleted != 1 {
		t.Fatalf("abandoned staging sweep = %d, %d, %v", stale, deleted, err)
	}
	if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
		t.Fatalf("staged file after sweep: %v", err)
	}
}
