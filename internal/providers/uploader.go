package providers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	rootPath, err := filepath.Abs(u.rootDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", err
	}
	defer root.Close()

	clean := filepath.Clean(objectPath)
	if clean == "." || filepath.IsAbs(objectPath) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("upload path escapes root: %q", objectPath)
	}
	if parent := filepath.Dir(clean); parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return "", err
		}
	}
	f, err := root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		return "", err
	}
	return "file://" + filepath.Join(rootPath, clean), nil
}
