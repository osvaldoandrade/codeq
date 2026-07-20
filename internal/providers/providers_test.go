package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalUploaderUploadBytes(t *testing.T) {
	tmpDir := t.TempDir()

	uploader := NewLocalUploader(tmpDir)
	ctx := context.Background()

	data := []byte("test content")
	url, err := uploader.UploadBytes(ctx, "test/file.txt", "text/plain", data)

	if err != nil {
		t.Fatalf("UploadBytes failed: %v", err)
	}

	if url == "" {
		t.Fatal("Expected non-empty URL")
	}

	// Verify file was created
	filePath := filepath.Join(tmpDir, "test/file.txt")
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read uploaded file: %v", err)
	}

	if string(content) != "test content" {
		t.Errorf("Expected content 'test content', got %s", string(content))
	}
}

func TestLocalUploaderCreatesDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	uploader := NewLocalUploader(tmpDir)
	ctx := context.Background()

	data := []byte("nested file")
	_, err := uploader.UploadBytes(ctx, "deep/nested/path/file.txt", "text/plain", data)

	if err != nil {
		t.Fatalf("UploadBytes failed: %v", err)
	}

	// Verify nested directories were created
	filePath := filepath.Join(tmpDir, "deep/nested/path/file.txt")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Fatal("Expected file to exist in nested directory")
	}
}

func TestLocalUploaderRejectsEscapingPath(t *testing.T) {
	tmpDir := t.TempDir()
	uploader := NewLocalUploader(tmpDir)

	if _, err := uploader.UploadBytes(context.Background(), "../escaped.txt", "text/plain", []byte("no")); err == nil {
		t.Fatal("expected parent traversal to be rejected")
	}
	if _, err := uploader.UploadBytes(context.Background(), filepath.Join(tmpDir, "absolute.txt"), "text/plain", []byte("no")); err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
}

func TestLocalUploaderRejectsSymlinkParent(t *testing.T) {
	tmpDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tmpDir, "link")); err != nil {
		t.Fatal(err)
	}

	uploader := NewLocalUploader(tmpDir)
	if _, err := uploader.UploadBytes(context.Background(), "link/file.txt", "text/plain", []byte("no")); err == nil {
		t.Fatal("expected symlink parent to be rejected")
	}
}

func TestNewRedisProvider(t *testing.T) {
	client := NewRedisProvider("localhost:6379", "password")

	if client == nil {
		t.Fatal("Expected redis client to be non-nil")
	}

	defer client.Close()
}
