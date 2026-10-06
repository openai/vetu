package binaryfetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
)

func FetchURLToFile(ctx context.Context, url string) (path string, err error) {
	resp, err := FetchURL(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	tempFile, err := os.CreateTemp("", "")
	if err != nil {
		return "", err
	}

	defer func() {
		// The caller owns only a completed download. Close before removing on Windows.
		_ = tempFile.Close()
		if err != nil {
			_ = os.Remove(tempFile.Name())
		}
	}()

	if _, err := io.Copy(tempFile, resp.Body); err != nil {
		return "", err
	}

	if err := tempFile.Close(); err != nil {
		return "", err
	}
	return tempFile.Name(), nil
}

func FetchURL(ctx context.Context, url string) (*http.Response, error) {
	client := http.Client{}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("failed to fetch %s: HTTP %d", url, resp.StatusCode)
	}

	return resp, nil
}
