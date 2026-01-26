package providers

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
)

type Uploader interface {
	UploadBytes(ctx context.Context, objectPath string, contentType string, data []byte) (string, error)
}

type localUploader struct {
	rootDir string
}

func NewLocalUploader(rootDir string) Uploader {
	return &localUploader{rootDir: rootDir}
}

func (u *localUploader) UploadBytes(ctx context.Context, objectPath string, contentType string, data []byte) (string, error) {
	dst := filepath.Join(u.rootDir, objectPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(dst)
	return "file://" + abs, nil
}
