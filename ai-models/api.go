package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const aaCacheTTL = time.Hour

var (
	openrouterAPI = "https://openrouter.ai/api/v1/models"
	aaFreeAPI     = "https://artificialanalysis.ai/api/v2/language/models/free"
)

// sortBy 为空时按 OpenRouter 默认顺序返回，否则追加服务端排序参数（如 most-popular）。
func fetchModels(key string, sortBy string) ([]Model, error) {
	url := openrouterAPI
	if sortBy != "" {
		url = fmt.Sprintf("%s?sort=%s", openrouterAPI, sortBy)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result ModelResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// fetchModelEndpoints 拉取指定模型（author/slug）在各 provider 详情。
func fetchModelEndpoints(key, author, slug string) (ModelEndpointsInfo, error) {
	endpointURL := fmt.Sprintf("%s/%s/%s/endpoints", openrouterAPI, url.PathEscape(author), url.PathEscape(slug))
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", endpointURL, nil)
	if err != nil {
		return ModelEndpointsInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ModelEndpointsInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ModelEndpointsInfo{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result EndpointsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ModelEndpointsInfo{}, err
	}
	return result.Data, nil
}

func fetchAAModels(aaKey string) ([]AAModel, error) {
	cachePath, err := aaCachePath()
	if err != nil {
		return nil, err
	}
	if cached, ok, err := readAACache(cachePath, time.Now()); err != nil {
		return nil, err
	} else if ok {
		return cached, nil
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var all []AAModel
	for page := 1; ; page++ {
		url := fmt.Sprintf("%s?page=%d", aaFreeAPI, page)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", aaKey)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		var result AAResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		all = append(all, result.Data...)
		if !result.Pagination.HasMore {
			break
		}
	}
	if err := writeAACache(cachePath, all); err != nil {
		return nil, err
	}
	return all, nil
}

func aaCachePath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(homeDir, ".cache", "ai-models", "aa.json"), nil
}

func readAACache(path string, now time.Time) ([]AAModel, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("stat AA cache: %w", err)
	}
	if now.Sub(info.ModTime()) < 0 || now.Sub(info.ModTime()) >= aaCacheTTL {
		return nil, false, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read AA cache: %w", err)
	}
	var cached []AAModel
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, false, nil
	}
	return cached, true, nil
}

func writeAACache(path string, models []AAModel) error {
	data, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("marshal AA cache: %w", err)
	}
	cacheDir := filepath.Dir(path)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("create AA cache directory: %w", err)
	}
	tempFile, err := os.CreateTemp(cacheDir, ".aa-cache-*")
	if err != nil {
		return fmt.Errorf("create temporary AA cache: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return fmt.Errorf("write temporary AA cache: %w", err)
	}
	if err := tempFile.Chmod(0o644); err != nil {
		tempFile.Close()
		return fmt.Errorf("set AA cache permissions: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temporary AA cache: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace AA cache: %w", err)
	}
	return nil
}
