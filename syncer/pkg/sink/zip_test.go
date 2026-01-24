package sink

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestZipSink(t *testing.T) {
	// Create a temporary file for the zip output
	tmpDir, err := os.MkdirTemp("", "zipsink-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "test.zip")

	// Create ZipSink
	sink, err := NewZipSink(zipPath)
	if err != nil {
		t.Fatalf("failed to create zip sink: %v", err)
	}

	// Write some data
	files := map[string]string{
		"file1.txt":       "content1",
		"dir/file2.yaml":  "content2",
	}

	for path, content := range files {
		if err := sink.Write(path, []byte(content)); err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}
	}

	// Close sink
	if err := sink.Close(); err != nil {
		t.Fatalf("failed to close sink: %v", err)
	}

	// Verify zip content
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("failed to open zip file: %v", err)
	}
	defer r.Close()

	if len(r.File) != len(files) {
		t.Errorf("expected %d files in zip, got %d", len(files), len(r.File))
	}

	for _, f := range r.File {
		content, ok := files[f.Name]
		if !ok {
			t.Errorf("unexpected file in zip: %s", f.Name)
			continue
		}

		rc, err := f.Open()
		if err != nil {
			t.Errorf("failed to open file %s in zip: %v", f.Name, err)
			continue
		}

		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("failed to read file %s in zip: %v", f.Name, err)
			continue
		}

		if string(got) != content {
			t.Errorf("file %s content mismatch: got %q, want %q", f.Name, got, content)
		}
	}
}
